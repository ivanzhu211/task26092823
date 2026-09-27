package colstore

import (
	"fmt"
	"testing"
	"time"
)

// TestPerfTwoMillionRows 在 50 列、200 万行的表上验证：
//   - 加列 / 删列 / 回滚 各 < 100ms（与行数无关）
//   - 读出一列的全部行 < 1s
func TestPerfTwoMillionRows(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 50 列：20 int + 15 float + 15 string
	schema := make([]Column, 0, 50)
	for i := 0; i < 20; i++ {
		schema = append(schema, Column{fmt.Sprintf("i%d", i), KindInt})
	}
	for i := 0; i < 15; i++ {
		schema = append(schema, Column{fmt.Sprintf("f%d", i), KindFloat})
	}
	for i := 0; i < 15; i++ {
		schema = append(schema, Column{fmt.Sprintf("s%d", i), KindString})
	}
	tb, err := s.CreateTable("t", schema)
	if err != nil {
		t.Fatal(err)
	}
	// 预生成一批 5 万行，重复追加 40 次 = 200 万行
	const batch = 50000
	rows := make([]map[string]Value, batch)
	for i := range rows {
		row := make(map[string]Value, 50)
		for _, c := range schema {
			switch c.Type {
			case KindInt:
				row[c.Name] = Int(int64(i))
			case KindFloat:
				row[c.Name] = Float(float64(i) * 0.5)
			case KindString:
				row[c.Name] = String(fmt.Sprintf("v%d", i%1000))
			}
		}
		rows[i] = row
	}
	const totalRows = 2000000
	start := time.Now()
	for n := 0; n < totalRows/batch; n++ {
		if err := tb.Append(rows); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("append %d rows x 50 cols: %v", totalRows, time.Since(start))
	if tb.RowCount() != totalRows {
		t.Fatalf("rows = %d", tb.RowCount())
	}

	// 加列 < 100ms
	elapsed := measure(func() {
		if err := tb.AddColumn(Column{"extra", KindInt}); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("AddColumn: %v", elapsed)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("AddColumn took %v, want < 100ms", elapsed)
	}

	// 删列 < 100ms
	elapsed = measure(func() {
		if err := tb.DropColumn("extra"); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("DropColumn: %v", elapsed)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("DropColumn took %v, want < 100ms", elapsed)
	}

	// 回滚 < 100ms（回滚到加列前的 v1）
	elapsed = measure(func() {
		if err := tb.Rollback(1); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("Rollback: %v", elapsed)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("Rollback took %v, want < 100ms", elapsed)
	}

	// 读出一列全部 200 万行 < 1s
	var r *Rows
	elapsed = measure(func() {
		var err error
		r, err = tb.Read([]string{"i7"})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("Read one column (%d rows): %v", totalRows, elapsed)
	if elapsed > time.Second {
		t.Fatalf("Read one column took %v, want < 1s", elapsed)
	}
	if int64(len(r.Values)) != totalRows || r.Values[123][0] != Int(123%batch) {
		t.Fatalf("bad read result")
	}

	// 字符串列也验证一下
	elapsed = measure(func() {
		r, err = tb.Read([]string{"s3"})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("Read one string column: %v", elapsed)
	if elapsed > time.Second {
		t.Fatalf("Read string column took %v, want < 1s", elapsed)
	}
	if err := tb.Close(); err != nil {
		t.Fatal(err)
	}
}

func measure(f func()) time.Duration {
	start := time.Now()
	f()
	return time.Since(start)
}
