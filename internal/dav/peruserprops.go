package dav

import (
	"context"
	"encoding/xml"
	"net/http"
	"slices"
)

// PerUserPropsPlugin stores property values per (principal, resource) on
// nodes the requester does not own, like Calendar Server: a delegate's
// PROPPATCH of displayname/calendar-color/anything lands in their own view
// and never clobbers the owner's values, and their PROPFIND sees their own
// values over the shared ones. Off unless registered (-per-user-props);
// sabre has no such feature.
//
// Values live in the regular property-storage backend under a synthetic
// path "<path>/\n<principal>" — the newline cannot occur in request paths,
// so the rows never collide with real ones and a co-existing sabre never
// resolves them. The "/" makes them look like children of the node, so the
// PropertyStoragePlugin's AfterUnbind/AfterMove prefix queries clean and
// rename them together with the node (a recreated collection must NOT
// resurrect a previous life's per-user values). Register AFTER the
// PropertyStoragePlugin so the late read phase overrides global values.
type PerUserPropsPlugin struct {
	Backend PropStorageBackend
	// Names is the property whitelist that gets per-user treatment.
	// Calendar Server scopes it to the personal presentation properties
	// (calendar-color, calendar-order); everything else a delegate writes
	// is a shared property, visible to the owner too.
	Names []xml.Name
	// Exempt principals (admins) keep writing shared properties: harness
	// provisioning PROPPATCHes resource calendars as admin and expects the
	// values to be globally visible.
	Exempt []string
	// MayWriteShared reports whether the principal may write the node's
	// shared (non-per-user) properties; the server wires it to the ACL
	// plugin's {DAV:}write-content check. A non-owner who may not is refused
	// every property outside Names with 403 (audit 5.2): the caldav ACL
	// grants read-only delegates {DAV:}write-properties so their per-user
	// writes get through, and without this gate that grant also let them
	// rename the owner's calendar. Nil fails closed.
	MayWriteShared func(ctx context.Context, path string) bool
}

func (p *PerUserPropsPlugin) PluginName() string { return "per-user-properties" }

func (p *PerUserPropsPlugin) Init(s *Server) {}

// perUserFor returns the synthetic storage path when the current principal
// gets a per-user view of the node, or "" for the shared path (owner, admin,
// unauthenticated, or unowned nodes).
func (p *PerUserPropsPlugin) perUserFor(ctx context.Context, path string, node Node) string {
	principal := Principal(ctx)
	if principal == "" || slices.Contains(p.Exempt, principal) {
		return ""
	}
	owned, ok := node.(interface{ Owner() string })
	if !ok {
		return ""
	}
	owner := owned.Owner()
	if owner == "" || owner == principal {
		return ""
	}
	return path + "/\n" + principal
}

// PropFindLate implements LatePropFindHook. Registered after the global
// PropertyStoragePlugin, so per-user values force-override both node/plugin
// values and globally stored ones; properties without a per-user value fall
// through to the shared view.
func (p *PerUserPropsPlugin) PropFindLate(pf *PropFind, node Node) error {
	ctx := pf.Context()
	perUser := p.perUserFor(pf.Context(), pf.Path(), node)
	if perUser == "" {
		return nil
	}
	return p.Backend.PropFind(ctx, perUser, pf)
}

// PrePropPatch implements PrePropPatchHook: for a non-owner it claims the
// whitelisted per-user mutations before the node's own handlers run, so a
// delegate's write of those cannot touch shared state. Everything else
// falls through to the regular handlers (a delegate's other property writes
// are shared, like Calendar Server) — but only for a delegate who may write
// the node; a read-only delegate's other properties fail with 403 and the
// 424 cascade does the rest.
func (p *PerUserPropsPlugin) PrePropPatch(ctx context.Context, path string, node Node, pp *PropPatch) error {
	perUser := p.perUserFor(ctx, path, node)
	if perUser == "" {
		return nil
	}
	pp.Handle(p.Names, func(values map[xml.Name]any) (map[xml.Name]int, bool) {
		// Delegate to the storage backend through a single-shot PropPatch
		// carrying only the claimed values.
		inner := NewPropPatch(values)
		if err := p.Backend.PropPatch(ctx, perUser, inner); err != nil {
			return nil, false
		}
		result, ok := inner.Commit()
		return result, ok
	})
	if p.MayWriteShared == nil || !p.MayWriteShared(ctx, path) {
		pp.HandleRemaining(func(values map[xml.Name]any) (map[xml.Name]int, bool) {
			result := make(map[xml.Name]int, len(values))
			for n := range values {
				result[n] = http.StatusForbidden
			}
			return result, false
		})
	}
	return nil
}
