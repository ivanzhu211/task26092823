package colstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustCreate(t *testing.T, s *Store, name string, schema []Column) *Table {
	t.Helper()
	tb, err := s.CreateTable(name, schema)
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func mustAppend(t *testing.T, tb *Table, rows ...map[string]Value) {
	t.Helper()
	if err := tb.Append(rows); err != nil {
		t.Fatal(err)
	}
}

// readAll 读出全部行，返回 map 形式便于断言。
func readAll(t *testing.T, tb *Table, cols ...string) []map[string]Value {
	t.Helper()
	r, err := tb.Read(cols)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]Value, len(r.Values))
	for i, row := range r.Values {
		m := map[string]Value{}
		for j, c := range r.Cols {
			m[c] = row[j]
		}
		out[i] = m
	}
	return out
}

func TestCreateTableValidation(t *testing.T) {
	s := newStore(t)
	if _, err := s.CreateTable("t0", nil); !errors.Is(err, ErrTooFewColumns) {
		t.Fatalf("empty schema: %v", err)
	}
	big := make([]Column, MaxColumns+1)
	for i := range big {
		big[i] = Column{Name: fmt.Sprintf("c%d", i), Type: KindInt}
	}
	if _, err := s.CreateTable("t1", big); !errors.Is(err, ErrTooManyColumns) {
		t.Fatalf("501 columns: %v", err)
	}
	if _, err := s.CreateTable("t2", []Column{{"a", KindInt}, {"a", KindInt}}); !errors.Is(err, ErrDupColumn) {
		t.Fatalf("dup column: %v", err)
	}
	tb := mustCreate(t, s, "t3", []Column{{"a", KindInt}})
	if tb.Version() != 1 {
		t.Fatalf("version = %d, want 1", tb.Version())
	}
	if _, err := s.CreateTable("t3", []Column{{"a", KindInt}}); !errors.Is(err, ErrTableExists) {
		t.Fatalf("recreate: %v", err)
	}
	if _, err := s.OpenTable("nope"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("open missing: %v", err)
	}
	// 恰好 500 列可以建
	ok := make([]Column, MaxColumns)
	for i := range ok {
		ok[i] = Column{Name: fmt.Sprintf("c%d", i), Type: KindInt}
	}
	if _, err := s.CreateTable("t4", ok); err != nil {
		t.Fatalf("500 columns: %v", err)
	}
}

func TestAppendAndReadBasic(t *testing.T) {
	s := newStore(t)
	tb := mustCreate(t, s, "t", []Column{{"i", KindInt}, {"f", KindFloat}, {"s", KindString}})
	mustAppend(t, tb,
		map[string]Value{"i": Int(1), "f": Float(1.5), "s": String("a")},
		map[string]Value{"i": Int(2)},                           // 未给的列为空
		map[string]Value{"s": String("")},                       // 空串不是空值
		map[string]Value{"i": Null(), "f": Null(), "s": Null()}, // 显式空
	)
	if tb.RowCount() != 4 {
		t.Fatalf("rows = %d", tb.RowCount())
	}
	got := readAll(t, tb, "i", "f", "s")
	want := []map[string]Value{
		{"i": Int(1), "f": Float(1.5), "s": String("a")},
		{"i": Int(2), "f": Null(), "s": Null()},
		{"i": Null(), "f": Null(), "s": String("")},
		{"i": Null(), "f": Null(), "s": Null()},
	}
	for i := range want {
		if got[i]["i"] != want[i]["i"] || got[i]["f"] != want[i]["f"] || got[i]["s"] != want[i]["s"] {
			t.Fatalf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// 只读部分列
	r, err := tb.Read([]string{"s"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Cols) != 1 || r.Cols[0] != "s" || r.Values[0][0] != String("a") {
		t.Fatalf("partial read: %+v", r)
	}
	// 读不存在的列
	if _, err := tb.Read([]string{"nope"}); !errors.Is(err, ErrNoSuchColumn) {
		t.Fatalf("read missing col: %v", err)
	}
}

func TestAppendRejectsBatchAtomically(t *testing.T) {
	s := newStore(t)
	tb := mustCreate(t, s, "t", []Column{{"i", KindInt}})
	// 未知列：整批拒绝
	err := tb.Append([]map[string]Value{{"i": Int(1)}, {"nope": Int(2)}})
	if !errors.Is(err, ErrUnknownColumn) {
		t.Fatalf("unknown col: %v", err)
	}
	if tb.RowCount() != 0 {
		t.Fatalf("rows after reject = %d", tb.RowCount())
	}
	// 类型不符：整批拒绝（第二行非法，第一行也不能写入）
	err = tb.Append([]map[string]Value{{"i": Int(1)}, {"i": String("x")}})
	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("type mismatch: %v", err)
	}
	if tb.RowCount() != 0 {
		t.Fatalf("rows after reject = %d", tb.RowCount())
	}
	// float 列不接受 int 值
	tb2 := mustCreate(t, s, "t2", []Column{{"f", KindFloat}})
	if err := tb2.Append([]map[string]Value{{"f": Int(1)}}); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("int into float col: %v", err)
	}
	// 拒绝后再正常追加不受影响
	mustAppend(t, tb, map[string]Value{"i": Int(9)})
	got := readAll(t, tb, "i")
	if len(got) != 1 || got[0]["i"] != Int(9) {
		t.Fatalf("got %+v", got)
	}
}

func TestAddColumnBackfillsNull(t *testing.T) {
	s := newStore(t)
	tb := mustCreate(t, s, "t", []Column{{"a", KindInt}})
	mustAppend(t, tb, map[string]Value{"a": Int(1)}, map[string]Value{"a": Int(2)})
	if err := tb.AddColumn(Column{"b", KindString}); err != nil {
		t.Fatal(err)
	}
	if tb.Version() != 2 {
		t.Fatalf("version = %d", tb.Version())
	}
	mustAppend(t, tb, map[string]Value{"a": Int(3), "b": String("x")})
	got := readAll(t, tb, "a", "b")
	want := []map[string]Value{
		{"a": Int(1), "b": Null()},
		{"a": Int(2), "b": Null()},
		{"a": Int(3), "b": String("x")},
	}
	for i := range want {
		if got[i]["a"] != want[i]["a"] || got[i]["b"] != want[i]["b"] {
			t.Fatalf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// 重名加列被拒绝
	if err := tb.AddColumn(Column{"b", KindInt}); !errors.Is(err, ErrDupColumn) {
		t.Fatalf("dup add: %v", err)
	}
}

func TestDropAndReAddSameNameIsNewColumn(t *testing.T) {
	s := newStore(t)
	tb := mustCreate(t, s, "t", []Column{{"a", KindInt}, {"b", KindString}})
	mustAppend(t, tb, map[string]Value{"a": Int(1), "b": String("old")})
	if err := tb.DropColumn("b"); err != nil {
		t.Fatal(err)
	}
	if tb.Version() != 2 {
		t.Fatalf("version = %d", tb.Version())
	}
	if _, err := tb.Read([]string{"b"}); !errors.Is(err, ErrNoSuchColumn) {
		t.Fatalf("read dropped col: %v", err)
	}
	// 同名再加（同类型）：是全新的列，读不到旧数据
	if err := tb.AddColumn(Column{"b", KindString}); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, tb, map[string]Value{"a": Int(2), "b": String("new")})
	got := readAll(t, tb, "a", "b")
	if got[0]["b"] != Null() {
		t.Fatalf("old row sees old data: %+v", got[0])
	}
	if got[1]["b"] != String("new") {
		t.Fatalf("new row: %+v", got[1])
	}
	// 删最后一列被拒绝
	single := mustCreate(t, s, "single", []Column{{"x", KindInt}})
	if err := single.DropColumn("x"); !errors.Is(err, ErrTooFewColumns) {
		t.Fatalf("drop last col: %v", err)
	}
	if err := single.DropColumn("nope"); !errors.Is(err, ErrNoSuchColumn) {
		t.Fatalf("drop missing: %v", err)
	}
}

func TestRollbackRestoresSchemaAndData(t *testing.T) {
	s := newStore(t)
	tb := mustCreate(t, s, "t", []Column{{"a", KindInt}}) // v1
	mustAppend(t, tb, map[string]Value{"a": Int(1)})
	tb.AddColumn(Column{"b", KindString}) // v2
	mustAppend(t, tb, map[string]Value{"a": Int(2), "b": String("x")})
	tb.DropColumn("b") // v3
	mustAppend(t, tb, map[string]Value{"a": Int(3)})
	// 回滚到 v2：b 列回来，且历史数据原样可见
	if err := tb.Rollback(2); err != nil {
		t.Fatal(err)
	}
	if tb.Version() != 4 {
		t.Fatalf("version = %d", tb.Version())
	}
	sch := tb.Schema()
	if len(sch) != 2 || sch[0] != (Column{"a", KindInt}) || sch[1] != (Column{"b", KindString}) {
		t.Fatalf("schema = %+v", sch)
	}
	got := readAll(t, tb, "a", "b")
	want := []map[string]Value{
		{"a": Int(1), "b": Null()}, // b 写入时不在结构里
		{"a": Int(2), "b": String("x")},
		{"a": Int(3), "b": Null()}, // 该行写入时 b 已被删
	}
	for i := range want {
		if got[i]["a"] != want[i]["a"] || got[i]["b"] != want[i]["b"] {
			t.Fatalf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// 回滚后继续追加，数据接得上
	mustAppend(t, tb, map[string]Value{"a": Int(4), "b": String("y")})
	got = readAll(t, tb, "b")
	if got[3]["b"] != String("y") {
		t.Fatalf("row 3 = %+v", got[3])
	}
	// 再回滚到 v1：b 从结构消失
	if err := tb.Rollback(1); err != nil {
		t.Fatal(err)
	}
	if _, err := tb.Read([]string{"b"}); !errors.Is(err, ErrNoSuchColumn) {
		t.Fatalf("read b after rollback to v1: %v", err)
	}
	// 再回滚到包含 b 的版本（v4），b 的数据原样回来
	if err := tb.Rollback(4); err != nil {
		t.Fatal(err)
	}
	got = readAll(t, tb, "b")
	wantB := []Value{Null(), String("x"), Null(), String("y")}
	for i, w := range wantB {
		if got[i]["b"] != w {
			t.Fatalf("row %d b = %+v, want %+v", i, got[i]["b"], w)
		}
	}
}

func TestRollbackValidation(t *testing.T) {
	s := newStore(t)
	tb := mustCreate(t, s, "t", []Column{{"a", KindInt}})
	tb.AddColumn(Column{"b", KindInt}) // v2
	if err := tb.Rollback(2); !errors.Is(err, ErrVersionInvalid) {
		t.Fatalf("rollback to current: %v", err)
	}
	if err := tb.Rollback(99); !errors.Is(err, ErrVersionInvalid) {
		t.Fatalf("rollback to unknown: %v", err)
	}
}

func TestVersionRetentionAndGC(t *testing.T) {
	s := newStore(t)
	dir := filepath.Join(s.dir, "t")
	tb := mustCreate(t, s, "t", []Column{{"a", KindInt}}) // v1, col a id=0
	mustAppend(t, tb, map[string]Value{"a": Int(1)})
	// 制造超过 20 个版本：交替加列/删列
	for i := 0; i < 25; i++ {
		if err := tb.AddColumn(Column{fmt.Sprintf("tmp%d", i), KindInt}); err != nil {
			t.Fatal(err)
		}
		if err := tb.DropColumn(fmt.Sprintf("tmp%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	vs := tb.Versions()
	if len(vs) != KeepVersions {
		t.Fatalf("retained = %d, want %d", len(vs), KeepVersions)
	}
	cur := tb.Version()
	if vs[len(vs)-1] != cur || vs[0] != cur-KeepVersions+1 {
		t.Fatalf("versions = %v, cur = %d", vs, cur)
	}
	// 回滚到已裁剪的版本失败
	if err := tb.Rollback(vs[0] - 1); !errors.Is(err, ErrVersionInvalid) {
		t.Fatalf("rollback to pruned: %v", err)
	}
	// 磁盘上只剩下当前结构列的文件（col id 0），其余全部删除
	entries, err := os.ReadDir(filepath.Join(dir, "cols"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "0.col" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("leftover col files: %v", names)
	}
	// 数据仍然正确
	got := readAll(t, tb, "a")
	if len(got) != 1 || got[0]["a"] != Int(1) {
		t.Fatalf("got %+v", got)
	}
}

func TestCloseReopenPersists(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(dir)
	tb := mustCreate(t, s, "t", []Column{{"a", KindInt}, {"b", KindString}})
	mustAppend(t, tb, map[string]Value{"a": Int(1), "b": String("x")})
	tb.AddColumn(Column{"c", KindFloat})
	mustAppend(t, tb, map[string]Value{"a": Int(2), "c": Float(2.5)})
	if err := tb.Close(); err != nil {
		t.Fatal(err)
	}
	// 关闭后操作报错
	if err := tb.Append(nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("append after close: %v", err)
	}
	s2, _ := OpenStore(dir)
	tb2, err := s2.OpenTable("t")
	if err != nil {
		t.Fatal(err)
	}
	if tb2.Version() != 2 || tb2.RowCount() != 2 {
		t.Fatalf("version=%d rows=%d", tb2.Version(), tb2.RowCount())
	}
	got := readAll(t, tb2, "a", "b", "c")
	want := []map[string]Value{
		{"a": Int(1), "b": String("x"), "c": Null()},
		{"a": Int(2), "b": Null(), "c": Float(2.5)},
	}
	for i := range want {
		if got[i]["a"] != want[i]["a"] || got[i]["b"] != want[i]["b"] || got[i]["c"] != want[i]["c"] {
			t.Fatalf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// 重开后还能回滚
	if err := tb2.Rollback(1); err != nil {
		t.Fatal(err)
	}
	got = readAll(t, tb2, "a", "b")
	if got[1]["b"] != Null() || got[0]["b"] != String("x") {
		t.Fatalf("after reopen+rollback: %+v", got)
	}
}
