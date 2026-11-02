package main

import (
	"slices"
	"testing"

	"github.com/migadu/calendsd/internal/acl"
)

// `principal list` pages: one page with the cursor of the next, or with
// --all every page until the listing ends.
func TestForEachPrincipalPage(t *testing.T) {
	backend := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/a"}, {URI: "principals/b"}, {URI: "principals/c"},
		{URI: "principals/d"}, {URI: "principals/e"},
	}}
	collect := func(search, after string, limit int, all bool) (pages [][]string, next string) {
		t.Helper()
		next, err := forEachPrincipalPage(t.Context(), backend, search, after, limit, all, func(page []acl.Principal) {
			var uris []string
			for _, p := range page {
				uris = append(uris, p.URI)
			}
			pages = append(pages, uris)
		})
		if err != nil {
			t.Fatal(err)
		}
		return pages, next
	}

	if pages, next := collect("", "", 2, false); len(pages) != 1 || !slices.Equal(pages[0], []string{"principals/a", "principals/b"}) || next != "principals/b" {
		t.Errorf("one page: %v, next %q", pages, next)
	}
	if pages, next := collect("", "principals/b", 2, false); len(pages) != 1 || !slices.Equal(pages[0], []string{"principals/c", "principals/d"}) || next != "principals/d" {
		t.Errorf("after b: %v, next %q", pages, next)
	}
	if pages, next := collect("", "principals/d", 2, false); len(pages) != 1 || !slices.Equal(pages[0], []string{"principals/e"}) || next != "" {
		t.Errorf("last partial page: %v, next %q", pages, next)
	}
	if pages, next := collect("", "principals/e", 2, false); len(pages) != 0 || next != "" {
		t.Errorf("past the end: %v, next %q", pages, next)
	}
	pages, next := collect("", "", 2, true)
	if len(pages) != 3 || next != "" || !slices.Equal(pages[2], []string{"principals/e"}) {
		t.Errorf("--all: %v, next %q", pages, next)
	}
	// A listing that ends on a full page needs one more (empty) fetch to
	// know it has ended; the empty page is not emitted.
	if pages, next := collect("", "principals/a", 2, true); len(pages) != 2 || next != "" {
		t.Errorf("--all ending on a full page: %v, next %q", pages, next)
	}
	if pages, next := collect("c", "", 10, false); len(pages) != 1 || !slices.Equal(pages[0], []string{"principals/c"}) || next != "" {
		t.Errorf("--search c: %v, next %q", pages, next)
	}
}
