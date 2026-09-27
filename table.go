package colstore

import (
	"fmt"
	"os"
	"path/filepath"
)

// Table 一张列式表的句柄。同一张表上的操作由调用方串行调用。
type Table struct {
	dir       string
	meta      *metaStore
	version   int64
	rowCount  int64
	nextColID int64
	schema    []columnRec          // 当前结构，按 Pos 排序
	byName    map[string]int       // 列名 -> schema 下标
	writers   map[int64]*colWriter // 当前结构各列的追加写句柄（惰性打开）
	segments  map[int64][]segment  // 所有存活列的行区间映射
}

func colFilePath(dir string, id int64) string {
	return filepath.Join(dir, fmt.Sprintf("c%d.dat", id))
}

// Version 当前结构版本号。
func (t *Table) Version() int {
	return int(t.version)
}

// Schema 当前结构（列按定义顺序）。
func (t *Table) Schema() []Column {
	out := make([]Column, len(t.schema))
	for i, c := range t.schema {
		out[i] = Column{Name: c.Name, Type: c.Type}
	}
	return out
}

// RowCount 当前表中的行数。
func (t *Table) RowCount() int {
	return int(t.rowCount)
}

// Versions 所有保留的结构版本号（升序）。
func (t *Table) Versions() ([]int, error) {
	vs, err := t.meta.versions()
	if err != nil {
		return nil, err
	}
	out := make([]int, len(vs))
	for i, v := range vs {
		out[i] = int(v)
	}
	return out, nil
}

// writerFor 返回某列的追加写句柄，必要时打开。
func (t *Table) writerFor(c columnRec) (*colWriter, error) {
	if w, ok := t.writers[c.ID]; ok {
		return w, nil
	}
	w, err := openColWriter(colFilePath(t.dir, c.ID), c.Type)
	if err != nil {
		return nil, err
	}
	t.writers[c.ID] = w
	return w, nil
}

// validateRow 校验一行：列必须都在当前结构里，值类型必须与列类型相符（空值不限）。
func (t *Table) validateRow(r Row) error {
	for name, v := range r {
		idx, ok := t.byName[name]
		if !ok {
			return fmt.Errorf("%w: %q", ErrUnknownColumn, name)
		}
		if v.Null {
			continue
		}
		if v.typ != t.schema[idx].Type {
			return fmt.Errorf("%w: column %q", ErrTypeMismatch, name)
		}
	}
	return nil
}

// Append 按当前结构追加一批行。任一行非法则整批拒绝，一行都不写入。
func (t *Table) Append(rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	for _, r := range rows {
		if err := t.validateRow(r); err != nil {
			return err
		}
	}
	n := int64(len(rows))
	for _, c := range t.schema {
		w, err := t.writerFor(c)
		if err != nil {
			return err
		}
		for _, r := range rows {
			v, ok := r[c.Name]
			if !ok {
				v = NullValue()
			}
			if err := w.append(v); err != nil {
				return err
			}
		}
	}
	// 更新行区间：本批行追加到每个当前列的末尾段，或开一个新段。
	for _, c := range t.schema {
		segs := t.segments[c.ID]
		if len(segs) > 0 && segs[len(segs)-1].Start+segs[len(segs)-1].Cnt == t.rowCount {
			segs[len(segs)-1].Cnt += n
		} else {
			segs = append(segs, segment{Start: t.rowCount, Cnt: n})
		}
		t.segments[c.ID] = segs
	}
	for _, w := range t.writers {
		if err := w.flush(); err != nil {
			return err
		}
	}
	t.rowCount += n
	return t.meta.setInt("row_count", t.rowCount)
}

// closeWriters 关闭所有打开的列写句柄。
func (t *Table) closeWriters() error {
	var firstErr error
	for id, w := range t.writers {
		if err := w.close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(t.writers, id)
	}
	return firstErr
}

// applySchemaChange 提交一个新结构版本：关闭写句柄、落库、垃圾回收列文件。
func (t *Table) applySchemaChange(newSchema []columnRec) error {
	if err := t.closeWriters(); err != nil {
		return err
	}
	live, err := t.meta.commitVersion(t.version+1, newSchema)
	if err != nil {
		return err
	}
	t.version++
	t.schema = newSchema
	t.byName = make(map[string]int, len(newSchema))
	for i, c := range newSchema {
		t.byName[c.Name] = i
	}
	// 不再存活的列，丢弃其行区间映射。
	for id := range t.segments {
		if !live[id] {
			delete(t.segments, id)
		}
	}
	return t.gcFiles(live)
}

// gcFiles 删除不再被任何保留版本引用的列数据文件。
func (t *Table) gcFiles(live map[int64]bool) error {
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		var id int64
		if n, _ := fmt.Sscanf(e.Name(), "c%d.dat", &id); n != 1 {
			continue
		}
		if !live[id] {
			if err := os.Remove(filepath.Join(t.dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// AddColumn 加列，产生新版本。新列对已有行读出为空。
func (t *Table) AddColumn(col Column) error {
	if err := checkName(col.Name); err != nil {
		return err
	}
	if !col.Type.valid() {
		return ErrTypeMismatch
	}
	if _, ok := t.byName[col.Name]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateColumn, col.Name)
	}
	if len(t.schema) >= MaxColumns {
		return ErrTooManyColumns
	}
	id := t.nextColID
	w, err := createColFile(colFilePath(t.dir, id), col.Type)
	if err != nil {
		return err
	}
	if err := w.close(); err != nil {
		return err
	}
	newSchema := make([]columnRec, len(t.schema)+1)
	copy(newSchema, t.schema)
	newSchema[len(t.schema)] = columnRec{ID: id, Name: col.Name, Type: col.Type, Pos: len(t.schema)}
	if err := t.applySchemaChange(newSchema); err != nil {
		return err
	}
	t.nextColID++
	return t.meta.setInt("next_col_id", t.nextColID)
}

// DropColumn 删列，产生新版本。
func (t *Table) DropColumn(name string) error {
	idx, ok := t.byName[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrColumnNotFound, name)
	}
	if len(t.schema) <= 1 {
		return ErrTooFewColumns
	}
	newSchema := make([]columnRec, 0, len(t.schema)-1)
	for i, c := range t.schema {
		if i == idx {
			continue
		}
		c.Pos = len(newSchema)
		newSchema = append(newSchema, c)
	}
	return t.applySchemaChange(newSchema)
}

// Rollback 回滚到历史版本 v（v 必须在保留的版本内且非当前版本），产生新版本。
func (t *Table) Rollback(v int) error {
	target := int64(v)
	if target == t.version {
		return fmt.Errorf("%w: %d is the current version", ErrVersionNotFound, v)
	}
	schema, err := t.meta.schemaAt(target)
	if err != nil {
		return err
	}
	if len(schema) == 0 {
		return fmt.Errorf("%w: %d", ErrVersionNotFound, v)
	}
	return t.applySchemaChange(schema)
}

// readColumn 读出一个列文件并按行区间对齐到全表行号，区间外的行为空。
func (t *Table) readColumn(c columnRec) ([]Value, error) {
	recs, err := readColFile(colFilePath(t.dir, c.ID), c.Type)
	if err != nil {
		return nil, err
	}
	out := make([]Value, t.rowCount)
	for i := range out {
		out[i] = NullValue()
	}
	off := int64(0)
	for _, s := range t.segments[c.ID] {
		if off+s.Cnt > int64(len(recs)) || s.Start+s.Cnt > t.rowCount {
			return nil, fmt.Errorf("colstore: segment table inconsistent for column %q", c.Name)
		}
		copy(out[s.Start:s.Start+s.Cnt], recs[off:off+s.Cnt])
		off += s.Cnt
	}
	if off != int64(len(recs)) {
		return nil, fmt.Errorf("colstore: column %q file has %d records, segments cover %d",
			c.Name, len(recs), off)
	}
	return out, nil
}

// ReadColumns 按当前结构读出指定的若干列，只打开被请求的列文件。
// 返回与 cols 一一对应的列数据，每列内按写入顺序排列。
// 这是面向数仓批量扫描的读接口；逐行访问请用 Read。
func (t *Table) ReadColumns(cols []string) ([][]Value, error) {
	if err := t.closeWriters(); err != nil {
		return nil, err
	}
	// 解析请求的列，去重并保持请求顺序。
	names := make([]string, 0, len(cols))
	recs := make([]columnRec, 0, len(cols))
	seen := make(map[string]bool, len(cols))
	for _, name := range cols {
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
		idx, ok := t.byName[name]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrColumnNotFound, name)
		}
		recs = append(recs, t.schema[idx])
	}
	out := make([][]Value, len(recs))
	for i, rec := range recs {
		vals, err := t.readColumn(rec)
		if err != nil {
			return nil, err
		}
		out[i] = vals
	}
	return out, nil
}

// Read 按当前结构读出指定的若干列，行按写入顺序排列。
// 只打开被请求的列文件。
func (t *Table) Read(cols []string) ([]Row, error) {
	colVals, err := t.ReadColumns(cols)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cols))
	seen := make(map[string]bool, len(cols))
	for _, name := range cols {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	rows := make([]Row, t.rowCount)
	for i := range rows {
		rows[i] = make(Row, len(names))
	}
	for j, name := range names {
		for i, v := range colVals[j] {
			rows[i][name] = v
		}
	}
	return rows, nil
}

// Close 关闭表句柄。
func (t *Table) Close() error {
	if t.meta == nil {
		return nil
	}
	err := t.closeWriters()
	if serr := t.meta.saveSegments(t.segments); serr != nil && err == nil {
		err = serr
	}
	m := t.meta
	t.meta = nil
	if cerr := m.close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}
