package acl_test

import (
	"encoding/xml"
	"strings"
	"testing"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	davxml "calserver/internal/dav/xml"
)

// enumServer builds a server with three users across two email domains plus an
// admin, each principal owning a calendar home. Authenticated as authAs.
//
//	user1@a.example, user2@a.example   — same domain
//	user3@b.example                    — foreign domain
//	admin (no email)                   — bypasses domain scoping
func enumServer(authAs string) *dav.Server {
	sd := "http://sabredav.org/ns"
	email := func(a string) map[xml.Name]any {
		return map[xml.Name]any{
			{Space: sd, Local: "email-address"}:         a,
			{Space: davxml.NSDav, Local: "displayname"}: a, // searchable by substring
		}
	}
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1", Props: email("user1@a.example")},
		{URI: "principals/user2", Props: email("user2@a.example")},
		{URI: "principals/user3", Props: email("user3@b.example")},
		{URI: "principals/admin"},
	}}
	backend := &caldav.MockBackend{Calendars: []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "cal", PrincipalURI: "principals/user1"},
		{ID: caldav.CalendarID{Calendar: "2"}, URI: "cal", PrincipalURI: "principals/user2"},
		{ID: caldav.CalendarID{Calendar: "3"}, URI: "cal", PrincipalURI: "principals/user3"},
	}}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin(authAs))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	return srv
}

// TestPrincipalListingScopedToDomain: a Depth:1 PROPFIND of /principals/ lists
// only same-domain principals (plus self), never a foreign-domain user's href.
func TestPrincipalListingScopedToDomain(t *testing.T) {
	body := propfind(t, enumServer("principals/user1"), "/principals/", "", "1").Body.String()

	for _, want := range []string{"principals/user1", "principals/user2"} {
		if !strings.Contains(body, want) {
			t.Errorf("principal listing missing same-domain %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "principals/user3") {
		t.Errorf("principal listing leaked foreign-domain user3:\n%s", body)
	}
}

// TestCalendarHomeListingScopedToDomain: the same scoping hides foreign-domain
// calendar homes under /calendars/.
func TestCalendarHomeListingScopedToDomain(t *testing.T) {
	body := propfind(t, enumServer("principals/user1"), "/calendars/", "", "1").Body.String()

	if !strings.Contains(body, "calendars/user2") {
		t.Errorf("calendar home listing missing same-domain user2:\n%s", body)
	}
	if strings.Contains(body, "calendars/user3") {
		t.Errorf("calendar home listing leaked foreign-domain user3:\n%s", body)
	}
}

// TestAdminSeesAllDomains: an admin principal bypasses domain scoping.
func TestAdminSeesAllDomains(t *testing.T) {
	body := propfind(t, enumServer("principals/admin"), "/principals/", "", "1").Body.String()
	for _, want := range []string{"principals/user1", "principals/user2", "principals/user3"} {
		if !strings.Contains(body, want) {
			t.Errorf("admin listing missing %s (scoping must not apply to admins):\n%s", want, body)
		}
	}
}

// TestPrincipalSearchScopedToDomain: principal-property-search (the attendee
// autocomplete channel) only returns same-domain matches. user1 searches for
// the substring ".example" that every principal's displayname contains, but
// must only get same-domain hits back.
func TestPrincipalSearchScopedToDomain(t *testing.T) {
	search := `<?xml version="1.0"?>` +
		`<d:principal-property-search xmlns:d="DAV:">` +
		`<d:property-search><d:prop><d:displayname/></d:prop><d:match>.example</d:match></d:property-search>` +
		`<d:prop><d:displayname/></d:prop>` +
		`</d:principal-property-search>`
	body := report(t, enumServer("principals/user1"), "/principals/", search, "Depth", "0").Body.String()

	if !strings.Contains(body, "principals/user2") {
		t.Errorf("directory search dropped same-domain user2:\n%s", body)
	}
	if strings.Contains(body, "principals/user3") || strings.Contains(body, "user3@b.example") {
		t.Errorf("directory search leaked foreign-domain user3:\n%s", body)
	}
}

// TestBrowserListingScopedToDomain: the HTML index goes through the same child
// filter as PROPFIND, so it cannot be used to enumerate foreign-domain users.
func TestBrowserListingScopedToDomain(t *testing.T) {
	srv := enumServer("principals/user1")
	srv.AddPlugin(&dav.BrowserPlugin{})
	for _, path := range []string{"/principals/", "/calendars/"} {
		w := request(t, srv, "GET", path, "")
		if w.Code != 200 {
			t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, path+"user2") {
			t.Errorf("GET %s missing same-domain user2:\n%s", path, body)
		}
		if strings.Contains(body, "user3") {
			t.Errorf("GET %s leaked foreign-domain user3:\n%s", path, body)
		}
	}
}
