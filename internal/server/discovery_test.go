package server_test

// Replays the discovery request sequences real CalDAV clients make (RFC 6764
// bootstrapping as done by Apple Calendar and DAVx5) against a mock-backed
// server with the production plugin stack, asserting the responses carry the
// properties clients depend on.

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	"calserver/internal/server"
)

type xmlName = xml.Name

func testHandler() *dav.Server {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/admin"},
	}}
	backend := &caldav.MockBackend{
		Calendars: []caldav.CalendarInfo{{
			ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/admin",
			Props: map[xmlName]any{
				{Space: "DAV:", Local: "displayname"}:                      "default calendar",
				{Space: "http://sabredav.org/ns", Local: "sync-token"}:     "5",
				{Space: "http://calendarserver.org/ns/", Local: "getctag"}: "http://sabre.io/ns/sync/5",
			},
		}},
	}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))

	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/admin"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&dav.SyncPlugin{})
	return srv
}

func propfind(t *testing.T, srv *dav.Server, path, depth, body string) string {
	t.Helper()
	req := httptest.NewRequest("PROPFIND", path, strings.NewReader(body))
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 207 {
		t.Fatalf("PROPFIND %s = %d: %s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// TestDiscoveryFlow walks the client bootstrap: root → principal → calendar
// home → calendar, checking each hop's required properties.
func TestDiscoveryFlow(t *testing.T) {
	srv := testHandler()

	// Hop 1: current-user-principal on the root.
	body := propfind(t, srv, "/", "0",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:current-user-principal/></d:prop></d:propfind>`)
	if !strings.Contains(body, "<d:href>/principals/admin/</d:href>") {
		t.Errorf("current-user-principal missing:\n%s", body)
	}

	// Hop 2: calendar-home-set + principal-URL on the principal.
	body = propfind(t, srv, "/principals/admin/", "0",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-home-set/><d:principal-URL/><d:displayname/></d:prop></d:propfind>`)
	if !strings.Contains(body, "<d:href>/calendars/admin/</d:href>") {
		t.Errorf("calendar-home-set missing:\n%s", body)
	}

	// Hop 3: calendar listing with the properties DAVx5/Apple request.
	body = propfind(t, srv, "/calendars/admin/", "1",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:cs="http://calendarserver.org/ns/">
		 <d:prop><d:resourcetype/><d:displayname/><d:current-user-privilege-set/><d:supported-report-set/><cs:getctag/><d:sync-token/><c:supported-calendar-component-set/></d:prop></d:propfind>`)
	for _, want := range []string{
		"<cal:calendar/>",                      // resourcetype marks the calendar
		"default calendar",                     // displayname
		"http://sabre.io/ns/sync/5",            // ctag + sync-token format
		"<d:sync-collection/>",                 // supported-report-set: sync
		"<cal:calendar-multiget/>",             // supported-report-set: multiget
		"<cal:calendar-query/>",                // supported-report-set: query
		"<d:privilege><d:read/></d:privilege>", // current-user-privilege-set
		"<d:privilege><d:write/></d:privilege>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("calendar listing missing %q:\n%s", want, body)
		}
	}
}

func TestWellKnownRedirect(t *testing.T) {
	h := server.WrapWellKnown(testHandler(), "/")
	req := httptest.NewRequest("PROPFIND", "/.well-known/caldav", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("well-known status = %d, want 301", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want /", got)
	}
}

// Both well-known names redirect, and every other path reaches the DAV server
// as sent: a doubled slash is normalized there, not bounced as a redirect.
func TestWellKnownWrapperPassesOtherPathsThrough(t *testing.T) {
	h := server.WrapWellKnown(testHandler(), "/")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("PROPFIND", "/.well-known/carddav", nil))
	if w.Code != http.StatusMovedPermanently {
		t.Errorf("carddav well-known = %d, want 301", w.Code)
	}

	req := httptest.NewRequest("PROPFIND", "/calendars//", strings.NewReader(
		`<propfind xmlns="DAV:"><prop><resourcetype/></prop></propfind>`))
	req.Header.Set("Depth", "0")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMultiStatus {
		t.Errorf("PROPFIND /calendars// = %d, want 207: %s", w.Code, w.Body.String())
	}
}
