package acl

import (
	"context"
	"encoding/xml"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"calendsd/internal/dav"
	davxml "calendsd/internal/dav/xml"
	"calendsd/internal/pathutil"
)

// Privilege is one node of the supported-privilege tree (RFC 3744 §5.3).
type Privilege struct {
	Name       string
	Abstract   bool
	Aggregates []Privilege
}

// SupportedPrivilegeProvider lets nodes extend the default privilege tree
// (sabre's IACL::getSupportedPrivilegeSet), e.g. calendars adding
// read-free-busy.
type SupportedPrivilegeProvider interface {
	SupportedPrivileges() *Privilege
}

// supportedPrivileges returns the privilege tree for a node, mirroring
// sabre's getSupportedPrivilegeSet default.
func supportedPrivileges(node dav.Node) Privilege {
	if provider, ok := node.(SupportedPrivilegeProvider); ok {
		if p := provider.SupportedPrivileges(); p != nil {
			return *p
		}
	}
	read := Privilege{Name: "{DAV:}read", Aggregates: []Privilege{
		{Name: "{DAV:}read-acl"},
		{Name: "{DAV:}read-current-user-privilege-set"},
	}}
	write := Privilege{Name: "{DAV:}write", Aggregates: []Privilege{
		{Name: "{DAV:}write-properties"},
		{Name: "{DAV:}write-content"},
		{Name: "{DAV:}unlock"},
	}}
	if _, isColl := node.(dav.Collection); isColl {
		write.Aggregates = append(write.Aggregates,
			Privilege{Name: "{DAV:}bind"}, Privilege{Name: "{DAV:}unbind"})
	}
	if _, isACL := node.(ACLHolder); isACL {
		write.Aggregates = append(write.Aggregates, Privilege{Name: "{DAV:}write-acl"})
	}
	return Privilege{Name: "{DAV:}all", Abstract: true, Aggregates: []Privilege{read, write}}
}

// flatPrivileges flattens the tree into name → transitive aggregate names.
func flatPrivileges(root Privilege) map[string][]string {
	flat := map[string][]string{}
	var walk func(p Privilege)
	walk = func(p Privilege) {
		names := make([]string, len(p.Aggregates))
		for i, sub := range p.Aggregates {
			names[i] = sub.Name
		}
		flat[p.Name] = names
		for _, sub := range p.Aggregates {
			walk(sub)
		}
	}
	if root.Name != "{DAV:}all" {
		root = Privilege{Name: "{DAV:}all", Abstract: true, Aggregates: []Privilege{root}}
	}
	walk(root)
	return flat
}

// defaultACL applies to nodes without their own ACL: the fixed collections
// above the homes (root, principals, calendars, addressbooks), which any
// authenticated user may read but nobody may write. Sabre's $defaultAcl grants
// {DAV:}all here, which made those shared nodes writable (dead properties) and
// any node that forgot its ACL writable by everyone.
var defaultACL = []ACE{{Privilege: "{DAV:}read", Principal: "{DAV:}authenticated", Protected: true}}

// nodeACL returns the effective ACL: node ACL (or the default) plus admin
// ACEs. Port of sabre's Plugin::getAcl.
func (p *Plugin) nodeACL(node dav.Node) []ACE {
	var acl []ACE
	if holder, isACL := node.(ACLHolder); isACL {
		acl = append(acl, holder.ACL()...)
	} else {
		acl = append(acl, defaultACL...)
	}
	// Calendar-Server compat: any authenticated user may read principal
	// resources (delegation and invite UIs browse other users). Off by
	// default — stock sabre principals are owner-only.
	if p.PrincipalsReadableByAuthenticated {
		switch node.(type) {
		case *PrincipalNode, *UserPrincipalNode, *ProxyPrincipalNode:
			acl = append(acl, ACE{Privilege: "{DAV:}read", Principal: "{DAV:}authenticated", Protected: true})
		}
	}
	for _, admin := range p.AdminPrincipals {
		acl = append(acl, ACE{Privilege: "{DAV:}all", Principal: admin, Protected: true})
	}
	return acl
}

// CurrentUserPrivileges computes the expanded privilege set the current
// principal holds on a node. Port of sabre's getCurrentUserPrivilegeSet.
func (p *Plugin) CurrentUserPrivileges(ctx context.Context, node dav.Node) ([]string, error) {
	principals, err := p.CurrentUserPrincipals(ctx)
	if err != nil {
		return nil, err
	}
	isPrincipal := func(candidate string) bool {
		return slices.Contains(principals, candidate)
	}
	authenticated := dav.Principal(ctx) != ""

	var collected []string
	for _, ace := range p.nodeACL(node) {
		switch ace.Principal {
		case "{DAV:}owner":
			if owned, ok := node.(interface{ Owner() string }); ok {
				if owner := owned.Owner(); owner != "" && isPrincipal(owner) {
					collected = append(collected, ace.Privilege)
				}
			}
		case "{DAV:}all":
			collected = append(collected, ace.Privilege)
		case "{DAV:}authenticated":
			if authenticated {
				collected = append(collected, ace.Privilege)
			}
		case "{DAV:}unauthenticated":
			if !authenticated {
				collected = append(collected, ace.Privilege)
			}
		case SameDomain:
			if authenticated && p.sameDomainAsOwner(ctx, node) {
				collected = append(collected, ace.Privilege)
			}
		case FreeBusyAudience:
			if authenticated && p.inFreeBusyAudience(ctx, node, principals) {
				collected = append(collected, ace.Privilege)
			}
		default:
			if isPrincipal(ace.Principal) {
				collected = append(collected, ace.Privilege)
			}
		}
	}

	// Expand aggregates transitively; unknown privileges are ignored,
	// like sabre.
	flat := flatPrivileges(supportedPrivileges(node))
	seen := map[string]bool{}
	var out []string
	for len(collected) > 0 {
		priv := collected[len(collected)-1]
		collected = collected[:len(collected)-1]
		if seen[priv] {
			continue
		}
		seen[priv] = true
		out = append(out, priv)
		collected = append(collected, flat[priv]...)
	}
	return out, nil
}

// inFreeBusyAudience reports whether the current principal (or one of its
// groups, in principals) may read the node owner's free-busy under the
// owner's visibility setting. A setting that cannot be read denies: an outage
// may hide free-busy but never widens it.
func (p *Plugin) inFreeBusyAudience(ctx context.Context, node dav.Node, principals []string) bool {
	owned, ok := node.(interface{ Owner() string })
	if !ok {
		return false
	}
	owner := owned.Owner()
	if owner == "" {
		return false
	}
	if owner == dav.Principal(ctx) {
		return true
	}
	if p.FreeBusyPolicy == nil {
		return p.sameDomainAsOwner(ctx, node)
	}
	vis, err := p.FreeBusyPolicy.FreeBusyVisibility(ctx, owner)
	if err != nil {
		slog.Warn("reading free-busy visibility; denying", "owner", owner, "err", err)
		return false
	}
	var addresses []string // the requester's, loaded only if an address is listed
	for _, allowed := range vis.Allowed {
		if !strings.HasPrefix(allowed, "mailto:") {
			if slices.Contains(principals, allowed) {
				return true
			}
			continue
		}
		if addresses == nil {
			addresses = []string{}
			if n, err := p.server.Tree.NodeForPath(ctx, dav.Principal(ctx)); err == nil {
				if ph, ok := n.(PrincipalHolder); ok {
					for _, a := range ph.AlternateURISet() {
						addresses = append(addresses, strings.ToLower(a))
					}
				}
			}
		}
		if slices.Contains(addresses, allowed) {
			return true
		}
	}
	return !vis.Hidden && p.sameDomainAsOwner(ctx, node)
}

// sameDomainAsOwner reports whether the current principal's email domain
// equals the node owner's email domain (the SameDomain pseudo-principal).
func (p *Plugin) sameDomainAsOwner(ctx context.Context, node dav.Node) bool {
	owned, ok := node.(interface{ Owner() string })
	if !ok {
		return false
	}
	owner, current := owned.Owner(), dav.Principal(ctx)
	if owner == "" || current == "" {
		return false
	}
	if owner == current {
		return true
	}
	ownerDomain := p.principalEmailDomain(ctx, owner)
	return ownerDomain != "" && ownerDomain == p.principalEmailDomain(ctx, current)
}

// FilterChildren scopes the top-level principal and home-collection listings
// (/principals, /calendars, /addressbooks) to the requester's own email
// domain, so an authenticated user cannot enumerate the existence of users in
// other domains via PROPFIND Depth:1. It only touches single-segment root
// collections — one entry per principal; deeper listings (a specific home's
// own calendars) and the tree root are left intact. Admins and same-domain
// principals are kept; a requester with no email domain sees only itself.
// Unauthenticated listings are unchanged (gated elsewhere), and a child that
// exposes no owning principal is never hidden.
func (p *Plugin) FilterChildren(ctx context.Context, parentPath string, children []dav.Node) []dav.Node {
	// Deeper paths ("calendars/user1") list a single owner's own resources and
	// must not be filtered; the empty root ("") lists the mount collections.
	if parentPath == "" || strings.Contains(parentPath, "/") {
		return children
	}
	current := dav.Principal(ctx)
	if current == "" {
		return children
	}
	if slices.Contains(p.AdminPrincipals, current) {
		return children
	}
	myDomain := p.principalEmailDomain(ctx, current)
	kept := make([]dav.Node, 0, len(children))
	for _, child := range children {
		owned, ok := child.(interface{ Owner() string })
		if !ok {
			kept = append(kept, child) // not principal-owned; leave it visible
			continue
		}
		owner := owned.Owner()
		if owner == current {
			kept = append(kept, child)
			continue
		}
		if myDomain != "" && p.principalEmailDomain(ctx, owner) == myDomain {
			kept = append(kept, child)
		}
	}
	return kept
}

// sameDomainPrincipals filters a list of principal paths to those the current
// requester may see: itself, or any principal sharing its email domain. Admins
// and unauthenticated callers get the list unchanged. Backs the principal
// directory search (attendee autocomplete).
func (p *Plugin) sameDomainPrincipals(ctx context.Context, principals []string) []string {
	current := dav.Principal(ctx)
	if current == "" || slices.Contains(p.AdminPrincipals, current) {
		return principals
	}
	myDomain := p.principalEmailDomain(ctx, current)
	out := make([]string, 0, len(principals))
	for _, pr := range principals {
		if pr == current || (myDomain != "" && p.principalEmailDomain(ctx, pr) == myDomain) {
			out = append(out, pr)
		}
	}
	return out
}

// principalEmailDomain returns the lowercased domain of a principal's
// {sabredav}email-address property, or "" when there is none.
func (p *Plugin) principalEmailDomain(ctx context.Context, principalPath string) string {
	node, err := p.server.Tree.NodeForPath(ctx, principalPath)
	if err != nil {
		return ""
	}
	pn, ok := node.(dav.PropNode)
	if !ok {
		return ""
	}
	emailName := xml.Name{Space: davxml.NSSabreDAV, Local: "email-address"}
	props, err := pn.Props(ctx, []xml.Name{emailName})
	if err != nil {
		return ""
	}
	email, _ := props[emailName].(string)
	if at := strings.LastIndex(email, "@"); at >= 0 && at < len(email)-1 {
		return strings.ToLower(email[at+1:])
	}
	return ""
}

// CheckPrivileges verifies the current principal holds every listed
// privilege on path. Port of sabre's checkPrivileges (throwExceptions
// variant).
func (p *Plugin) CheckPrivileges(ctx context.Context, path string, privileges ...string) error {
	node, err := p.server.Tree.NodeForPath(ctx, path)
	if err != nil {
		return err
	}
	held, err := p.CurrentUserPrivileges(ctx, node)
	if err != nil {
		return err
	}
	heldSet := map[string]bool{}
	for _, priv := range held {
		heldSet[priv] = true
	}
	var failed []string
	for _, priv := range privileges {
		if !heldSet[priv] {
			failed = append(failed, priv)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	if p.AllowUnauthenticatedAccess && dav.Principal(ctx) == "" {
		return dav.ErrNotAuthenticated("Login was needed for privilege: %s on %s",
			strings.Join(failed, ", "), path)
	}
	return &dav.Error{
		Status:    http.StatusForbidden,
		Exception: `Sabre\DAVACL\Exception\NeedPrivileges`,
		Message:   "User did not have the required privileges (" + strings.Join(failed, ", ") + ") for path \"" + path + "\"",
	}
}

// hasPrivilege is CheckPrivileges without the error (sabre's
// throwExceptions=false form).
func (p *Plugin) hasPrivilege(ctx context.Context, path, privilege string) bool {
	return p.CheckPrivileges(ctx, path, privilege) == nil
}

// BeforeMethod enforces per-method privileges before dispatch, mirroring
// sabre's beforeMethod table plus the beforeBind/beforeUnbind event checks
// (which our port derives from method semantics rather than events).
func (p *Plugin) BeforeMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	ctx := r.Context()
	path, err := p.server.Path(r)
	if err != nil {
		return true, nil // URI errors surface in the method handler
	}
	exists := p.server.Tree.NodeExists(ctx, path)
	parent := func() string {
		parentPath, _ := pathutil.Split(path)
		return parentPath
	}

	check := func(target string, privileges ...string) (bool, error) {
		if err := p.CheckPrivileges(ctx, target, privileges...); err != nil {
			return false, err
		}
		return true, nil
	}

	switch r.Method {
	case "GET", "HEAD", "OPTIONS", "REPORT":
		// Sabre's beforeMethod gates GET/HEAD/OPTIONS with {DAV:}read.
		// PROPFIND is deliberately NOT gated here: the propFind hook marks
		// every property 403 instead, so a foreign-principal PROPFIND yields
		// a 207 with 403 propstats, not a request-level 403 (sabre parity —
		// CalDAVTester's proxy suites read other principals this way).
		// REPORT stays gated (stricter than sabre): our calendar-query
		// prefilter would otherwise leak matching object hrefs.
		if exists {
			return check(path, "{DAV:}read")
		}
	case "PUT":
		if exists {
			return check(path, "{DAV:}write-content")
		}
		return check(parent(), "{DAV:}bind")
	case "PROPPATCH":
		if exists {
			return check(path, "{DAV:}write-properties")
		}
	case "MKCOL", "MKCALENDAR":
		if !exists {
			return check(parent(), "{DAV:}bind")
		}
	case "DELETE":
		if exists {
			return check(parent(), "{DAV:}unbind")
		}
	case "COPY", "MOVE":
		// The destination checks do not depend on the source existing: the
		// handler would otherwise reach an unauthorized destination before
		// it notices the source is missing.
		//
		// Sabre passes R_RECURSIVE here, but its checkPrivileges ignores
		// the recursion argument entirely — the effective behavior (and
		// ours) checks the top node only.
		if exists {
			if ok, err := check(path, "{DAV:}read"); !ok {
				return ok, err
			}
		}
		dest, destErr := p.server.ResolveHref(r.Header.Get("Destination"))
		if destErr != nil {
			return true, nil // handler reports the bad destination
		}
		destParent, _ := pathutil.Split(dest)
		if p.server.Tree.NodeExists(ctx, dest) {
			if ok, err := check(destParent, "{DAV:}unbind"); !ok {
				return ok, err
			}
		}
		if ok, err := check(destParent, "{DAV:}bind"); !ok {
			return ok, err
		}
		if r.Method == "MOVE" && exists {
			return check(parent(), "{DAV:}unbind")
		}
	}
	return true, nil
}
