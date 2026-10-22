package dav

import "context"

// PropStorageBackend persists arbitrary dead properties per path. Port of
// sabre's PropertyStorage\Backend\BackendInterface.
type PropStorageBackend interface {
	// PropFind resolves stored properties onto the PropFind object.
	PropFind(ctx context.Context, path string, pf *PropFind) error
	// PropPatch claims and applies mutations (typically HandleRemaining).
	PropPatch(ctx context.Context, path string, pp *PropPatch) error
	// Delete removes all properties of a path (and its children).
	Delete(ctx context.Context, path string) error
	// Move renames the property paths after a MOVE.
	Move(ctx context.Context, source, destination string) error
}

// PropertyStoragePlugin persists any property no other subsystem claimed.
// Port of sabre's PropertyStorage\Plugin.
type PropertyStoragePlugin struct {
	Backend PropStorageBackend
	// PathFilter, when set, limits which paths get stored properties.
	PathFilter func(path string) bool
}

func (p *PropertyStoragePlugin) PluginName() string { return "property-storage" }

func (p *PropertyStoragePlugin) Init(s *Server) {}

func (p *PropertyStoragePlugin) allowed(path string) bool {
	return p.PathFilter == nil || p.PathFilter(path)
}

// PropFindLate implements LatePropFindHook (sabre priority 130: after node
// dead properties).
func (p *PropertyStoragePlugin) PropFindLate(pf *PropFind, node Node) error {
	ctx := pf.Context()
	if !p.allowed(pf.Path()) {
		return nil
	}
	return p.Backend.PropFind(ctx, pf.Path(), pf)
}

// PropPatch implements PropPatchHook (sabre priority 300: after the node's
// own handlers).
func (p *PropertyStoragePlugin) PropPatch(ctx context.Context, path string, pp *PropPatch) error {
	if !p.allowed(path) {
		return nil
	}
	return p.Backend.PropPatch(ctx, path, pp)
}

func (p *PropertyStoragePlugin) AfterUnbind(ctx context.Context, path string) error {
	if !p.allowed(path) {
		return nil
	}
	return p.Backend.Delete(ctx, path)
}

func (p *PropertyStoragePlugin) AfterMove(ctx context.Context, source, destination string) error {
	if !p.allowed(source) || !p.allowed(destination) {
		return nil
	}
	return p.Backend.Move(ctx, source, destination)
}
