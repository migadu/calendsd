package schema

import (
	"slices"
	"testing"
)

func TestTables(t *testing.T) {
	tables := Tables()
	for _, want := range []string{"notifications", "calendar_availability", "free_busy_visibility"} {
		if !slices.Contains(tables, want) {
			t.Errorf("Tables() = %v, missing %s", tables, want)
		}
	}
}
