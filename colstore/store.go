package colstore

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

// Store 管理一个根目录下的所有表。每张表是根目录下的一个子目录：
//
//	<root>/<table>/meta.db      SQLite 元数据（结构版本、列身份、行段映射）
//	<root>/<table>/cols/<id>.col  每列一个数据文件，按列 ID 命名
type Store struct {
	dir string
}

var tableNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// OpenStore 打开（必要时创建）根目录。
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Close 关闭 Store。已打开的 Table 需各自关闭。
func (s *Store) Close() error { return nil }

func validateSchema(schema []Column) error {
	if len(schema) < 1 {
		return ErrTooFewColumns
	}
	if len(schema) > MaxColumns {
		return ErrTooManyColumns
	}
	seen := make(map[string]bool, len(schema))
	for _, c := range schema {
		if c.Name == "" {
			return ErrNoSuchColumn
		}
		if c.Type != KindInt && c.Type != KindFloat && c.Type != KindString {
			return ErrTypeMismatch
		}
		if seen[c.Name] {
			return ErrDupColumn
		}
		seen[c.Name] = true
	}
	return nil
}

// CreateTable 建表，产生版本 1。
func (s *Store) CreateTable(name string, schema []Column) (*Table, error) {
	if !tableNameRE.MatchString(name) {
		return nil, ErrTableNotFound
	}
	if err := validateSchema(schema); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.dir, name)
	if _, err := os.Stat(dir); err == nil {
		return nil, ErrTableExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "cols"), 0o755); err != nil {
		return nil, err
	}
	m, err := openMeta(filepath.Join(dir, "meta.db"))
	if err != nil {
		return nil, err
	}
	if err := m.initSchema(schema); err != nil {
		m.close()
		return nil, err
	}
	return newTable(dir, m)
}

// OpenTable 打开已存在的表。
func (s *Store) OpenTable(name string) (*Table, error) {
	dir := filepath.Join(s.dir, name)
	if _, err := os.Stat(filepath.Join(dir, "meta.db")); err != nil {
		return nil, ErrTableNotFound
	}
	m, err := openMeta(filepath.Join(dir, "meta.db"))
	if err != nil {
		return nil, err
	}
	return newTable(dir, m)
}
