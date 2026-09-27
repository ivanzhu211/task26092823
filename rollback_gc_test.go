package colstore

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func schemaNames(cols []Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

// 回滚后：当前结构的列名、类型、顺序与目标版本完全相同。
func TestRollbackRestoresSchema(t *testing.T) {
	s := openStore(t)
	mustCreate(t, s, "t", []Column{{"a", TypeInt}, {"b", TypeString}})
	tb := mustTable(t, s, "t")
	if err := tb.AddColumn(Column{"c", TypeFloat}); err != nil { // v2
		t.Fatal(err)
	}
	if err := tb.DropColumn("b"); err != nil { // v3
		t.Fatal(err)
	}
	if err := tb.Rollback(2); err != nil { // v4 = v2 的结构
		t.Fatal(err)
	}
	got := tb.Schema()
	want := []Column{{"a", TypeInt}, {"b", TypeString}, {"c", TypeFloat}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema = %+v, want %+v", got, want)
	}
}

// 回滚的数据语义：每一行读出写入时写进该列的值；写入时该列不在结构里的行为空；
// 目标版本之后加的列消失，但回滚到包含它的版本时数据原样回来。
func TestRollbackDataSemantics(t *testing.T) {
	s := openStore(t)
	mustCreate(t, s, "t", []Column{{"a", TypeInt}})
	tb := mustTable(t, s, "t")
	mustAppend := func(rows ...Row) {
		t.Helper()
		if err := tb.Append(rows); err != nil {
			t.Fatal(err)
		}
	}
	mustAppend(Row{"a": IntValue(10)}, Row{"a": IntValue(11)})    // v1 时代：行 0,1
	if err := tb.AddColumn(Column{"b", TypeString}); err != nil { // v2
		t.Fatal(err)
	}
	mustAppend(Row{"a": IntValue(12), "b": StringValue("x")}, Row{"a": IntValue(13), "b": StringValue("y")}) // 行 2,3
	if err := tb.DropColumn("a"); err != nil {                                                               // v3
		t.Fatal(err)
	}
	mustAppend(Row{"b": StringValue("z")}) // 行 4（此时 a 不在结构里）

	if err := tb.Rollback(2); err != nil { // v4：结构 [a, b]
		t.Fatal(err)
	}
	if got := schemaNames(tb.Schema()); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("schema = %v", got)
	}
	rows, err := tb.Read([]string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	wantA := []Value{IntValue(10), IntValue(11), IntValue(12), IntValue(13), NullValue()}
	wantB := []Value{NullValue(), NullValue(), StringValue("x"), StringValue("y"), StringValue("z")}
	for i := range rows {
		if rows[i]["a"] != wantA[i] {
			t.Fatalf("row %d a = %+v, want %+v", i, rows[i]["a"], wantA[i])
		}
		if rows[i]["b"] != wantB[i] {
			t.Fatalf("row %d b = %+v, want %+v", i, rows[i]["b"], wantB[i])
		}
	}

	// 回滚到 v1：b 从结构里消失。
	if err := tb.Rollback(1); err != nil { // v5
		t.Fatal(err)
	}
	if got := schemaNames(tb.Schema()); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("schema = %v", got)
	}
	// 再回滚到 v4（含 b 的版本）：b 的数据原样回来。
	if err := tb.Rollback(4); err != nil { // v6
		t.Fatal(err)
	}
	rows, err = tb.Read([]string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		if rows[i]["b"] != wantB[i] {
			t.Fatalf("row %d b = %+v, want %+v", i, rows[i]["b"], wantB[i])
		}
	}
}

// 删掉同名再加：是全新的列，读不到旧列的任何数据。
func TestReaddSameNameIsNewColumn(t *testing.T) {
	s := openStore(t)
	mustCreate(t, s, "t", []Column{{"x", TypeInt}, {"y", TypeInt}})
	tb := mustTable(t, s, "t")
	if err := tb.Append([]Row{{"x": IntValue(1), "y": IntValue(2)}}); err != nil {
		t.Fatal(err)
	}
	if err := tb.DropColumn("x"); err != nil { // v2
		t.Fatal(err)
	}
	if err := tb.AddColumn(Column{"x", TypeInt}); err != nil { // v3：同名同类型的新列
		t.Fatal(err)
	}
	if err := tb.Append([]Row{{"x": IntValue(99), "y": IntValue(3)}}); err != nil {
		t.Fatal(err)
	}
	rows, err := tb.Read([]string{"x", "y"})
	if err != nil {
		t.Fatal(err)
	}
	if !rows[0]["x"].Null {
		t.Fatalf("old x data leaked into new column: %+v", rows[0]["x"])
	}
	if rows[1]["x"].Int != 99 {
		t.Fatalf("new x = %+v", rows[1]["x"])
	}
	if rows[0]["y"].Int != 2 || rows[1]["y"].Int != 3 {
		t.Fatalf("y = %+v %+v", rows[0]["y"], rows[1]["y"])
	}
}

// 列不再出现在任何保留版本里时，数据文件在操作结束时从磁盘删除。
func TestGCRemovesDeadColumnFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mustCreate(t, s, "t", []Column{{"a", TypeInt}, {"dead", TypeString}})
	tb, err := s.Table("t")
	if err != nil {
		t.Fatal(err)
	}
	defer tb.Close()
	if err := tb.Append([]Row{{"a": IntValue(1), "dead": StringValue("x")}}); err != nil {
		t.Fatal(err)
	}
	if err := tb.DropColumn("dead"); err != nil { // v2；dead 仍在 v1 里，文件保留
		t.Fatal(err)
	}
	tdir := filepath.Join(dir, "t")
	countFiles := func() int {
		entries, err := os.ReadDir(tdir)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".dat" {
				n++
			}
		}
		return n
	}
	if n := countFiles(); n != 2 {
		t.Fatalf("files = %d, want 2 (dead column still referenced by v1)", n)
	}
	// 再做 19 次结构变更 -> 当前 v21，保留 v2..v21，v1 被淘汰，dead 列彻底失引用。
	for i := 0; i < 19; i++ {
		if err := tb.AddColumn(Column{"tmp" + itoa(i), TypeInt}); err != nil {
			t.Fatal(err)
		}
	}
	if n := countFiles(); n != 20 {
		t.Fatalf("files = %d, want 20 (a + 19 tmp)", n)
	}
	// 连续删 19 个临时列，每次产生新版本 -> 当前 v40，结构只剩 [a]。
	// 但 tmp_i 最后出现的版本是 v(21+i)，仍在保留窗口 v21..v40 内，文件保留。
	for i := 0; i < 19; i++ {
		if err := tb.DropColumn("tmp" + itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if n := countFiles(); n != 20 {
		t.Fatalf("files = %d, want 20 (tmp columns still referenced by retained versions)", n)
	}
	// 再加 19 个哨兵列 -> 当前 v59，保留 v40..v59，tmp 列彻底失引用，文件删除。
	for i := 0; i < 19; i++ {
		if err := tb.AddColumn(Column{"s" + itoa(i), TypeInt}); err != nil {
			t.Fatal(err)
		}
	}
	if n := countFiles(); n != 20 {
		t.Fatalf("files = %d, want 20 (a + 19 sentinels)", n)
	}
	// 磁盘上的列文件集合与元数据中的存活列一致。
	m, err := openMeta(filepath.Join(tdir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	live, err := m.liveColIDs()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(tdir)
	if err != nil {
		t.Fatal(err)
	}
	onDisk := map[int64]bool{}
	for _, e := range entries {
		var id int64
		if n, _ := fmt.Sscanf(e.Name(), "c%d.dat", &id); n == 1 {
			onDisk[id] = true
		}
	}
	if !reflect.DeepEqual(onDisk, live) {
		t.Fatalf("on-disk %v != live %v", onDisk, live)
	}
}

// 关闭后重新打开，读出的结果与关闭前完全一致。
func TestCloseReopenConsistency(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, "t", []Column{{"a", TypeInt}, {"s", TypeString}})
	tb, err := s.Table("t")
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.Append([]Row{{"a": IntValue(1), "s": StringValue("one")}, Row{"a": IntValue(2)}}); err != nil {
		t.Fatal(err)
	}
	if err := tb.AddColumn(Column{"f", TypeFloat}); err != nil {
		t.Fatal(err)
	}
	if err := tb.Append([]Row{{"f": FloatValue(0.5), "s": StringValue("")}}); err != nil {
		t.Fatal(err)
	}
	if err := tb.DropColumn("a"); err != nil {
		t.Fatal(err)
	}
	if err := tb.Rollback(2); err != nil {
		t.Fatal(err)
	}
	before, err := tb.Read([]string{"a", "s", "f"})
	if err != nil {
		t.Fatal(err)
	}
	verBefore := tb.Version()
	if err := tb.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	tb2, err := s2.Table("t")
	if err != nil {
		t.Fatal(err)
	}
	defer tb2.Close()
	if tb2.Version() != verBefore {
		t.Fatalf("version = %d, want %d", tb2.Version(), verBefore)
	}
	after, err := tb2.Read([]string{"a", "s", "f"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("before %+v != after %+v", before, after)
	}
	// 重开后继续追加、改结构仍正常。
	if err := tb2.Append([]Row{{"a": IntValue(9)}}); err != nil {
		t.Fatal(err)
	}
	if err := tb2.AddColumn(Column{"g", TypeInt}); err != nil {
		t.Fatal(err)
	}
	rows, err := tb2.Read([]string{"a", "g"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || rows[3]["a"].Int != 9 || !rows[0]["g"].Null {
		t.Fatalf("rows = %+v", rows)
	}
}
