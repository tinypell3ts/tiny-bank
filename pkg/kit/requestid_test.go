package kit

import (
	"strings"
	"testing"
)

func TestValidRequestID(t *testing.T) {
	tests := map[string]bool{
		"abc-123_XYZ":           true,
		strings.Repeat("a", 64): true, // exactly the limit
		"":                      false,
		strings.Repeat("a", 65): false, // one over
		"abc\n":                 false, // a trailing newline must not slip through $
		"a b":                   false,
		"café":                  false, // ASCII only
		"id;drop":               false,
	}
	for id, want := range tests {
		if got := validRequestID(id); got != want {
			t.Errorf("validRequestID(%q) = %v, want %v", id, got, want)
		}
	}
}
