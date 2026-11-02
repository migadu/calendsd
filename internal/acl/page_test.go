package acl_test

import (
	"slices"
	"testing"

	"github.com/migadu/calendsd/internal/acl"
)

// The Go paging that PagePrincipals applies over a backend without keyset
// queries: page boundaries in both directions and the search rule.
func TestPagePrincipalsInGo(t *testing.T) {
	backend := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/eve", Props: emailProps("eve@example.org")},
		{URI: "principals/bob", Props: emailProps("Bob@example.org")},
		{URI: "principals/alice", Props: emailProps("alice@example.org")},
		{URI: "principals/dan"},
		{URI: "principals/carol", Props: emailProps("carol@example.org")},
		{URI: "principals/alice/calendar-proxy-read"},
		{URI: "other/zed"},
	}}
	uris := func(page acl.PageRequest) []string {
		t.Helper()
		list, err := acl.PagePrincipals(t.Context(), backend, "principals", page)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range list {
			out = append(out, p.URI)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		page acl.PageRequest
		want []string
	}{
		{"first page", acl.PageRequest{Limit: 2}, []string{"principals/alice", "principals/bob"}},
		{"second page", acl.PageRequest{Limit: 2, After: "principals/bob"}, []string{"principals/carol", "principals/dan"}},
		{"last partial page", acl.PageRequest{Limit: 2, After: "principals/dan"}, []string{"principals/eve"}},
		{"past the end", acl.PageRequest{Limit: 2, After: "principals/eve"}, nil},
		{"exact page", acl.PageRequest{Limit: 5}, []string{"principals/alice", "principals/bob", "principals/carol", "principals/dan", "principals/eve"}},
		{"before", acl.PageRequest{Limit: 2, Before: "principals/dan"}, []string{"principals/bob", "principals/carol"}},
		{"before the start", acl.PageRequest{Limit: 2, Before: "principals/alice"}, nil},
		{"search name", acl.PageRequest{Limit: 10, Search: "A"}, []string{"principals/alice"}},
		{"search email", acl.PageRequest{Limit: 10, Search: "bo"}, []string{"principals/bob"}},
		{"search after", acl.PageRequest{Limit: 10, Search: "c", After: "principals/bob"}, []string{"principals/carol"}},
		{"default limit", acl.PageRequest{}, []string{"principals/alice", "principals/bob", "principals/carol", "principals/dan", "principals/eve"}},
	} {
		if got := uris(tc.page); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
