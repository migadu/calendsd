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
	groups, _ := backend.GroupMembership(t.Context(), "principals/user2")
	if !contains(groups, "principals/user1/calendar-proxy-write") {
		t.Errorf("user2 group membership %v missing the proxy group", groups)
	}
}

// group-member-set hrefs resolve like any other href: the base URI is
// stripped, absolute URLs and percent-escapes are understood, the DAV
// namespace may carry any prefix, and an href outside the server fails.
func TestProxyGroupMemberSetResolvesHrefs(t *testing.T) {
	backend := &acl.MemoryBackend{
		Principals: []acl.Principal{{URI: "principals/user1"}, {URI: "principals/user2"}, {URI: "principals/user 3"}},
		Groups:     map[string][]string{},
	}
	root := mock.NewCollection("root")
	pc := acl.NewPrincipalCollection(backend, "principals")
	pc.Proxies = true
	root.Attach(pc)
	srv := dav.NewServer("/dav/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{})

	patch := func(hrefs string) *httptest.ResponseRecorder {
		return do(t, srv, "PROPPATCH", "/dav/principals/user1/calendar-proxy-write/",
			`<?xml version="1.0"?><x:propertyupdate xmlns:x="DAV:"><x:set><x:prop>`+
				`<x:group-member-set>`+hrefs+`</x:group-member-set>`+
				`</x:prop></x:set></x:propertyupdate>`)
	}
	rec := patch(`<x:href>/dav/principals/user2/</x:href><x:href> https://cal.example/dav/principals/user%203/ </x:href>`)
	if rec.Code != 207 || strings.Contains(rec.Body.String(), "403") {
		t.Fatalf("PROPPATCH = %d: %s", rec.Code, rec.Body.String())
	}
	members := backend.Groups["principals/user1/calendar-proxy-write"]
	if len(members) != 2 || members[0] != "principals/user2" || members[1] != "principals/user 3" {
		t.Errorf("group-member-set = %q, want [principals/user2 principals/user 3]", members)
	}

	rec = patch(`<x:href>/elsewhere/principals/user2/</x:href>`)
	if !strings.Contains(rec.Body.String(), "403") {
		t.Errorf("href outside the base was accepted: %s", rec.Body.String())
	}
	if members := backend.Groups["principals/user1/calendar-proxy-write"]; len(members) != 2 {
		t.Errorf("failed PROPPATCH changed group-member-set to %q", members)
	}
}

// principal-match on principal-property compares hrefs as resolved paths, so
// it works under a non-root base URI and with percent-encoded names.
func TestPrincipalMatchResolvesHrefs(t *testing.T) {
	backend := &acl.MemoryBackend{
		Principals: []acl.Principal{{URI: "principals/user 1"}, {URI: "principals/user2"}},
		Groups:     map[string][]string{"principals/user 1/calendar-proxy-write": {"principals/user2"}},
	}
	root := mock.NewCollection("root")
	pc := acl.NewPrincipalCollection(backend, "principals")
	pc.Proxies = true
	root.Attach(pc)
	srv := dav.NewServer("/dav/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user2"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/user2"}})

	rec := do(t, srv, "REPORT", "/dav/principals/user%201/",
		`<?xml version="1.0"?><d:principal-match xmlns:d="DAV:">`+
			`<d:principal-property><d:group-member-set/></d:principal-property>`+
			`<d:prop><d:group-member-set/></d:prop>`+
			`</d:principal-match>`)
	if rec.Code != 207 {
		t.Fatalf("REPORT = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<d:href>/dav/principals/user%201/calendar-proxy-write") ||
		!strings.Contains(body, "/dav/principals/user2/") {
		t.Errorf("principal-match missed the proxy group or its members:\n%s", body)
	}
	// RFC 4918: every href sits inside a {DAV:}response (sabre omitted it).
	if !strings.Contains(body, "<d:response>\n  <d:href>/dav/principals/user%201/calendar-proxy-write") {
		t.Errorf("principal-match responses are not wrapped in {DAV:}response:\n%s", body)
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

// Principal hrefs are percent-encoded: a name with a space must not produce
// a broken href in current-user-principal or principal-URL.
func TestPrincipalHrefsAreEncoded(t *testing.T) {
	backend := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/user 1"}}}
	srv := proxyServer(t, backend, "principals/user 1")
	rec := do(t, srv, "PROPFIND", "/principals/user%201/",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop>`+
			`<d:current-user-principal/><d:principal-URL/></d:prop></d:propfind>`)
	body := rec.Body.String()
	if strings.Contains(body, "user 1") || strings.Count(body, "<d:href>/principals/user%201/</d:href>") < 2 {
		t.Errorf("principal hrefs not encoded:\n%s", body)
	}
}
