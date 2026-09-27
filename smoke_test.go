package colstore

import "testing"

func TestSmoke(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateTable("t1", []Column{
		{Name: "id", Type: TypeInt},
		{Name: "name", Type: TypeString},
	}); err != nil {
		t.Fatal(err)
	}
	tb, err := s.Table("t1")
	if err != nil {
		t.Fatal(err)
	}
	defer tb.Close()
	if tb.Version() != 1 {
		t.Fatalf("version = %d, want 1", tb.Version())
	}
	err = tb.Append([]Row{
		{"id": IntValue(1), "name": StringValue("alice")},
		{"id": IntValue(2)},
		{"name": StringValue("")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.AddColumn(Column{Name: "score", Type: TypeFloat}); err != nil {
		t.Fatal(err)
	}
	err = tb.Append([]Row{{"id": IntValue(3), "score": FloatValue(9.5)}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tb.Read([]string{"id", "name", "score"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}
	if rows[0]["id"].Int != 1 || rows[0]["name"].Str != "alice" {
		t.Fatalf("row0 = %+v", rows[0])
	}
	if !rows[1]["name"].Null {
		t.Fatalf("row1 name should be null: %+v", rows[1])
	}
	if rows[2]["name"].Null || rows[2]["name"].Str != "" {
		t.Fatalf("row2 name should be empty string, not null: %+v", rows[2])
	}
	if !rows[0]["score"].Null || !rows[2]["score"].Null {
		t.Fatal("score should be null for rows written before AddColumn")
	}
	if rows[3]["score"].Float != 9.5 || !rows[3]["name"].Null {
		t.Fatalf("row3 = %+v", rows[3])
	}
}
