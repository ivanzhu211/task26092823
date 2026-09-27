package colstore

import (
	"fmt"
	"testing"
	"time"
)

// 性能验收：50 列、200 万行的表上，
// 加列/删列/回滚各在 100ms 内，读出一列的全部行在 1s 内。
func TestPerfTwoMillionRows(t *testing.T) {
	const (
		numCols   = 50
		numRows   = 2_000_000
		batchRows = 50_000
	)
	s := openStore(t)
	cols := make([]Column, numCols)
	for i := range cols {
		typ := TypeInt
		switch i % 3 {
		case 1:
			typ = TypeFloat
		case 2:
			typ = TypeString
		}
		cols[i] = Column{Name: fmt.Sprintf("c%d", i), Type: typ}
	}
	mustCreate(t, s, "t", cols)
	tb := mustTable(t, s, "t")

	// 灌入 200 万行。
	batch := make([]Row, batchRows)
	for appended := 0; appended < numRows; appended += batchRows {
		for i := range batch {
			r := Row{}
			for j := 0; j < numCols; j += 7 { // 每行只给部分列赋值，其余为空
				c := cols[j]
				switch c.Type {
				case TypeInt:
					r[c.Name] = IntValue(int64(appended + i))
				case TypeFloat:
					r[c.Name] = FloatValue(float64(appended+i) * 0.5)
				case TypeString:
					r[c.Name] = StringValue("v")
				}
			}
			batch[i] = r
		}
		if err := tb.Append(batch); err != nil {
			t.Fatal(err)
		}
	}
	if got := tb.RowCount(); got != numRows {
		t.Fatalf("rows = %d", got)
	}

	// 加列 < 100ms。
	start := time.Now()
	if err := tb.AddColumn(Column{"added", TypeInt}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("AddColumn took %v, want < 100ms", d)
	} else {
		t.Logf("AddColumn: %v", d)
	}

	// 删列 < 100ms。
	start = time.Now()
	if err := tb.DropColumn("added"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("DropColumn took %v, want < 100ms", d)
	} else {
		t.Logf("DropColumn: %v", d)
	}

	// 回滚 < 100ms。
	start = time.Now()
	if err := tb.Rollback(2); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("Rollback took %v, want < 100ms", d)
	} else {
		t.Logf("Rollback: %v", d)
	}

	// 读出一列的全部行 < 1s（列式批量读接口）。
	start = time.Now()
	colVals, err := tb.ReadColumns([]string{"c0"})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Read one column took %v, want < 1s", d)
	} else {
		t.Logf("Read one column x %d rows: %v", numRows, d)
	}
	if len(colVals) != 1 || len(colVals[0]) != numRows {
		t.Fatalf("got %d x %d values", len(colVals), len(colVals[0]))
	}
	if colVals[0][0].Int != 0 || colVals[0][numRows-1].Int != numRows-1 {
		t.Fatalf("bad values: %v %v", colVals[0][0], colVals[0][numRows-1])
	}
	// 行式读接口抽查。
	rows, err := tb.Read([]string{"c0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != numRows || rows[0]["c0"].Int != 0 || rows[numRows-1]["c0"].Int != numRows-1 {
		t.Fatalf("row read mismatch")
	}
}
