package colstore

import (
	"os"
	"path/filepath"
	"regexp"
)

// Store 管理一个本地目录下的所有表。
type Store struct {
	dir string
}

// Open 打开（必要时创建）一个存储目录。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Close 关闭存储。当前实现没有需要释放的全局资源。
func (s *Store) Close() error {
	return nil
}

var validName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func checkName(name string) error {
	if !validName.MatchString(name) {
		return ErrInvalidName
	}
	return nil
}

// validateSchema 校验一组列定义：1..500 列、类型合法、列名合法且不重复。
func validateSchema(cols []Column) error {
	if len(cols) < 1 {
		return ErrTooFewColumns
	}
	if len(cols) > MaxColumns {
		return ErrTooManyColumns
	}
	seen := make(map[string]bool, len(cols))
	for _, c := range cols {
		if !c.Type.valid() {
			return ErrTypeMismatch
		}
		if err := checkName(c.Name); err != nil {
			return err
		}
		if seen[c.Name] {
			return ErrDuplicateColumn
		}
		seen[c.Name] = true
	}
	return nil
}

func (s *Store) tableDir(name string) string {
	return filepath.Join(s.dir, name)
}

// CreateTable 建表，产生结构版本 1。
func (s *Store) CreateTable(name string, cols []Column) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := validateSchema(cols); err != nil {
		return err
	}
	dir := s.tableDir(name)
	if _, err := os.Stat(dir); err == nil {
		return ErrTableExists
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	m, err := createMeta(filepath.Join(dir, "meta.db"), cols)
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	// 为每一列创建空数据文件，列 ID 从 0 开始与元数据一致。
	for i := range cols {
		w, err := createColFile(colFilePath(dir, int64(i)), cols[i].Type)
		if err != nil {
			m.close()
			os.RemoveAll(dir)
			return err
		}
		if err := w.close(); err != nil {
			m.close()
			os.RemoveAll(dir)
			return err
		}
	}
	return m.close()
}

// Table 打开一张已存在的表。
func (s *Store) Table(name string) (*Table, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	dir := s.tableDir(name)
	if _, err := os.Stat(filepath.Join(dir, "meta.db")); err != nil {
		return nil, ErrTableNotFound
	}
	m, err := openMeta(filepath.Join(dir, "meta.db"))
	if err != nil {
		return nil, err
	}
	curVer, rowCount, nextColID, err := m.state()
	if err != nil {
		m.close()
		return nil, err
	}
	schema, err := m.schemaAt(curVer)
	if err != nil {
		m.close()
		return nil, err
	}
	segs, err := m.loadSegments()
	if err != nil {
		m.close()
		return nil, err
	}
	live, err := m.liveColIDs()
	if err != nil {
		m.close()
		return nil, err
	}
	for id := range segs {
		if !live[id] {
			delete(segs, id)
		}
	}
	t := &Table{
		dir:       dir,
		meta:      m,
		version:   curVer,
		rowCount:  rowCount,
		nextColID: nextColID,
		schema:    schema,
		byName:    make(map[string]int, len(schema)),
		writers:   make(map[int64]*colWriter),
		segments:  segs,
	}
	for i, c := range schema {
		t.byName[c.Name] = i
	}
	return t, nil
}
