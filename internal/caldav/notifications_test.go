package caldav_test

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
)

// sharedSharing builds two servers (one per actor) over one shared backend,
// so the sharer and the sharee act against the same state.
func sharedSharing() (backend *caldav.MockSharingBackend, servers map[string]*dav.Server) {
	return sharedSharingBase("/")
}

// sharedSharingBase is sharedSharing with an explicit server BaseURI, so tests
// can exercise non-root deployments (where href construction must include the
// prefix).
func sharedSharingBase(base string) (backend *caldav.MockSharingBackend, servers map[string]*dav.Server) {
	dn := xmlNM{Space: "DAV:", Local: "displayname"}
	email := xmlNM{Space: "http://sabredav.org/ns", Local: "email-address"}
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1", Props: map[xmlNM]any{dn: "User One", email: "user1@example.org"}},
		{URI: "principals/user2", Props: map[xmlNM]any{dn: "User Two", email: "user2@example.org"}},
	}}
	backend = &caldav.MockSharingBackend{Shares: map[caldav.CalendarID][]caldav.Sharee{}}
	backend.Calendars = []caldav.CalendarInfo{{
		ID: caldav.CalendarID{Calendar: "1"}, URI: "cal1", PrincipalURI: "principals/user1",
	}}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{}

	build := func(actor string) *dav.Server {
		root := mock.NewCollection("root")
		root.Attach(acl.NewPrincipalCollection(principals, "principals"))
		root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
		srv := dav.NewServer(base, root)
		srv.AddPlugin(auth.NewLoggedInPlugin(actor))
		srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
		srv.AddPlugin(&caldav.Plugin{})
		srv.AddPlugin(&caldav.SharingPlugin{})
		srv.AddPlugin(&caldav.NotificationsPlugin{})
		return srv
	}
	return backend, map[string]*dav.Server{
		"principals/user1": build("principals/user1"),
		"principals/user2": build("principals/user2"),
	}
}

func req(t *testing.T, srv *dav.Server, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w
}

// TestSharingNotificationRoundTrip: user1 shares a calendar with user2; the
// invite must appear in user2's notification collection and be retrievable.
func TestSharingNotificationRoundTrip(t *testing.T) {
	_, servers := sharedSharing()
	user1, user2 := servers["principals/user1"], servers["principals/user2"]

	share := `<?xml version="1.0"?><cs:share xmlns:cs="http://calendarserver.org/ns/" xmlns:d="DAV:">
	  <cs:set><d:href>mailto:user2@example.org</d:href><cs:common-name>User Two</cs:common-name><cs:read-write/><cs:summary>Join my calendar</cs:summary></cs:set>
	</cs:share>`
	w := req(t, user1, "POST", "/calendars/user1/cal1", share, "Content-Type", "text/xml")
	if w.Code != 200 {
		t.Fatalf("share POST = %d: %s", w.Code, w.Body.String())
	}

	// user2 discovers the notification-URL.
	w = req(t, user2, "PROPFIND", "/principals/user2/",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><d:prop><cs:notification-URL/></d:prop></d:propfind>`,
		"Depth", "0")
	if !strings.Contains(w.Body.String(), "/calendars/user2/notifications/") {
		t.Fatalf("notification-URL missing:\n%s", w.Body.String())
	}

	// user2 lists their notifications (Depth 1).
	w = req(t, user2, "PROPFIND", "/calendars/user2/notifications/",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getcontenttype/></d:prop></d:propfind>`,
		"Depth", "1")
	body := w.Body.String()
	// One child notification (an .xml item) should be listed.
	if strings.Count(body, "<d:response>") < 2 { // collection + 1 item
		t.Fatalf("notification not listed in the inbox:\n%s", body)
	}

	// Fetch the notification item (the .xml child) and verify it's the
	// invite.
	xmlIdx := strings.Index(body, ".xml</d:href>")
	if xmlIdx < 0 {
		t.Fatalf("no notification item href:\n%s", body)
	}
	start := strings.LastIndex(body[:xmlIdx], "<d:href>") + len("<d:href>")
	href := body[start : xmlIdx+len(".xml")]
	w = req(t, user2, "GET", href, "")
	item := w.Body.String()
	if w.Code != 200 || !strings.Contains(item, "invite-notification") {
		t.Fatalf("notification GET = %d:\n%s", w.Code, item)
	}
	for _, want := range []string{`<read-write xmlns="http://calendarserver.org/ns/">`, "Join my calendar", "/calendars/user1/cal1", "principals/user1"} {
		if !strings.Contains(item, want) {
			t.Errorf("notification body missing %q:\n%s", want, item)
		}
	}
}

// TestShareNotificationsFollowCommittedResult: a set overridden by a remove
// in the same POST shares nothing, so it must not notify; a sharee named
// twice is notified once, with the access that was committed.
func TestShareNotificationsFollowCommittedResult(t *testing.T) {
	backend, servers := sharedSharing()
	user1 := servers["principals/user1"]

	share := `<?xml version="1.0"?><cs:share xmlns:cs="http://calendarserver.org/ns/" xmlns:d="DAV:">
	  <cs:set><d:href>mailto:user2@example.org</d:href></cs:set>
	  <cs:remove><d:href>mailto:user2@example.org</d:href></cs:remove>
	</cs:share>`
	if w := req(t, user1, "POST", "/calendars/user1/cal1", share, "Content-Type", "text/xml"); w.Code != 200 {
		t.Fatalf("share POST = %d: %s", w.Code, w.Body.String())
	}
	if n := backend.Notifications["principals/user2"]; len(n) != 0 {
		t.Fatalf("set+remove left %d notifications, want 0", len(n))
	}

	share = `<?xml version="1.0"?><cs:share xmlns:cs="http://calendarserver.org/ns/" xmlns:d="DAV:">
	  <cs:set><d:href>mailto:user2@example.org</d:href></cs:set>
	  <cs:set><d:href>MAILTO:User2@Example.org</d:href><cs:read-write/></cs:set>
	</cs:share>`
	if w := req(t, user1, "POST", "/calendars/user1/cal1", share, "Content-Type", "text/xml"); w.Code != 200 {
		t.Fatalf("share POST = %d: %s", w.Code, w.Body.String())
	}
	n := backend.Notifications["principals/user2"]
	if len(n) != 1 || n[0].Access != caldav.AccessReadWrite {
		t.Fatalf("notifications = %+v, want one read-write invite", n)
	}
}

// TestShareNotificationClock: the invite's DTSTAMP comes from the plugin's
// clock; a client cannot choose it with a request header.
func TestShareNotificationClock(t *testing.T) {
	email := xmlNM{Space: "http://sabredav.org/ns", Local: "email-address"}
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1", Props: map[xmlNM]any{email: "user1@example.org"}},
		{URI: "principals/user2", Props: map[xmlNM]any{email: "user2@example.org"}},
	}}
	backend := &caldav.MockSharingBackend{}
	backend.Calendars = []caldav.CalendarInfo{{
		ID: caldav.CalendarID{Calendar: "1"}, URI: "cal1", PrincipalURI: "principals/user1",
	}}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	now := time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC)
	srv.AddPlugin(&caldav.SharingPlugin{Now: func() time.Time { return now }})

	share := `<?xml version="1.0"?><cs:share xmlns:cs="http://calendarserver.org/ns/" xmlns:d="DAV:">
	  <cs:set><d:href>mailto:user2@example.org</d:href></cs:set>
	</cs:share>`
	w := req(t, srv, "POST", "/calendars/user1/cal1", share,
		"Content-Type", "text/xml", "X-Test-Now", "2001-01-01T00:00:00Z")
	if w.Code != 200 {
		t.Fatalf("share POST = %d: %s", w.Code, w.Body.String())
	}
	n := backend.Notifications["principals/user2"]
	if len(n) != 1 || !n[0].DTStamp.Equal(now) {
		t.Fatalf("notifications = %+v, want one stamped %v", n, now)
	}
}

// resolvingPrincipals is a principal backend with an indexed address lookup;
// it counts full listings, which the lookup must make unnecessary.
type resolvingPrincipals struct {
	*acl.MemoryBackend
	listings, lookups int
}

func (b *resolvingPrincipals) PrincipalsByPrefix(ctx context.Context, prefix string) ([]acl.Principal, error) {
	b.listings++
	return b.MemoryBackend.PrincipalsByPrefix(ctx, prefix)
}

func (b *resolvingPrincipals) PrincipalByAddress(ctx context.Context, prefix, address string) (string, error) {
	b.lookups++
	if prefix == "principals" && strings.EqualFold(address, "mailto:user2@example.org") {
		return "principals/user2", nil
	}
	return "", nil
}

// TestShareResolvesThroughAddressResolver: a principal backend with an
// address lookup resolves sharees without listing every principal.
func TestShareResolvesThroughAddressResolver(t *testing.T) {
	email := xmlNM{Space: "http://sabredav.org/ns", Local: "email-address"}
	principals := &resolvingPrincipals{MemoryBackend: &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1", Props: map[xmlNM]any{email: "user1@example.org"}},
		{URI: "principals/user2", Props: map[xmlNM]any{email: "user2@example.org"}},
	}}}
	backend := &caldav.MockSharingBackend{}
	backend.Calendars = []caldav.CalendarInfo{{
		ID: caldav.CalendarID{Calendar: "1"}, URI: "cal1", PrincipalURI: "principals/user1",
	}}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&caldav.SharingPlugin{})

	share := `<?xml version="1.0"?><cs:share xmlns:cs="http://calendarserver.org/ns/" xmlns:d="DAV:">
	  <cs:set><d:href>mailto:User2@example.org</d:href></cs:set>
	</cs:share>`
	if w := req(t, srv, "POST", "/calendars/user1/cal1", share, "Content-Type", "text/xml"); w.Code != 200 {
		t.Fatalf("share POST = %d: %s", w.Code, w.Body.String())
	}
	if got := backend.Shares[caldav.CalendarID{Calendar: "1"}]; len(got) != 1 || got[0].Principal != "principals/user2" {
		t.Fatalf("shares = %+v, want user2 resolved", got)
	}
	if principals.lookups == 0 || principals.listings != 0 {
		t.Errorf("lookups = %d, listings = %d; want the lookup and no listing", principals.lookups, principals.listings)
	}
}

// TestInvitePropertyListsSharees: after sharing a calendar read-write with one
// person and read-only with another, a PROPFIND of {cs}invite on the owner's
// calendar must return a <cs:user> per sharee with the right access level, so
// the owner's client can show who has access.
func TestInvitePropertyListsSharees(t *testing.T) {
	_, servers := sharedSharing()
	user1 := servers["principals/user1"]

	share := `<?xml version="1.0"?><cs:share xmlns:cs="http://calendarserver.org/ns/" xmlns:d="DAV:">
	  <cs:set><d:href>mailto:user2@example.org</d:href><cs:common-name>User Two</cs:common-name><cs:read-write/></cs:set>
	  <cs:set><d:href>mailto:guest@example.org</d:href></cs:set>
	</cs:share>`
	if w := req(t, user1, "POST", "/calendars/user1/cal1", share, "Content-Type", "text/xml"); w.Code != 200 {
		t.Fatalf("share POST = %d: %s", w.Code, w.Body.String())
	}

	w := req(t, user1, "PROPFIND", "/calendars/user1/cal1",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><d:prop><cs:invite/></d:prop></d:propfind>`,
		"Depth", "0")
	body := w.Body.String()
	if w.Code != 207 {
		t.Fatalf("invite PROPFIND = %d:\n%s", w.Code, body)
	}
	for _, want := range []string{
		"<cs:invite>", "mailto:user2@example.org", "User Two",
		`<read-write xmlns="http://calendarserver.org/ns/">`,
		"mailto:guest@example.org", `<read xmlns="http://calendarserver.org/ns/">`,
		`<invite-noresponse xmlns="http://calendarserver.org/ns/">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("invite property missing %q:\n%s", want, body)
		}
	}
	if strings.Count(body, `<user xmlns="http://calendarserver.org/ns/">`) != 2 {
		t.Errorf("want 2 {cs}user entries, got:\n%s", body)
	}
}

// TestInvitePropertyIsWellFormed: a stored sharee name with characters XML
// cannot carry still yields a parseable multistatus.
func TestInvitePropertyIsWellFormed(t *testing.T) {
	backend, servers := sharedSharing()
	backend.Shares[caldav.CalendarID{Calendar: "1"}] = []caldav.Sharee{{
		Href: "mailto:x@example.org", CommonName: "A & <B>\x01", Access: caldav.AccessRead,
	}}
	w := req(t, servers["principals/user1"], "PROPFIND", "/calendars/user1/cal1",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><d:prop><cs:invite/></d:prop></d:propfind>`,
		"Depth", "0")
	if w.Code != 207 {
		t.Fatalf("invite PROPFIND = %d:\n%s", w.Code, w.Body.String())
	}
	dec := xml.NewDecoder(strings.NewReader(w.Body.String()))
	for {
		if _, err := dec.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("multistatus is not well-formed: %v\n%s", err, w.Body.String())
		}
	}
}

// bodyReader is a POST handler registered after the sharing plugin; it
// records the body it gets.
type bodyReader struct{ got string }

func (b *bodyReader) PluginName() string                                { return "body-reader" }
func (b *bodyReader) Init(*dav.Server)                                  {}
func (b *bodyReader) Methods(ctx context.Context, path string) []string { return []string{"POST"} }
func (b *bodyReader) HandleMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	raw, err := io.ReadAll(r.Body)
	b.got = string(raw)
	w.WriteHeader(http.StatusNoContent)
	return true, err
}

// TestSharingLeavesOtherPostBodies: an XML POST to a calendar that is not a
// sharing document reaches the next plugin with its body intact.
func TestSharingLeavesOtherPostBodies(t *testing.T) {
	_, servers := sharedSharing()
	srv := servers["principals/user1"]
	next := &bodyReader{}
	srv.AddPlugin(next)

	body := `<?xml version="1.0"?><x:other xmlns:x="urn:example"/>`
	w := req(t, srv, "POST", "/calendars/user1/cal1", body, "Content-Type", "application/xml")
	if w.Code != http.StatusNoContent || next.got != body {
		t.Fatalf("POST = %d, next plugin got %q; want 204 and the full body", w.Code, next.got)
	}
}

// TestInviteReplySharedAsBaseURI: an accepted invite-reply's <cs:shared-as>
// href must carry the server's BaseURI under a non-root deployment. The
// backend returns a request-relative path, so resolveHref joins it onto the
// base-carrying request URL (/dav/calendars/user2/) — a leading-slash return
// would drop the prefix.
func TestInviteReplySharedAsBaseURI(t *testing.T) {
	_, servers := sharedSharingBase("/dav/")
	user2 := servers["principals/user2"]

	reply := `<?xml version="1.0"?><cs:invite-reply xmlns:cs="http://calendarserver.org/ns/" xmlns:d="DAV:">
	  <cs:hosturl><d:href>/dav/calendars/user1/cal1</d:href></cs:hosturl>
	  <cs:invite-accepted/>
	</cs:invite-reply>`
	w := req(t, user2, "POST", "/dav/calendars/user2/", reply, "Content-Type", "application/xml")
	if w.Code != 200 {
		t.Fatalf("invite-reply POST = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The mock returns "calendars/blabla/calendar", resolved against the
	// request path /dav/calendars/user2/ — so the href must start with /dav/.
	if !strings.Contains(body, `<href xmlns="DAV:">/dav/calendars/user2/calendars/blabla/calendar</href>`) {
		t.Errorf("shared-as href did not carry the BaseURI:\n%s", body)
	}
}
