package dav

import (
	"context"
	"encoding/xml"
	"net/http"
)

// PropStorageBackend persists arbitrary dead properties per path.
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

// StoredProp is one stored dead property as PropPrefetcher returns it: the
// value is what PropFind would Set, a string or an xml.Raw.
type StoredProp struct {
	Name  xml.Name
	Value any
}

// PropPrefetcher is a PropStorageBackend that can read the properties of
// many paths in one query (storage.PropertyStorage). With it, a Depth:1
// listing or a report over many objects costs one propertystorage read
// instead of one per node (docs/slow-queries.md, follow-up).
type PropPrefetcher interface {
	// PropFindMany returns the stored properties of each path, keyed by
	// path; a path with none has no entry.
	PropFindMany(ctx context.Context, paths []string) (map[string][]StoredProp, error)
}

// PropertyStoragePlugin persists any property no other subsystem claimed.
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

// prefetched is the request-scoped key under which Prefetch stores what it
// read: the properties of every path it was given, an empty slice for a
// path with none, so a missing entry means "not prefetched, ask per path".
type prefetched struct{ plugin *PropertyStoragePlugin }

// Prefetch implements PrefetchHook over a PropPrefetcher backend: one read
// for every allowed path of the response, served from the request memo by
// PropFindLate.
func (p *PropertyStoragePlugin) Prefetch(ctx context.Context, paths []string) error {
	prefetcher, ok := p.Backend.(PropPrefetcher)
	if !ok {
		return nil
	}
	var wanted []string
	for _, path := range paths {
		if p.allowed(path) {
			wanted = append(wanted, path)
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	found, err := prefetcher.PropFindMany(ctx, wanted)
	if err != nil {
		return err
	}
	cache, _ := RequestValue(ctx, prefetched{p})
	memo, _ := cache.(map[string][]StoredProp)
	if memo == nil {
		memo = map[string][]StoredProp{}
		SetRequestValue(ctx, prefetched{p}, memo)
	}
	for _, path := range wanted {
		memo[path] = found[path] // nil for a path without properties: still "known"
	}
	return nil
}

// PropFindLate implements LatePropFindHook: it runs after node dead
// properties.
func (p *PropertyStoragePlugin) PropFindLate(pf *PropFind, node Node) error {
	ctx := pf.Context()
	if !p.allowed(pf.Path()) {
		return nil
	}
	if cache, ok := RequestValue(ctx, prefetched{p}); ok {
		if props, known := cache.(map[string][]StoredProp)[pf.Path()]; known {
			for _, prop := range props {
				pf.Set(prop.Name, prop.Value, http.StatusOK)
			}
			return nil
		}
	}
	return p.Backend.PropFind(ctx, pf.Path(), pf)
}

// PropPatch implements PropPatchHook: it runs after the node's own handlers.
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
