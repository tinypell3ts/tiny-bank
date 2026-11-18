package db

import "testing"

func TestOperationOf(t *testing.T) {
	for sql, want := range map[string]string{
		"SELECT id FROM journals WHERE id = $1":       "SELECT",
		"\n\t  insert into journals (id) values ($1)": "INSERT",
		"UPDATE\ntransfers SET state = $2":            "UPDATE",
		"begin":                                       "BEGIN",
		"commit;":                                     "COMMIT",
		"  ":                                          "QUERY",
	} {
		if got := operationOf(sql); got != want {
			t.Errorf("operationOf(%q) = %q, want %q", sql, got, want)
		}
	}
}
