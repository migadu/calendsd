package storage_test

// Principal backend tests against a scratch Postgres with the sabre schema.
// Postgres-only; skips when the dev Postgres isn't reachable.

import (
	"reflect"
	"sort"
	"testing"

	"calserver/internal/storage"
)

// TestPrincipalsByPrefix pins the listing now filtered in SQL: direct
// children of the prefix only — not the proxy sub-principals, not other
// prefixes, and with LIKE metacharacters in the prefix matched literally.
func TestPrincipalsByPrefix(t *testing.T) {
	db := openCardDAVScratchDB(t, "calserver_d_principalsprefix")
	// The sabre seed ships principals/admin and its two proxy sub-principals.
	if _, err := db.Exec(`INSERT INTO principals (uri) VALUES
		('principals/alice'), ('principals/alice/calendar-proxy-read'),
		('groups/staff'), ('a_b/x'), ('aXb/y'), ('top')`); err != nil {
		t.Fatal(err)
	}
	p := &storage.Principals{DB: db}
	for prefix, want := range map[string][]string{
		"principals": {"principals/admin", "principals/alice"},
		"groups":     {"groups/staff"},
		"a_b":        {"a_b/x"},
		"":           {"top"},
		"nothing":    nil,
	} {
		list, err := p.PrincipalsByPrefix(t.Context(), prefix)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, pr := range list {
			got = append(got, pr.URI)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("PrincipalsByPrefix(%q) = %v, want %v", prefix, got, want)
		}
	}
}
