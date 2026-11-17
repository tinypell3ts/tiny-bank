package kit

import "testing"

func TestParseUUID(t *testing.T) {
	const canonical = "c0ffee00-0000-4000-8000-000000000001"
	if id, ok := ParseUUID(canonical); !ok || id.String() != canonical {
		t.Fatalf("ParseUUID(%q) = %v, %v; want it accepted", canonical, id, ok)
	}
	for _, raw := range []string{
		"C0FFEE00-0000-4000-8000-000000000001",   // upper case
		"c0ffee00000040008000000000000001",       // no dashes
		"{c0ffee00-0000-4000-8000-000000000001}", // braces
		"urn:uuid:c0ffee00-0000-4000-8000-000000000001",
		" c0ffee00-0000-4000-8000-000000000001",
		"not-a-uuid",
		"",
	} {
		if _, ok := ParseUUID(raw); ok {
			t.Errorf("ParseUUID(%q) accepted a non-canonical form", raw)
		}
	}
}
