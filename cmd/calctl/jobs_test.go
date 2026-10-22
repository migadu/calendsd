package main

import "testing"

func TestParseJobID(t *testing.T) {
	for s, want := range map[string]int64{"42": 42, "1": 1} {
		if id, ok := parseJobID(s); !ok || id != want {
			t.Errorf("parseJobID(%q) = %d, %v; want %d", s, id, ok, want)
		}
	}
	for _, s := range []string{"12abc", "0x1f", "1_0", "0", "-3", ""} {
		if id, ok := parseJobID(s); ok {
			t.Errorf("parseJobID(%q) = %d, want rejected", s, id)
		}
	}
}
