package acl

// Scoped root listings (docs/slow-queries.md, item 2): the principal, calendar
// and address-book roots list one child per user, and FilterChildren keeps
// the requester plus its email domain. Rather than listing every principal
// and then looking each one up to apply that rule, the plugin states the
// rule as a dav.ChildScope and the roots list only what it keeps.

import (
	"context"
	"slices"
	"strings"

	"calendsd/internal/dav"
	"calendsd/internal/pathutil"
)

var _ dav.ChildScopeHook = (*Plugin)(nil)

// ChildScope implements dav.ChildScopeHook with FilterChildren's rule: only
// single-segment root collections are filtered, only for an authenticated
// requester, and admins see everything, so they get no scope.
func (p *Plugin) ChildScope(ctx context.Context, parentPath string) (dav.ChildScope, bool) {
	if parentPath == "" || strings.Contains(parentPath, "/") {
		return dav.ChildScope{}, false
	}
	current := dav.Principal(ctx)
	if current == "" || slices.Contains(p.AdminPrincipals, current) {
		return dav.ChildScope{}, false
	}
	return dav.ChildScope{Principal: current, Domain: p.principalEmailDomain(ctx, current)}, true
}

// ScopedPrincipals lists the principals directly under prefix that
// FilterChildren keeps for scope: those in the scope's email domain, in one
// indexed query when the backend is a DomainLister, plus the requester
// itself when it lives under prefix, whether or not it has a domain. The
// roots build their ChildrenScoped on it.
func ScopedPrincipals(ctx context.Context, backend Backend, prefix string, scope dav.ChildScope) ([]Principal, error) {
	var out []Principal
	if scope.Domain != "" {
		var err error
		if lister, ok := backend.(DomainLister); ok {
			out, err = lister.PrincipalsByPrefixInDomain(ctx, prefix, scope.Domain)
		} else {
			var all []Principal
			all, err = backend.PrincipalsByPrefix(ctx, prefix)
			for _, p := range all {
				if EmailDomain(principalEmail(p)) == scope.Domain {
					out = append(out, p)
				}
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if parent, _ := pathutil.Split(scope.Principal); parent != prefix {
		return out, nil
	}
	if slices.ContainsFunc(out, func(p Principal) bool { return p.URI == scope.Principal }) {
		return out, nil
	}
	self, err := backend.PrincipalByPath(ctx, scope.Principal)
	if err != nil {
		return nil, err
	}
	if self != nil {
		out = append([]Principal{*self}, out...)
	}
	return out, nil
}
