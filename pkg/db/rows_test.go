package db

import "testing"

func TestColumns(t *testing.T) {
	type row struct {
		ID        string `db:"id"`
		CreatedAt string `db:"created_at"`
		Skipped   string `db:"-"`
		internal  string // unexported fields aren't columns
	}
	_ = row{internal: "used, so lint knows it's on purpose"}
	if got := Columns[row](); got != "id, created_at" {
		t.Errorf("Columns = %q, want %q", got, "id, created_at")
	}
}

func TestColumnsRefusesAFieldWithNoTag(t *testing.T) {
	type row struct {
		ID   string `db:"id"`
		Name string
	}
	defer func() {
		if recover() == nil {
			t.Error("Columns accepted a field with no db tag, want a panic: its column would be left out of every query")
		}
	}()
	Columns[row]()
}
