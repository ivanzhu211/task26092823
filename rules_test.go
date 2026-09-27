package colstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreate(t *testing.T, s *Store, name string, cols []Column) {
	t.Helper()
	if err := s.CreateTable(name, cols); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
}

func mustTable(t *testing.T, s *Store, name string) *Table {
	t.Helper()
	tb, err := s.Table(name)
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	t.Cleanup(func() { tb.Close() })
	return tb
}

func TestCreateTableValidation(t *testing.T) {
	s := openStore(t)

	if err := s.CreateTable("t0", nil); !errors.Is(err, ErrTooFewColumns) {
		t.Fatalf("0 columns: %v", err)
	}
	many := make([]Column, MaxColumns+1)
	for i := range many {
		many[i] = Column{Name: string(rune('a'+i%26)) + string(rune('A'+i/26)) + string(rune('0'+i%10)), Type: TypeInt}
	}
	seen := map[string]bool{}
	for i := range many {
		for seen[many[i].Name] {
			many[i].Name += "x"
		}
		seen[many[i].Name] = true
	}
	if err := s.CreateTable("t501", many); !errors.Is(err, ErrTooManyColumns) {
		t.Fatalf("501 columns: %v", err)
	}
	if err := s.CreateTable("tdup", []Column{{"a", TypeInt}, {"a", TypeInt}}); !errors.Is(err, ErrDuplicateColumn) {
		t.Fatalf("dup columns: %v", err)
	}
	if err := s.CreateTable("tbadtype", []Column{{"a", ColumnType(0)}}); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("bad type: %v", err)
	}
	if err := s.CreateTable("bad name!", []Column{{"a", TypeInt}}); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("bad table name: %v", err)
	}
	if err := s.CreateTable("tbadcol", []Column{{"1col", TypeInt}}); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("bad column name: %v", err)
	}

	mustCreate(t, s, "t1", []Column{{"a", TypeInt}})
	if err := s.CreateTable("t1", []Column{{"b", TypeInt}}); !errors.Is(err, ErrTableExists) {
		t.Fatalf("create existing: %v", err)
	}
	if _, err := s.Table("nosuch"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("open missing: %v", err)
	}
}

func TestAppendValidation(t *testing.T) {
	s := openStore(t)
	mustCreate(t, s, "t", []Column{{"id", TypeInt}, {"v", TypeFloat}})
	tb := mustTable(t, s, "t")

	if err := tb.Append([]Row{{"id": IntValue(1)}, {"ghost": IntValue(2)}}); !errors.Is(err, ErrUnknownColumn) {
		t.Fatalf("unknown column: %v", err)
	}
	if got := tb.RowCount(); got != 0 {
		t.Fatalf("row count = %d after rejected batch", got)
	}
	if err := tb.Append([]Row{{"id": FloatValue(1.5)}}); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("type mismatch: %v", err)
	}
	if got := tb.RowCount(); got != 0 {
		t.Fatalf("row count = %d after rejected batch", got)
	}
	// 批次里第二行非法：整批拒绝，一行都不写入。
	err := tb.Append([]Row{{"id": IntValue(1)}, {"id": StringValue("x")}})
	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("mixed batch: %v", err)
	}
	if got := tb.RowCount(); got != 0 {
		t.Fatalf("row count = %d, partial batch must not be written", got)
	}
	// 空值可以写给任何类型的列。
	if err := tb.Append([]Row{{"id": NullValue(), "v": NullValue()}}); err != nil {
		t.Fatalf("null values: %v", err)
	}
	if got := tb.RowCount(); got != 1 {
		t.Fatalf("row count = %d, want 1", got)
	}
	rows, err := tb.Read([]string{"id", "v"})
	if err != nil {
		t.Fatal(err)
	}
	if !rows[0]["id"].Null || !rows[0]["v"].Null {
		t.Fatalf("expected nulls, got %+v", rows[0])
	}
}

func TestEmptyStringVsNull(t *testing.T) {
	s := openStore(t)
	mustCreate(t, s, "t", []Column{{"s", TypeString}})
	tb := mustTable(t, s, "t")
	if err := tb.Append([]Row{{"s": StringValue("")}, {}}); err != nil {
		t.Fatal(err)
	}
	rows, err := tb.Read([]string{"s"})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0]["s"].Null || rows[0]["s"].Str != "" {
		t.Fatalf("empty string must not be null: %+v", rows[0]["s"])
	}
	if !rows[1]["s"].Null {
		t.Fatalf("missing value must be null: %+v", rows[1]["s"])
	}
}

func TestVersionNumbering(t *testing.T) {
	s := openStore(t)
	mustCreate(t, s, "t", []Column{{"a", TypeInt}})
	tb := mustTable(t, s, "t")
	if tb.Version() != 1 {
		t.Fatalf("version = %d, want 1", tb.Version())
	}
	if err := tb.AddColumn(Column{"b", TypeString}); err != nil {
		t.Fatal(err)
	}
	if tb.Version() != 2 {
		t.Fatalf("version = %d, want 2", tb.Version())
	}
	if err := tb.DropColumn("b"); err != nil {
		t.Fatal(err)
	}
	if tb.Version() != 3 {
		t.Fatalf("version = %d, want 3", tb.Version())
	}
	if err := tb.Rollback(2); err != nil {
		t.Fatal(err)
	}
	if tb.Version() != 4 {
		t.Fatalf("version = %d, want 4", tb.Version())
	}
	// 追加不产生新版本。
	if err := tb.Append([]Row{{"a": IntValue(1)}}); err != nil {
		t.Fatal(err)
	}
	if tb.Version() != 4 {
		t.Fatalf("append must not bump version, got %d", tb.Version())
	}
}

func TestKeepTwentyVersions(t *testing.T) {
	s := openStore(t)
	mustCreate(t, s, "t", []Column{{"a", TypeInt}})
	tb := mustTable(t, s, "t")
	// 25 次结构变更 -> 版本 2..26，只保留最近 20 个：7..26。
	for i := 0; i < 25; i++ {
		name := "c" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if err := tb.AddColumn(Column{name, TypeInt}); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := tb.Versions()
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != KeepVersions || vs[0] != 7 || vs[len(vs)-1] != 26 {
		t.Fatalf("versions = %v", vs)
	}
	if err := tb.Rollback(6); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("rollback to pruned version: %v", err)
	}
	if err := tb.Rollback(26); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("rollback to current version: %v", err)
	}
	if err := tb.Rollback(7); err != nil {
		t.Fatalf("rollback to oldest retained version: %v", err)
	}
	if tb.Version() != 27 {
		t.Fatalf("version = %d, want 27", tb.Version())
	}
}

func TestDropColumnRules(t *testing.T) {
	s := openStore(t)
	mustCreate(t, s, "t", []Column{{"a", TypeInt}, {"b", TypeString}})
	tb := mustTable(t, s, "t")
	if err := tb.DropColumn("ghost"); !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("drop missing: %v", err)
	}
	if err := tb.AddColumn(Column{"a", TypeFloat}); !errors.Is(err, ErrDuplicateColumn) {
		t.Fatalf("add existing: %v", err)
	}
	if err := tb.DropColumn("b"); err != nil {
		t.Fatal(err)
	}
	// 只剩一列时不允许再删。
	if err := tb.DropColumn("a"); !errors.Is(err, ErrTooFewColumns) {
		t.Fatalf("drop last column: %v", err)
	}
	// 超过 500 列后不能再加。
	s2 := openStore(t)
	cols := make([]Column, MaxColumns)
	for i := range cols {
		cols[i] = Column{Name: "c" + itoa(i), Type: TypeInt}
	}
	mustCreate(t, s2, "big", cols)
	tb2 := mustTable(t, s2, "big")
	if err := tb2.AddColumn(Column{"overflow", TypeInt}); !errors.Is(err, ErrTooManyColumns) {
		t.Fatalf("add 501st column: %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// 读指定列时不打开其他列的文件：删掉未请求列的文件，读其余列不受影响。
func TestReadDoesNotTouchOtherColumnFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mustCreate(t, s, "t", []Column{{"a", TypeInt}, {"b", TypeInt}})
	tb, err := s.Table("t")
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.Append([]Row{{"a": IntValue(7), "b": IntValue(8)}}); err != nil {
		t.Fatal(err)
	}
	if err := tb.Close(); err != nil {
		t.Fatal(err)
	}
	// 找到 b 列的文件并删掉。
	entries, err := os.ReadDir(filepath.Join(dir, "t"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := openMeta(filepath.Join(dir, "t", "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	cur, _, _, err := m.state()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := m.schemaAt(cur)
	if err != nil {
		t.Fatal(err)
	}
	var bFile string
	for _, c := range schema {
		if c.Name == "b" {
			bFile = colFilePath(filepath.Join(dir, "t"), c.ID)
		}
	}
	if bFile == "" {
		t.Fatal("column b not found")
	}
	if err := os.Remove(bFile); err != nil {
		t.Fatal(err)
	}
	_ = entries
	tb2, err := s.Table("t")
	if err != nil {
		t.Fatal(err)
	}
	defer tb2.Close()
	rows, err := tb2.Read([]string{"a"})
	if err != nil {
		t.Fatalf("read a without b's file: %v", err)
	}
	if rows[0]["a"].Int != 7 {
		t.Fatalf("got %+v", rows[0])
	}
	if _, err := tb2.Read([]string{"b"}); err == nil {
		t.Fatal("read b should fail without its file")
	}
}

func TestReadUnknownColumn(t *testing.T) {
	s := openStore(t)
	mustCreate(t, s, "t", []Column{{"a", TypeInt}})
	tb := mustTable(t, s, "t")
	if _, err := tb.Read([]string{"ghost"}); !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("read unknown column: %v", err)
	}
}
