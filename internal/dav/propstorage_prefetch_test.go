package dav

import (
	"context"
	"encoding/xml"
	"net/http"
	"testing"

	davxml "github.com/migadu/calendsd/internal/dav/xml"
)

// prefetchStore is memPropStore as a PropPrefetcher, counting both kinds of
// read.
type prefetchStore struct {
	memPropStore
	perPath, batched int
}

func (s *prefetchStore) PropFind(ctx context.Context, path string, pf *PropFind) error {
	s.perPath++
	return s.memPropStore.PropFind(ctx, path, pf)
}

func (s *prefetchStore) PropFindMany(ctx context.Context, paths []string) (map[string][]StoredProp, error) {
	s.batched++
	out := map[string][]StoredProp{}
	for _, path := range paths {
		for n, v := range s.props[path] {
			out[path] = append(out[path], StoredProp{Name: n, Value: v})
		}
	}
	return out, nil
}

// A Depth:1 listing over a backend that can prefetch reads the stored
// properties of every child in one go, children without any included, and
// still serves them; a context outside a request (no holder) falls back to
// the per-path read.
func TestPropertyStoragePrefetchesListing(t *testing.T) {
	color := xml.Name{Space: "DAV:", Local: "details"}
	store := &prefetchStore{memPropStore: memPropStore{props: map[string]map[xml.Name]any{
		"root/a": {color: "A"},
		"root/c": {color: "C"},
	}}}
	root := &propColl{name: "root", children: []Node{
		&emptyFile{name: "a"}, &emptyFile{name: "b"}, &emptyFile{name: "c"},
	}}
	srv := NewServer("/", &propColl{children: []Node{root}})
	srv.AddPlugin(&PropertyStoragePlugin{Backend: store})

	ctx := withPrincipalState(context.Background())
	responses, err := srv.PropertiesForPath(ctx, "root", []xml.Name{color}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if store.batched != 1 || store.perPath != 0 {
		t.Errorf("listing read propertystorage %d times batched, %d per path; want 1 and 0", store.batched, store.perPath)
	}
	got := map[string]string{}
	for _, resp := range responses {
		for _, ps := range resp.Propstats {
			if ps.Status != http.StatusOK {
				continue
			}
			for _, prop := range ps.Props {
				if prop.Name == color {
					got[resp.Href] = string(prop.Value.(davxml.Text))
				}
			}
		}
	}
	if got["/root/a/"] != "A" || got["/root/c/"] != "C" || len(got) != 2 {
		t.Errorf("served properties %v, want a=A and c=C", got)
	}

	store.batched, store.perPath = 0, 0
	if _, err := srv.PropertiesForPath(context.Background(), "root", []xml.Name{color}, 1); err != nil {
		t.Fatal(err)
	}
	if store.perPath != 4 {
		t.Errorf("without a request holder the listing read per path %d times, want 4 (collection and children)", store.perPath)
	}
}

// propColl is an in-memory collection of files for the listing tests here.
type propColl struct {
	emptyColl
	name     string
	children []Node
}

func (c *propColl) Name() string { return c.name }
func (c *propColl) Children(context.Context) ([]Node, error) {
	return c.children, nil
}
func (c *propColl) Child(ctx context.Context, name string) (Node, error) {
	for _, child := range c.children {
		if child.Name() == name {
			return child, nil
		}
	}
	return nil, ErrNotFound("no %s", name)
}

type emptyFile struct {
	emptyColl
	name string
}

func (f *emptyFile) Name() string { return f.name }
