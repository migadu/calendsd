package acl

import (
	"context"
	"slices"
	"strings"

	"github.com/migadu/calendsd/internal/pathutil"
)

// DefaultPageSize is the page a PageRequest without a Limit gets.
const DefaultPageSize = 100

// PagePrincipals lists one page of the principals directly under prefix, in
// URI order: through the backend's keyset query when it is a Pager, and by
// paging the full listing in Go otherwise (the in-memory backend).
func PagePrincipals(ctx context.Context, backend Backend, prefix string, page PageRequest) ([]Principal, error) {
	if page.Limit <= 0 {
		page.Limit = DefaultPageSize
	}
	if pager, ok := backend.(Pager); ok {
		return pager.PrincipalsPage(ctx, prefix, page)
	}
	all, err := backend.PrincipalsByPrefix(ctx, prefix)
	if err != nil {
		return nil, err
	}
	return pageInGo(all, page), nil
}

// pageInGo applies a PageRequest to a full listing: the Go statement of the
// rule storage.Principals.PrincipalsPage runs in SQL.
func pageInGo(all []Principal, page PageRequest) []Principal {
	var kept []Principal
	for _, p := range all {
		if page.Search != "" && !searchMatches(p, page.Search) {
			continue
		}
		switch {
		case page.Before != "" && p.URI >= page.Before:
			continue
		case page.Before == "" && page.After != "" && p.URI <= page.After:
			continue
		}
		kept = append(kept, p)
	}
	slices.SortFunc(kept, func(a, b Principal) int { return strings.Compare(a.URI, b.URI) })
	if len(kept) <= page.Limit {
		return kept
	}
	if page.Before != "" {
		return kept[len(kept)-page.Limit:]
	}
	return kept[:page.Limit]
}

// searchMatches reports whether a principal's name or email starts with
// search, ignoring case.
func searchMatches(p Principal, search string) bool {
	search = strings.ToLower(search)
	_, name := pathutil.Split(p.URI)
	return strings.HasPrefix(strings.ToLower(name), search) ||
		strings.HasPrefix(strings.ToLower(principalEmail(p)), search)
}
