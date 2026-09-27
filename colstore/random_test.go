package colstore

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// modelCol 是模型中的一列（按列身份区分，同名新列是不同 modelCol）。
type modelCol struct {
	name string
	typ  Kind
	data map[int64]Value // 稀疏：只记写入时给了值的行
}

// model 按业务规则独立推算一张表应有的行为。
type model struct {
	versions [][]*modelCol // 每个版本的结构（含列身份）
	cols     []*modelCol   // 所有曾存在的列（按身份）
	rows     int64
	nextName int
	dropped  []string // 曾被删掉、可被同名再加的名字
}

func (m *model) cur() []*modelCol { return m.versions[len(m.versions)-1] }

func (m *model) retained() [][]*modelCol {
	v := m.versions
	if len(v) > KeepVersions {
		v = v[len(v)-KeepVersions:]
	}
	return v
}

func (m *model) append(rows []map[string]Value) {
	cur := m.cur()
	byName := map[string]*modelCol{}
	for _, c := range cur {
		byName[c.name] = c
	}
	for i, row := range rows {
		for name, v := range row {
			byName[name].data[m.rows+int64(i)] = v
		}
	}
	m.rows += int64(len(rows))
}

func (m *model) addColumn(name string, typ Kind) {
	c := &modelCol{name: name, typ: typ, data: map[int64]Value{}}
	m.cols = append(m.cols, c)
	m.versions = append(m.versions, append(append([]*modelCol{}, m.cur()...), c))
}

func (m *model) dropColumn(name string) {
	var next []*modelCol
	for _, c := range m.cur() {
		if c.name != name {
			next = append(next, c)
		}
	}
	m.dropped = append(m.dropped, name)
	m.versions = append(m.versions, next)
}

// rollback 回滚到 retained 中的第 idx 个版本（0=最老）。
func (m *model) rollback(idx int) {
	target := m.retained()[idx]
	m.versions = append(m.versions, append([]*modelCol{}, target...))
}

// randomTest 随机混合操作并与模型逐步比对。
func TestRandomAgainstModel(t *testing.T) {
	rng := rand.New(rand.NewSource(20260928))
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := s.CreateTable("t", []Column{{"c0", KindInt}, {"c1", KindString}})
	if err != nil {
		t.Fatal(err)
	}
	m := &model{nextName: 2}
	m.versions = append(m.versions, []*modelCol{
		{name: "c0", typ: KindInt, data: map[int64]Value{}},
		{name: "c1", typ: KindString, data: map[int64]Value{}},
	})
	m.cols = append(m.cols, m.cur()...)

	randValue := func(typ Kind) Value {
		switch rng.Intn(5) {
		case 0:
			return Null()
		}
		switch typ {
		case KindInt:
			return Int(rng.Int63n(1000) - 500)
		case KindFloat:
			return Float(rng.NormFloat64())
		default:
			if rng.Intn(4) == 0 {
				return String("") // 空串
			}
			return String(fmt.Sprintf("s%d", rng.Intn(100)))
		}
	}

	verify := func(step int) {
		t.Helper()
		// 结构、版本、行数一致
		sch := tb.Schema()
		cur := m.cur()
		if len(sch) != len(cur) {
			t.Fatalf("step %d: schema len %d, want %d", step, len(sch), len(cur))
		}
		for i, c := range cur {
			if sch[i].Name != c.name || sch[i].Type != c.typ {
				t.Fatalf("step %d: schema[%d]=%+v, want {%s %v}", step, i, sch[i], c.name, c.typ)
			}
		}
		if tb.RowCount() != m.rows {
			t.Fatalf("step %d: rows %d, want %d", step, tb.RowCount(), m.rows)
		}
		if tb.Version() != len(m.versions) {
			t.Fatalf("step %d: version %d, want %d", step, tb.Version(), len(m.versions))
		}
		// 读出全部列的全部行，逐格比对
		names := make([]string, len(cur))
		for i, c := range cur {
			names[i] = c.name
		}
		r, err := tb.Read(names)
		if err != nil {
			t.Fatalf("step %d: read: %v", step, err)
		}
		if int64(len(r.Values)) != m.rows {
			t.Fatalf("step %d: read rows %d, want %d", step, len(r.Values), m.rows)
		}
		for i := int64(0); i < m.rows; i++ {
			for j, c := range cur {
				want := Null()
				if v, ok := c.data[i]; ok {
					want = v
				}
				if got := r.Values[i][j]; got != want {
					t.Fatalf("step %d: row %d col %q = %+v, want %+v", step, i, c.name, got, want)
				}
			}
		}
	}

	const steps = 3000
	for step := 0; step < steps; step++ {
		op := rng.Intn(100)
		switch {
		case op < 45: // 追加 0~3 行
			cur := m.cur()
			n := rng.Intn(4)
			rows := make([]map[string]Value, n)
			for i := range rows {
				row := map[string]Value{}
				for _, c := range cur {
					if rng.Intn(2) == 0 {
						row[c.name] = randValue(c.typ)
					}
				}
				rows[i] = row
			}
			if err := tb.Append(rows); err != nil {
				t.Fatalf("step %d append: %v", step, err)
			}
			m.append(rows)
		case op < 60: // 加列（新名字）
			if len(m.cur()) >= 30 {
				continue
			}
			name := fmt.Sprintf("c%d", m.nextName)
			m.nextName++
			typ := Kind(1 + rng.Intn(3))
			if err := tb.AddColumn(Column{name, typ}); err != nil {
				t.Fatalf("step %d add: %v", step, err)
			}
			m.addColumn(name, typ)
		case op < 75: // 删列
			cur := m.cur()
			if len(cur) <= 1 {
				continue
			}
			name := cur[rng.Intn(len(cur))].name
			if err := tb.DropColumn(name); err != nil {
				t.Fatalf("step %d drop: %v", step, err)
			}
			m.dropColumn(name)
		case op < 85: // 同名再加列（全新的列）
			if len(m.dropped) == 0 || len(m.cur()) >= 30 {
				continue
			}
			inCur := map[string]bool{}
			for _, c := range m.cur() {
				inCur[c.name] = true
			}
			var cands []string
			for _, n := range m.dropped {
				if !inCur[n] {
					cands = append(cands, n)
				}
			}
			if len(cands) == 0 {
				continue
			}
			name := cands[rng.Intn(len(cands))]
			typ := Kind(1 + rng.Intn(3))
			if err := tb.AddColumn(Column{name, typ}); err != nil {
				t.Fatalf("step %d re-add: %v", step, err)
			}
			m.addColumn(name, typ)
		case op < 97: // 回滚到保留的某个非当前版本
			ret := m.retained()
			if len(ret) < 2 {
				continue
			}
			idx := rng.Intn(len(ret) - 1) // 不含当前版本
			libVer := tb.Versions()[idx]
			if err := tb.Rollback(libVer); err != nil {
				t.Fatalf("step %d rollback to v%d: %v", step, libVer, err)
			}
			m.rollback(idx)
		default: // 关闭再打开
			if err := tb.Close(); err != nil {
				t.Fatalf("step %d close: %v", step, err)
			}
			tb, err = s.OpenTable("t")
			if err != nil {
				t.Fatalf("step %d reopen: %v", step, err)
			}
		}
		verify(step)
	}
	if err := tb.Close(); err != nil {
		t.Fatal(err)
	}

	// 结束后：磁盘上的列文件 == 仍被保留版本引用的列，一个不多一个不少
	tb, err = s.OpenTable("t")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tb.m.db.Query(`SELECT col_id FROM columns`)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := map[string]bool{}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		wantFiles[fmt.Sprintf("%d.col", id)] = true
	}
	rows.Close()
	entries, err := os.ReadDir(filepath.Join(dir, "t", "cols"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !wantFiles[e.Name()] {
			t.Fatalf("leftover column file on disk: %s", e.Name())
		}
		delete(wantFiles, e.Name())
	}
	// 注意：从未追加过数据的列没有数据文件是正常的；
	// 但有 segments 记录的列必须有文件。
	segRows, err := tb.m.db.Query(`SELECT DISTINCT col_id FROM segments`)
	if err != nil {
		t.Fatal(err)
	}
	for segRows.Next() {
		var id int64
		segRows.Scan(&id)
		f := fmt.Sprintf("%d.col", id)
		if _, err := os.Stat(filepath.Join(dir, "t", "cols", f)); err != nil {
			t.Fatalf("column %d has segments but no data file", id)
		}
	}
	segRows.Close()
	// 模型侧交叉验证：模型认为还活着的列身份数 == 库中 columns 表行数
	alive := map[*modelCol]bool{}
	for _, v := range m.retained() {
		for _, c := range v {
			alive[c] = true
		}
	}
	var dbCols int
	if err := tb.m.db.QueryRow(`SELECT COUNT(*) FROM columns`).Scan(&dbCols); err != nil {
		t.Fatal(err)
	}
	if dbCols != len(alive) {
		t.Fatalf("live columns: db=%d model=%d", dbCols, len(alive))
	}
	// 保留版本号集合与模型一致
	gotVers := tb.Versions()
	wantFirst := len(m.versions) - KeepVersions + 1
	if len(gotVers) != KeepVersions || gotVers[0] != wantFirst {
		sort.Ints(gotVers)
		t.Fatalf("retained versions = %v, want %d..%d", gotVers, wantFirst, len(m.versions))
	}
	tb.Close()
}
