package dav

import (
	"context"
	"encoding/xml"
	"net/http"
	"testing"
)

// memPropStore is a minimal in-memory PropStorageBackend.
type memPropStore struct {
	props map[string]map[xml.Name]any
}

func (m *memPropStore) PropFind(ctx context.Context, path string, pf *PropFind) error {
	for n, v := range m.props[path] {
		pf.Set(n, v, http.StatusOK)
	}
	return nil
}

func (m *memPropStore) PropPatch(ctx context.Context, path string, pp *PropPatch) error {
	pp.HandleRemaining(func(values map[xml.Name]any) (map[xml.Name]int, bool) {
		if m.props == nil {
			m.props = map[string]map[xml.Name]any{}
		}
		if m.props[path] == nil {
			m.props[path] = map[xml.Name]any{}
		}
		for n, v := range values {
			if v == nil {
				delete(m.props[path], n)
			} else {
				m.props[path][n] = v
			}
		}
		return nil, true
	})
	return nil
}

func (m *memPropStore) Delete(ctx context.Context, path string) error       { return nil }
func (m *memPropStore) Move(ctx context.Context, source, dest string) error { return nil }

// ownedColl is a collection with an owner, standing in for a Calendar.
type ownedColl struct {
	emptyColl
	owner string
}

func (o ownedColl) Owner() string { return o.owner }

func perUserCtx(principal string) context.Context {
	ctx := withPrincipalState(context.Background())
	SetPrincipal(ctx, principal)
	return ctx
}

// TestPerUserProps: a whitelisted property PROPPATCHed by a non-owner is
// stored per-user and overlays only that user's reads; other names fall
// through untouched, and owners/exempt principals bypass the layer entirely.
func TestPerUserProps(t *testing.T) {
	color := xml.Name{Space: "http://apple.com/ns/ical/", Local: "calendar-color"}
	other := xml.Name{Space: "DAV:", Local: "details"}
	store := &memPropStore{}
	plugin := &PerUserPropsPlugin{
		Backend: store,
		Names:   []xml.Name{color},
		Exempt:  []string{"principals/admin"},
	}
	node := ownedColl{owner: "principals/user1"}
	const path = "calendars/user1/cal"

	// Delegate writes color (claimed per-user) and details (left unclaimed).
	pp := NewPropPatch(map[xml.Name]any{color: "#000002", other: "shared"})
	if err := plugin.PrePropPatch(perUserCtx("principals/user2"), path, node, pp); err != nil {
		t.Fatal(err)
	}
	if got := pp.Remaining(); len(got) != 1 || got[0] != other {
		t.Fatalf("Remaining() = %v, want only the non-whitelisted prop", got)
	}
	// The global property-storage plugin claims the rest, like the real
	// pipeline (its PropPatchHook runs after the node handlers).
	if err := store.PropPatch(t.Context(), path, pp); err != nil {
		t.Fatal(err)
	}
	result, ok := pp.Commit()
	if !ok {
		t.Fatalf("commit failed: %v", result)
	}
	if v := store.props[path+"/\nprincipals/user2"][color]; v != "#000002" {
		t.Fatalf("per-user store = %v, want #000002", store.props)
	}
	if v := store.props[path][other]; v != "shared" {
		t.Fatalf("non-whitelisted prop must land in shared storage, got %v", store.props)
	}
	if _, leaked := store.props[path][color]; leaked {
		t.Fatalf("per-user prop leaked into shared storage: %v", store.props)
	}

	// The delegate's read sees their value; the owner's read does not.
	pf := NewPropFind(perUserCtx("principals/user2"), path, []xml.Name{color}, 0)
	if err := plugin.PropFindLate(pf, node); err != nil {
		t.Fatal(err)
	}
	if len(pf.Props404()) != 0 {
		t.Errorf("delegate read missing per-user color")
	}
	pf = NewPropFind(perUserCtx("principals/user1"), path, []xml.Name{color}, 0)
	if err := plugin.PropFindLate(pf, node); err != nil {
		t.Fatal(err)
	}
	if len(pf.Props404()) != 1 {
		t.Errorf("owner read must not see the delegate's per-user color")
	}

	// Owner and exempt admin writes are not claimed at all.
	for _, principal := range []string{"principals/user1", "principals/admin"} {
		pp = NewPropPatch(map[xml.Name]any{color: "#000009"})
		if err := plugin.PrePropPatch(perUserCtx(principal), path, node, pp); err != nil {
			t.Fatal(err)
		}
		if got := pp.Remaining(); len(got) != 1 {
			t.Errorf("%s write claimed per-user, want fall-through", principal)
		}
	}
}
