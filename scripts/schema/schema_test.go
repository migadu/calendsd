package schema

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestMigrations(t *testing.T) {
	ms := Migrations()
	if len(ms) < 2 || ms[0].Name != "sabre" || ms[1].Name != "extension" {
		t.Fatalf("Migrations() starts %v, want the sabre and extension baseline", ms)
	}
	for i, m := range ms {
		if m.Version != i+1 || strings.TrimSpace(m.SQL) == "" {
			t.Errorf("migration %d = version %d, %d bytes", i, m.Version, len(m.SQL))
		}
	}
	if Latest() != len(ms) {
		t.Errorf("Latest() = %d, want %d", Latest(), len(ms))
	}
}

func TestLoadRejectsBadNames(t *testing.T) {
	for name, files := range map[string][]string{
		"gap":        {"0001_a.sql", "0003_c.sql"},
		"unnumbered": {"0001_a.sql", "extra.sql"},
	} {
		fsys := fstest.MapFS{}
		for _, f := range files {
			fsys["migrations/"+f] = &fstest.MapFile{Data: []byte("SELECT 1;")}
		}
		if _, err := load(fsys); err == nil {
			t.Errorf("%s: load(%v) succeeded", name, files)
		}
	}
}

func TestTables(t *testing.T) {
	tables := Tables()
	for _, want := range []string{"calendarobjects", "users", "notifications", "calendar_availability", "free_busy_visibility"} {
		if !slices.Contains(tables, want) {
			t.Errorf("Tables() = %v, missing %s", tables, want)
		}
	}
}
