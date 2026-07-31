package acl_test

import (
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	davxml "calserver/internal/dav/xml"
)

// leakServer builds a two-user server with one calendar owned by user1 whose
// CalendarInfo.Props carry dead properties (e.g. displayname). It optionally
// registers an in-memory property-storage plugin seeded with a stored dead
// property for the calendar path. authAs is the logged-in principal.
func leakServer(authAs string, calProps map[xml.Name]any, propStore *mock.MemoryPropStorage) *dav.Server {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1"}, {URI: "principals/user2"}, {URI: "principals/admin"},
	}}
	backend := &caldav.MockBackend{
		Calendars: []caldav.CalendarInfo{{
			ID: caldav.CalendarID{Calendar: "1"}, URI: "cal", PrincipalURI: "principals/user1",
			Props: calProps,
		}},
	}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))

	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin(authAs))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	if propStore != nil {
		srv.AddPlugin(&dav.PropertyStoragePlugin{Backend: propStore})
	}
	return srv
}

func propfind(t *testing.T, srv *dav.Server, path, body, depth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PROPFIND", path, strings.NewReader(body))
	req.Header.Set("Depth", depth)
	if body != "" {
		req.Header.Set("Content-Type", "application/xml")
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

// TestBugAllpropLeaksDeadProps pins the HIGH-severity ACL PropFind leak
// (internal/acl/plugin.go:100-107). A stranger issuing an allprop PROPFIND
// (empty body) against a calendar they cannot read must NOT receive the
// calendar's dead properties. The ACL gate only marks props known at gate
// time (pf.Requested()) as 403; node dead properties added afterward at the
// late phase leak at status 200.
//
// Asserts CORRECT (secure) behavior, so it FAILS against the current buggy
// code, demonstrating the leak.
func TestBugAllpropLeaksDeadProps(t *testing.T) {
	dn := xml.Name{Space: davxml.NSDav, Local: "displayname"}
	srv := leakServer("principals/user2",
		map[xml.Name]any{dn: "SECRET-CALENDAR-NAME"}, nil)

	// Empty body == allprop request.
	w := propfind(t, srv, "/calendars/user1/cal/", "", "0")
	if w.Code != 207 {
		t.Fatalf("stranger PROPFIND = %d, want 207:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Core assertion: the owner's private displayname must not leak.
	if strings.Contains(body, "SECRET-CALENDAR-NAME") {
		t.Errorf("BUG: stranger allprop PROPFIND leaked owner's dead property:\n%s", body)
	}

	// Sanity: the gate is otherwise working — standard props come back 403,
	// with no 200 OK propstat (mirrors TestStrangerCannotReadCalendar).
	if !strings.Contains(body, "HTTP/1.1 403 Forbidden") ||
		strings.Contains(body, "HTTP/1.1 200 OK") {
		t.Errorf("stranger PROPFIND should return only 403 propstats:\n%s", body)
	}
}

// TestBugNamedPropLeaksFromPropertyStorage pins the same HIGH-severity leak
// for property-storage properties: a stranger issuing a NAMED PROPFIND for a
// dead property held in property storage must NOT receive it. The ACL gate
// marks the requested name 403, but the very-late property-storage phase
// (LatePropFindHook) re-adds it at status 200, overriding the denial.
//
// The stored value is seeded via a PROPPATCH performed by the owner (user1),
// then read back as the stranger (user2).
func TestBugNamedPropLeaksFromPropertyStorage(t *testing.T) {
	color := xml.Name{Space: "http://apple.com/ns/ical/", Local: "calendar-color"}
	// Seed the dead property directly into property storage for the calendar
	// path. (A PROPPATCH through the pipeline would instead be claimed by the
	// calendar node's own UpdateCalendar handler before reaching the
	// property-storage plugin, exercising the node path rather than the
	// property-storage late-hook path this test targets.)
	const calPath = "calendars/user1/cal"
	store := &mock.MemoryPropStorage{Props: map[string]map[xml.Name]any{
		calPath: {color: "#FF0000FF"},
	}}
	if !storeHas(store, color, "#FF0000FF") {
		t.Fatalf("property storage not seeded; store=%v", store.Props)
	}

	// Stranger requests the stored property by name.
	strangerSrv := leakServer("principals/user2", map[xml.Name]any{}, store)
	named := `<?xml version="1.0"?>` +
		`<d:propfind xmlns:d="DAV:" xmlns:x="http://apple.com/ns/ical/">` +
		`<d:prop><x:calendar-color/></d:prop></d:propfind>`
	w := propfind(t, strangerSrv, "/calendars/user1/cal/", named, "0")
	if w.Code != 207 {
		t.Fatalf("stranger PROPFIND = %d, want 207:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Core assertion: the stored color must not leak at 200.
	if strings.Contains(body, "#FF0000FF") {
		t.Errorf("BUG: stranger named PROPFIND leaked stored property:\n%s", body)
	}
	if strings.Contains(body, "HTTP/1.1 200 OK") {
		t.Errorf("stranger PROPFIND should not contain any 200 OK propstat:\n%s", body)
	}
}

func storeHas(store *mock.MemoryPropStorage, name xml.Name, want string) bool {
	for _, props := range store.Props {
		if v, ok := props[name].(string); ok && v == want {
			return true
		}
	}
	return false
}
