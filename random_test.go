package colstore

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// modelColumn 模型中的一列实例：同名再加列会拿到新的 id。
type modelColumn struct {
	id   int64
	name string
	typ  ColumnType
}

type modelVersion struct {
	num    int
	schema []modelColumn
}

// model 按业务规则独立推算的表状态。
type model struct {
	nextID   int64
	version  int
	schema   []modelColumn
	versions []modelVersion    // 保留的版本（含当前），最多 KeepVersions 个
	rows     []map[int64]Value // 每行：列实例 id -> 写入的值
}

func newModel(cols []Column) *model {
	m := &model{nextID: 0, version: 1}
	for _, c := range cols {
		m.schema = append(m.schema, modelColumn{id: m.nextID, name: c.Name, typ: c.Type})
		m.nextID++
	}
	m.snapshot()
	return m
}

func (m *model) snapshot() {
	schema := make([]modelColumn, len(m.schema))
	copy(schema, m.schema)
	m.versions = append(m.versions, modelVersion{num: m.version, schema: schema})
	if len(m.versions) > KeepVersions {
		m.versions = m.versions[len(m.versions)-KeepVersions:]
	}
}

func (m *model) append(rows []Row) {
	byName := map[string]modelColumn{}
	for _, c := range m.schema {
		byName[c.name] = c
	}
	for _, r := range rows {
		mr := map[int64]Value{}
		for name, v := range r {
			c := byName[name]
			mr[c.id] = v
		}
		m.rows = append(m.rows, mr)
	}
}

func (m *model) addColumn(name string, typ ColumnType) {
	m.schema = append(m.schema, modelColumn{id: m.nextID, name: name, typ: typ})
	m.nextID++
	m.version++
	m.snapshot()
}

func (m *model) dropColumn(name string) {
	out := m.schema[:0]
	for _, c := range m.schema {
		if c.name != name {
			out = append(out, c)
		}
	}
	m.schema = out
	m.version++
	m.snapshot()
}

func (m *model) rollback(v int) {
	for _, mv := range m.versions {
		if mv.num == v {
			schema := make([]modelColumn, len(mv.schema))
			copy(schema, mv.schema)
			m.schema = schema
			m.version++
			m.snapshot()
			return
		}
	}
	panic("model: rollback to unknown version")
}

func (m *model) retainedVersions() []int {
	out := make([]int, len(m.versions))
	for i, mv := range m.versions {
		out[i] = mv.num
	}
	return out
}

// expectRows 推算当前结构下全部行的读出结果。
func (m *model) expectRows() []Row {
	out := make([]Row, len(m.rows))
	for i, mr := range m.rows {
		r := Row{}
		for _, c := range m.schema {
			if v, ok := mr[c.id]; ok {
				r[c.name] = v
			} else {
				r[c.name] = NullValue()
			}
		}
		out[i] = r
	}
	return out
}

func randValue(rng *rand.Rand, typ ColumnType) Value {
	if rng.Intn(5) == 0 {
		return NullValue()
	}
	switch typ {
	case TypeInt:
		return IntValue(int64(rng.Intn(2000) - 1000))
	case TypeFloat:
		return FloatValue(rng.NormFloat64() * 100)
	case TypeString:
		switch rng.Intn(4) {
		case 0:
			return StringValue("") // 空串
		case 1:
			return StringValue(fmt.Sprintf("s%d", rng.Intn(100)))
		default:
			return StringValue(fmt.Sprintf("v%d-%d", rng.Intn(10), rng.Intn(10)))
		}
	}
	panic("bad type")
}

func checkFullRead(t *testing.T, tb *Table, m *model) {
	t.Helper()
	if tb.Version() != m.version {
		t.Fatalf("version = %d, model %d", tb.Version(), m.version)
	}
	schema := tb.Schema()
	if len(schema) != len(m.schema) {
		t.Fatalf("schema len = %d, model %d", len(schema), len(m.schema))
	}
	names := make([]string, len(schema))
	for i, c := range schema {
		if c.Name != m.schema[i].name || c.Type != m.schema[i].typ {
			t.Fatalf("schema[%d] = %+v, model %+v", i, c, m.schema[i])
		}
		names[i] = c.Name
	}
	vs, err := tb.Versions()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(vs, m.retainedVersions()) {
		t.Fatalf("versions = %v, model %v", vs, m.retainedVersions())
	}
	got, err := tb.Read(names)
	if err != nil {
		t.Fatal(err)
	}
	want := m.expectRows()
	if !reflect.DeepEqual(got, want) {
		for i := range want {
			if !reflect.DeepEqual(got[i], want[i]) {
				t.Fatalf("row %d: got %+v want %+v", i, got[i], want[i])
			}
		}
		t.Fatalf("row count: got %d want %d", len(got), len(want))
	}
}

// 随机测试：几千次混合操作，每步之后全量读出并与模型对比，中途多次关闭再打开。
func TestRandomMixedOps(t *testing.T) {
	rng := rand.New(rand.NewSource(20260928))
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	initCols := []Column{{"c0", TypeInt}, {"c1", TypeString}, {"c2", TypeFloat}}
	if err := s.CreateTable("t", initCols); err != nil {
		t.Fatal(err)
	}
	m := newModel(initCols)
	tb, err := s.Table("t")
	if err != nil {
		t.Fatal(err)
	}

	reopen := func() {
		if err := tb.Close(); err != nil {
			t.Fatal(err)
		}
		if rng.Intn(2) == 0 {
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(dir)
			if err != nil {
				t.Fatal(err)
			}
		}
		tb, err = s.Table("t")
		if err != nil {
			t.Fatal(err)
		}
	}

	const ops = 3000
	types := []ColumnType{TypeInt, TypeFloat, TypeString}
	for op := 0; op < ops; op++ {
		action := rng.Intn(100)
		switch {
		case action < 35: // 追加一批合法行
			n := 1 + rng.Intn(8)
			rows := make([]Row, n)
			for i := range rows {
				r := Row{}
				for _, c := range m.schema {
					if rng.Intn(3) > 0 { // 1/3 概率缺省 -> 空值
						r[c.name] = randValue(rng, c.typ)
					}
				}
				rows[i] = r
			}
			if err := tb.Append(rows); err != nil {
				t.Fatalf("op %d append: %v", op, err)
			}
			m.append(rows)

		case action < 40: // 追加非法批次：整批拒绝
			if len(m.schema) == 0 {
				break
			}
			c := m.schema[rng.Intn(len(m.schema))]
			bad := Row{}
			if rng.Intn(2) == 0 {
				bad["ghost_"+c.name] = IntValue(1) // 不存在的列
			} else {
				bad[c.name] = randValue(rng, types[(int(c.typ))%3]) // 错误类型（非空值）
				for bad[c.name].Null {
					bad[c.name] = randValue(rng, types[(int(c.typ))%3])
				}
			}
			rows := []Row{{}, bad}
			before := tb.RowCount()
			if err := tb.Append(rows); err == nil {
				t.Fatalf("op %d: invalid batch accepted", op)
			}
			if tb.RowCount() != before {
				t.Fatalf("op %d: invalid batch partially written", op)
			}

		case action < 55: // 加列（小名字池，必然出现同名再加）
			if len(m.schema) >= 12 {
				break
			}
			name := fmt.Sprintf("c%d", rng.Intn(12))
			used := false
			for _, c := range m.schema {
				if c.name == name {
					used = true
				}
			}
			if used {
				break
			}
			typ := types[rng.Intn(3)]
			if err := tb.AddColumn(Column{name, typ}); err != nil {
				t.Fatalf("op %d addColumn: %v", op, err)
			}
			m.addColumn(name, typ)

		case action < 70: // 删列
			if len(m.schema) <= 1 {
				break
			}
			c := m.schema[rng.Intn(len(m.schema))]
			if err := tb.DropColumn(c.name); err != nil {
				t.Fatalf("op %d dropColumn: %v", op, err)
			}
			m.dropColumn(c.name)

		case action < 85: // 回滚到保留的非当前版本
			var candidates []int
			for _, v := range m.retainedVersions() {
				if v != m.version {
					candidates = append(candidates, v)
				}
			}
			if len(candidates) == 0 {
				break
			}
			v := candidates[rng.Intn(len(candidates))]
			if err := tb.Rollback(v); err != nil {
				t.Fatalf("op %d rollback(%d): %v", op, v, err)
			}
			m.rollback(v)

		case action < 90: // 回滚到非法版本：必须报错
			if err := tb.Rollback(m.version); err == nil {
				t.Fatalf("op %d: rollback to current version accepted", op)
			}
			if err := tb.Rollback(m.version + 100); err == nil {
				t.Fatalf("op %d: rollback to future version accepted", op)
			}

		default: // 关闭再打开
			reopen()
		}
		checkFullRead(t, tb, m)
	}

	// 测试结束时：已不在任何保留版本里的列，磁盘上不再有它的数据。
	if err := tb.Close(); err != nil {
		t.Fatal(err)
	}
	meta, err := openMeta(filepath.Join(dir, "t", "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	live, err := meta.liveColIDs()
	if err != nil {
		t.Fatal(err)
	}
	meta.close()
	entries, err := os.ReadDir(filepath.Join(dir, "t"))
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
		t.Fatalf("on-disk columns %v != live columns %v", onDisk, live)
	}
}
