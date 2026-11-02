package storage_test

// Keyset pagination of the principal listing (docs/slow-queries.md, item 5):
// the same pages, in both directions and under a search term, as the Go
// paging acl.PagePrincipals applies to the full listing.

import (
	"slices"
	"testing"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/storage"
	"github.com/migadu/calendsd/internal/storage/storagetest"
)

func TestPrincipalsPage(t *testing.T) {
	db, qlog := storagetest.NewLoggedDB(t, "calserver_b_principals_page")
	mustExec(t, db, `
		INSERT INTO principals (uri, email, displayname) VALUES
			('principals/eve', 'eve@example.org', 'Eve'),
			('principals/bob', 'Bob@example.org', 'Bob'),
			('principals/alice', 'alice@example.org', 'Alice'),
			('principals/dan', NULL, 'Dan'),
			('principals/carol', 'carol@example.org', 'Carol'),
			('principals/alice/calendar-proxy-read', NULL, NULL),
			('principals/a_b', 'under@example.org', 'Underscore'),
			('other/zed', 'zed@example.org', 'Zed');`)
	principals := &storage.Principals{DB: db}
	// The in-memory paging over the same rows is the oracle.
	all, err := principals.PrincipalsByPrefix(t.Context(), "principals")
	if err != nil {
		t.Fatal(err)
	}
	oracle := &acl.MemoryBackend{Principals: all}

	uris := func(list []acl.Principal) []string {
		var out []string
		for _, p := range list {
			out = append(out, p.URI)
		}
		return out
	}
	for _, page := range []acl.PageRequest{
		{Limit: 2}, {Limit: 2, After: "principals/bob"}, {Limit: 2, After: "principals/dan"},
		{Limit: 2, After: "principals/eve"}, {Limit: 6}, {Limit: 7},
		{Limit: 2, Before: "principals/dan"}, {Limit: 2, Before: "principals/a_b"}, {Limit: 10, Before: "principals/eve"},
		{Limit: 10, Search: "A"}, {Limit: 10, Search: "bo"}, {Limit: 10, Search: "c", After: "principals/bob"},
		{Limit: 10, Search: "a_"}, {Limit: 10, Search: "%"}, {},
	} {
		qlog.Reset()
		got, err := principals.PrincipalsPage(t.Context(), "principals", page)
		if err != nil {
			t.Fatalf("%+v: %v", page, err)
		}
		want, err := acl.PagePrincipals(t.Context(), oracle, "principals", page)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(uris(got), uris(want)) {
			t.Errorf("%+v: SQL %v, Go %v", page, uris(got), uris(want))
		}
		if n := qlog.Count(); n != 1 {
			t.Errorf("%+v: %d queries, want 1", page, n)
		}
	}
	if got, _ := principals.PrincipalsPage(t.Context(), "principals", acl.PageRequest{Limit: 2, After: "principals/dan"}); len(got) != 1 {
		t.Errorf("last partial page has %d principals, want 1", len(got))
	}
}
