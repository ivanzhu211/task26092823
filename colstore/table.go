package colstore

import (
	"fmt"
	"os"
	"path/filepath"
)

// Table 是一张打开的列式表。同一时刻同一张表只允许一个 Table 实例，
// 调用方需串行调用其方法。
type Table struct {
	dir     string
	m       *meta
	schema  []colRef // 当前结构（按列顺序）
	version int64
	rows    int64
	writers map[int64]*colWriter // 追加用，按需打开，文件只增不删故可缓存
	closed  bool
}

// newTable 由已打开的元数据构造 Table（加载当前状态）。
func newTable(dir string, m *meta) (*Table, error) {
	ver, err := m.getKV("current_version")
	if err != nil {
		return nil, err
	}
	rows, err := m.getKV("row_count")
	if err != nil {
		return nil, err
	}
	schema, err := m.schemaAt(ver)
	if err != nil {
		return nil, err
	}
	return &Table{dir: dir, m: m, schema: schema, version: ver, rows: rows, writers: map[int64]*colWriter{}}, nil
}

func (t *Table) colPath(colID int64) string {
	return filepath.Join(t.dir, "cols", fmt.Sprintf("%d.col", colID))
}

// Schema 返回当前结构的列（按顺序）。
func (t *Table) Schema() []Column {
	out := make([]Column, len(t.schema))
	for i, c := range t.schema {
		out[i] = Column{Name: c.name, Type: c.typ}
	}
	return out
}

// Version 返回当前结构版本号。
func (t *Table) Version() int { return int(t.version) }

// Versions 返回当前保留的所有版本号（含当前版本，升序）。
func (t *Table) Versions() []int {
	vs, _ := t.m.retainedVersions()
	out := make([]int, len(vs))
	for i, v := range vs {
		out[i] = int(v)
	}
	return out
}

// RowCount 返回表的总行数。
func (t *Table) RowCount() int64 { return t.rows }

func (t *Table) colByName(name string) (colRef, bool) {
	for _, c := range t.schema {
		if c.name == name {
			return c, true
		}
	}
	return colRef{}, false
}

// Append 按当前结构追加一批行。任一行非法则整批拒绝，一行都不写入。
func (t *Table) Append(rows []map[string]Value) error {
	if t.closed {
		return ErrClosed
	}
	if len(rows) == 0 {
		return nil
	}
	// 先整批校验，再写任何数据
	for _, row := range rows {
		for name, v := range row {
			c, ok := t.colByName(name)
			if !ok {
				return fmt.Errorf("%w: %q", ErrUnknownColumn, name)
			}
			if !typeMatches(c.typ, v) {
				return fmt.Errorf("%w: column %q", ErrTypeMismatch, name)
			}
		}
	}
	start := t.rows
	n := int64(len(rows))
	colIDs := make([]int64, len(t.schema))
	for i, c := range t.schema {
		colIDs[i] = c.colID
		w, err := t.writerFor(c.colID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			v, ok := row[c.name]
			if !ok {
				v = Null()
			}
			if err := w.write(v); err != nil {
				return err
			}
		}
		if err := w.flush(); err != nil {
			return err
		}
	}
	if err := t.m.recordAppend(colIDs, start, n); err != nil {
		return err
	}
	t.rows = start + n
	return nil
}

func typeMatches(typ Kind, v Value) bool {
	if v.Kind == KindNull {
		return true
	}
	return v.Kind == typ
}

func (t *Table) writerFor(colID int64) (*colWriter, error) {
	if w, ok := t.writers[colID]; ok {
		return w, nil
	}
	existing, err := t.m.fileRowCount(colID)
	if err != nil {
		return nil, err
	}
	w, err := openColWriter(t.colPath(colID), existing)
	if err != nil {
		return nil, err
	}
	t.writers[colID] = w
	return w, nil
}

// commitSchema 提交一个新结构版本并做垃圾回收（删除不再被任何
// 保留版本引用的列数据文件）。
func (t *Table) commitSchema(cols []colRef) error {
	newVer, garbage, err := t.m.commitVersion(cols)
	if err != nil {
		return err
	}
	for _, id := range garbage {
		if w, ok := t.writers[id]; ok {
			w.close()
			delete(t.writers, id)
		}
		os.Remove(t.colPath(id))
	}
	t.schema = cols
	t.version = newVer
	return nil
}

// AddColumn 加列，产生新版本。新增列对历史行读出为空。
func (t *Table) AddColumn(col Column) error {
	if t.closed {
		return ErrClosed
	}
	if len(t.schema) >= MaxColumns {
		return ErrTooManyColumns
	}
	if col.Name == "" {
		return ErrNoSuchColumn
	}
	if col.Type != KindInt && col.Type != KindFloat && col.Type != KindString {
		return ErrTypeMismatch
	}
	if _, ok := t.colByName(col.Name); ok {
		return fmt.Errorf("%w: %q", ErrDupColumn, col.Name)
	}
	id, err := t.m.registerColumn(col.Name, col.Type)
	if err != nil {
		return err
	}
	cols := append(append([]colRef{}, t.schema...), colRef{colID: id, name: col.Name, typ: col.Type})
	return t.commitSchema(cols)
}

// DropColumn 删列，产生新版本。
func (t *Table) DropColumn(name string) error {
	if t.closed {
		return ErrClosed
	}
	if _, ok := t.colByName(name); !ok {
		return fmt.Errorf("%w: %q", ErrNoSuchColumn, name)
	}
	if len(t.schema) == 1 {
		return ErrTooFewColumns
	}
	cols := make([]colRef, 0, len(t.schema)-1)
	for _, c := range t.schema {
		if c.name != name {
			cols = append(cols, c)
		}
	}
	return t.commitSchema(cols)
}

// Rollback 回滚到保留的非当前历史版本，产生新版本。
// 新版本结构与目标版本完全相同（同样的列身份），数据原样可见。
func (t *Table) Rollback(version int) error {
	if t.closed {
		return ErrClosed
	}
	if int64(version) == t.version {
		return ErrVersionInvalid
	}
	retained, err := t.m.retainedVersions()
	if err != nil {
		return err
	}
	ok := false
	for _, v := range retained {
		if v == int64(version) {
			ok = true
			break
		}
	}
	if !ok {
		return ErrVersionInvalid
	}
	cols, err := t.m.schemaAt(int64(version))
	if err != nil {
		return err
	}
	return t.commitSchema(cols)
}

// Rows 是 Read 的结果：列名与按写入顺序排列的行。
type Rows struct {
	Cols   []string
	Values [][]Value // Values[i][j] 为第 i 行第 j 列
}

// Read 按当前结构读出指定的若干列，行按写入顺序排列。
// 只打开被请求列的数据文件。
func (t *Table) Read(cols []string) (*Rows, error) {
	if t.closed {
		return nil, ErrClosed
	}
	refs := make([]colRef, len(cols))
	for i, name := range cols {
		c, ok := t.colByName(name)
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrNoSuchColumn, name)
		}
		refs[i] = c
	}
	n := t.rows
	colVals := make([][]Value, len(refs))
	for j, ref := range refs {
		vals, err := t.readColumn(ref.colID, n)
		if err != nil {
			return nil, err
		}
		colVals[j] = vals
	}
	out := &Rows{Cols: append([]string{}, cols...), Values: make([][]Value, n)}
	for i := int64(0); i < n; i++ {
		row := make([]Value, len(refs))
		for j := range refs {
			row[j] = colVals[j][i]
		}
		out.Values[i] = row
	}
	return out, nil
}

// readColumn 读出单列全部行：不在该列任何段中的行为空值。
func (t *Table) readColumn(colID int64, n int64) ([]Value, error) {
	// 若该列有未关闭的追加写入句柄，先刷盘保证读到最新数据
	if w, ok := t.writers[colID]; ok {
		if err := w.flush(); err != nil {
			return nil, err
		}
	}
	segs, err := t.m.segmentsFor(colID)
	if err != nil {
		return nil, err
	}
	vals := make([]Value, n) // 零值即空值
	if len(segs) == 0 {
		return vals, nil
	}
	r, err := openColReader(t.colPath(colID))
	if err != nil {
		return nil, err
	}
	defer r.close()
	for _, s := range segs {
		for k := int64(0); k < s.Count; k++ {
			v, err := r.read()
			if err != nil {
				return nil, err
			}
			vals[s.StartRow+k] = v
		}
	}
	return vals, nil
}

// Close 关闭表（刷盘并释放全部文件句柄与元数据连接）。
func (t *Table) Close() error {
	if t.closed {
		return nil
	}
	t.closed = true
	var firstErr error
	for id, w := range t.writers {
		if err := w.close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(t.writers, id)
	}
	if err := t.m.close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
