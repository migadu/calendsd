package acl

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"calserver/internal/dav"
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

// defaultACL applies to nodes without their own ACL: anything goes as long
// as the user is authenticated, like sabre's $defaultAcl.
var defaultACL = []ACE{{Privilege: "{DAV:}all", Principal: "{DAV:}authenticated", Protected: true}}

// nodeACL returns the effective ACL: node ACL (or the default) plus admin
// ACEs. Port of sabre's Plugin::getAcl.
func (p *Plugin) nodeACL(node dav.Node) []ACE {
	var acl []ACE
	if holder, isACL := node.(ACLHolder); isACL {
		acl = append(acl, holder.ACL()...)
	} else {
		acl = append(acl, defaultACL...)
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

// CheckPrivileges verifies the current principal holds every listed
// privilege on path. Port of sabre's checkPrivileges (throwExceptions
// variant).
func (p *Plugin) CheckPrivileges(ctx context.Context, path string, privileges ...string) error {
	node, err := p.server.Tree.NodeForPath(path)
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
	exists := p.server.Tree.NodeExists(path)
	parent := func() string {
		parentPath, _ := splitPath(path)
		return parentPath
	}

	check := func(target string, privileges ...string) (bool, error) {
		if err := p.CheckPrivileges(ctx, target, privileges...); err != nil {
			return false, err
		}
		return true, nil
	}

	switch r.Method {
	case "GET", "HEAD", "OPTIONS", "REPORT", "PROPFIND":
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
		if !exists {
			return true, nil
		}
		// Sabre passes R_RECURSIVE here, but its checkPrivileges ignores
		// the recursion argument entirely — the effective behavior (and
		// ours) checks the top node only.
		if ok, err := check(path, "{DAV:}read"); !ok {
			return ok, err
		}
		dest, destErr := p.server.ResolveHref(r.Header.Get("Destination"))
		if destErr != nil {
			return true, nil // handler reports the bad destination
		}
		destParent, _ := splitPath(dest)
		if p.server.Tree.NodeExists(dest) {
			if ok, err := check(destParent, "{DAV:}unbind"); !ok {
				return ok, err
			}
		}
		if ok, err := check(destParent, "{DAV:}bind"); !ok {
			return ok, err
		}
		if r.Method == "MOVE" {
			return check(parent(), "{DAV:}unbind")
		}
	}
	return true, nil
}
