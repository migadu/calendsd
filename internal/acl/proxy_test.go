package acl_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
)

func proxyServer(t *testing.T, backend *acl.MemoryBackend, loginAs string) *dav.Server {
	t.Helper()
	root := mock.NewCollection("root")
	pc := acl.NewPrincipalCollection(backend, "principals")
	pc.Proxies = true
	root.Attach(pc)
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin(loginAs))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	return srv
}

func do(t *testing.T, srv *dav.Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// Calendar-proxy delegation: a user principal exposes calendar-proxy-write, a
// PROPPATCH of group-member-set persists the delegate, and the delegate then
// belongs to the proxy group (which is what grants access to the owner's
// calendars).
func TestCalendarProxyDelegation(t *testing.T) {
	backend := &acl.MemoryBackend{
		Principals: []acl.Principal{
			{URI: "principals/user1"},
			{URI: "principals/user2"},
			{URI: "principals/user1/calendar-proxy-read"},
			{URI: "principals/user1/calendar-proxy-write"},
		},
		Groups: map[string][]string{},
	}
	srv := proxyServer(t, backend, "principals/user1")

	// Discovery: the proxy principal resolves and reports both principal and
	// calendar-proxy-write resource types.
	rec := do(t, srv, "PROPFIND", "/principals/user1/calendar-proxy-write/",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/></d:prop></d:propfind>`)
	if rec.Code != 207 {
		t.Fatalf("PROPFIND proxy node = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "calendar-proxy-write") || !strings.Contains(body, "principal") {
		t.Errorf("proxy resourcetype missing principal/calendar-proxy-write:\n%s", body)
	}

	// Configuration: grant user2 write-proxy access to user1's calendars.
	rec = do(t, srv, "PROPPATCH", "/principals/user1/calendar-proxy-write/",
		`<?xml version="1.0"?><d:propertyupdate xmlns:d="DAV:"><d:set><d:prop>`+
			`<d:group-member-set><d:href>/principals/user2/</d:href></d:group-member-set>`+
			`</d:prop></d:set></d:propertyupdate>`)
	if rec.Code != 207 || strings.Contains(rec.Body.String(), "403") {
		t.Fatalf("PROPPATCH group-member-set = %d: %s", rec.Code, rec.Body.String())
	}

	// Persistence + delegation link: user2 is now a member of the proxy group.
	members := backend.Groups["principals/user1/calendar-proxy-write"]
	if len(members) != 1 || members[0] != "principals/user2" {
		t.Errorf("group-member-set = %v, want [principals/user2]", members)
	}
	groups, _ := backend.GroupMembership("principals/user2")
	if !contains(groups, "principals/user1/calendar-proxy-write") {
		t.Errorf("user2 group membership %v missing the proxy group", groups)
	}
}

// calendar-proxy-write-for lists the principals a user may proxy for.
func TestCalendarProxyWriteFor(t *testing.T) {
	backend := &acl.MemoryBackend{
		Principals: []acl.Principal{{URI: "principals/user1"}, {URI: "principals/user2"}},
		Groups:     map[string][]string{"principals/user1/calendar-proxy-write": {"principals/user2"}},
	}
	srv := proxyServer(t, backend, "principals/user2")
	rec := do(t, srv, "PROPFIND", "/principals/user2/",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><d:prop><cs:calendar-proxy-write-for/></d:prop></d:propfind>`)
	if rec.Code != 207 {
		t.Fatalf("PROPFIND = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "/principals/user1/") {
		t.Errorf("calendar-proxy-write-for missing user1:\n%s", body)
	}
}

// With proxies disabled a principal stays a leaf (no {DAV:}collection), matching
// plain DAVACL principals — the behavior the corpus depends on.
func TestPrincipalLeafWithoutProxies(t *testing.T) {
	backend := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/user1"}}}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(backend, "principals")) // Proxies default false
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})

	rec := do(t, srv, "PROPFIND", "/principals/user1/",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/></d:prop></d:propfind>`)
	if body := rec.Body.String(); strings.Contains(body, "collection") {
		t.Errorf("plain principal wrongly reports {DAV:}collection:\n%s", body)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
