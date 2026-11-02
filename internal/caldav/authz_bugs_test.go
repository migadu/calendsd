package caldav_test

import (
	"context"
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/auth"
	"github.com/migadu/calendsd/internal/caldav"
	"github.com/migadu/calendsd/internal/dav"
	"github.com/migadu/calendsd/internal/dav/mock"
)

// These tests pin three confirmed authorization bugs in internal/caldav. Each
// asserts the CORRECT (secure) behavior, so it FAILS against the current code
// and passes once the bug is fixed. They must not be weakened to pass.

// ---------------------------------------------------------------------------
// Bug 1: scheduling.go:112-140 — SchedulingObject (an inbox message) implements
// neither acl.ACLHolder nor Owner(). In acl/privileges.go nodeACL() (line 80-86)
// a node without an ACLHolder falls back to defaultACL (privileges.go:76),
// which is {DAV:}all for {DAV:}authenticated. So ANY logged-in principal can
// GET another principal's inbox scheduling messages (which carry iTIP
// invite/reply payloads: attendee lists, free/busy, meeting details).
//
// Expected: a GET of user1's inbox object by user2 is denied (403, or 404 if
// the object is hidden). Actual: 200 with the iTIP body.
// ---------------------------------------------------------------------------
func TestBugInboxSchedulingObjectReadableByStranger(t *testing.T) {
	srv, backend := twoPrincipalServer("principals/user2")

	// user1 has a REPLY sitting in their schedule inbox.
	msg := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\nMETHOD:REPLY\r\n" +
		"BEGIN:VEVENT\r\nUID:secret-invite\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:Private Board Meeting\r\n" +
		"ORGANIZER:mailto:user1@example.org\r\nATTENDEE:mailto:boss@example.org\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	backend.Scheduling = map[string]map[string][]byte{
		"principals/user1": {"msg1.ics": []byte(msg)},
	}

	get := httptest.NewRequest("GET", "/calendars/user1/inbox/msg1.ics", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, get)

	if w.Code == 200 {
		t.Fatalf("stranger (user2) read user1's inbox message: GET = 200 with body:\n%s\nwant 403/404",
			w.Body.String())
	}
	if w.Code != 403 && w.Code != 404 {
		t.Fatalf("stranger GET of user1 inbox message = %d, want 403 or 404:\n%s", w.Code, w.Body.String())
	}
	// And the private meeting details must not leak in the body regardless.
	if strings.Contains(w.Body.String(), "Private Board Meeting") {
		t.Fatalf("inbox iTIP payload leaked to stranger:\n%s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Bug 2: notifications.go:110-126 — NotificationNode implements neither
// acl.ACLHolder nor Owner(). As in bug 1, nodeACL() falls back to defaultACL
// (all-for-authenticated), so any logged-in principal can GET another
// principal's notification items — including pending share-invitation XML,
// which discloses who shared what calendar with whom.
//
// The parent Notifications collection carries an owner-only ACL
// (notifications.go:106-108) AND an Owner() (line 65), but the individual
// NotificationNode child does not, so a direct GET of the item bypasses it.
//
// user1 has a pending invite notification. user2 (a stranger to user1's
// notifications) GETs it. Expected: 403/404. Actual: 200 with the invite XML.
// ---------------------------------------------------------------------------
func TestBugNotificationItemReadableByStranger(t *testing.T) {
	// Build a two-principal server whose backend implements NotificationsBackend,
	// so /calendars/<p>/notifications/ is exposed. MockSharingBackend embeds
	// MemoryNotificationsBackend, giving us that plus the SharingPlugin wiring.
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1"}, {URI: "principals/user2"},
	}}
	backend := &caldav.MockSharingBackend{Shares: map[caldav.CalendarID][]caldav.Sharee{}}
	backend.Calendars = []caldav.CalendarInfo{{
		ID: caldav.CalendarID{Calendar: "1"}, URI: "cal1", PrincipalURI: "principals/user1",
	}}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{}

	// Seed a pending share-invite notification directly into user1's inbox.
	invite := caldav.InviteNotification{
		ID:           "notif-secret-1",
		HostURL:      "calendars/user1/cal1",
		Organizer:    "principals/admin",
		Access:       caldav.AccessReadWrite,
		InviteStatus: caldav.InviteNoResponse,
		Summary:      "Confidential shared calendar",
	}
	if err := backend.AddNotification(t.Context(), "principals/user1", invite); err != nil {
		t.Fatalf("seed notification: %v", err)
	}

	build := func(actor string) *dav.Server {
		root := mock.NewCollection("root")
		root.Attach(acl.NewPrincipalCollection(principals, "principals"))
		root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
		srv := dav.NewServer("/", root)
		srv.AddPlugin(auth.NewLoggedInPlugin(actor))
		srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
		srv.AddPlugin(&caldav.Plugin{})
		srv.AddPlugin(&caldav.SharingPlugin{})
		srv.AddPlugin(&caldav.NotificationsPlugin{})
		return srv
	}

	// The notification file name is "<id>.xml" (notifications.go:118).
	url := "/calendars/user1/notifications/notif-secret-1.xml"

	// Sanity: the owner (user1) CAN read it, proving the URL is correct.
	owner := build("principals/user1")
	wOwner := httptest.NewRecorder()
	owner.ServeHTTP(wOwner, httptest.NewRequest("GET", url, nil))
	if wOwner.Code != 200 {
		t.Fatalf("owner GET of own notification = %d, want 200 (URL wrong?):\n%s",
			wOwner.Code, wOwner.Body.String())
	}

	// The attack: user2 (a stranger) reads user1's notification.
	stranger := build("principals/user2")
	w := httptest.NewRecorder()
	stranger.ServeHTTP(w, httptest.NewRequest("GET", url, nil))

	if w.Code == 200 {
		t.Fatalf("stranger (user2) read user1's share-invite notification: GET = 200:\n%s\nwant 403/404",
			w.Body.String())
	}
	if w.Code != 403 && w.Code != 404 {
		t.Fatalf("stranger GET of user1 notification = %d, want 403 or 404:\n%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "Confidential shared calendar") {
		t.Fatalf("notification invite XML leaked to stranger:\n%s", w.Body.String())
	}
}

// recordingPublishBackend wraps MockSharingBackend to record whether the
// publish state of a calendar was actually mutated, so we can assert that an
// unauthorized POST changed nothing.
type recordingPublishBackend struct {
	caldav.MockSharingBackend
	publishCalls []publishCall
}

type publishCall struct {
	id    caldav.CalendarID
	value bool
}

func (b *recordingPublishBackend) SetPublishStatus(ctx context.Context, id caldav.CalendarID, value bool) error {
	b.publishCalls = append(b.publishCalls, publishCall{id: id, value: value})
	return nil
}

// ---------------------------------------------------------------------------
// Bug 3: sharing.go:147-162,229-280 — SharingPlugin.HandleMethod dispatches the
// publish-calendar / unpublish-calendar / invite-reply POST actions with NO
// authorization check. POST is not in acl.Plugin.BeforeMethod's table
// (privileges.go:222-277), and unlike shareRequest() (which explicitly checks
// {DAV:}share, sharing.go:170-178) the publish()/inviteReply() paths call the
// backend directly. So a non-owner can publish (make world-discoverable) or
// unpublish another principal's calendar.
//
// user2 (a non-owner) POSTs a {cs}publish-calendar to user1's calendar.
// Expected: 403 and NO SetPublishStatus mutation. Actual: 202 and the publish
// state is flipped.
// ---------------------------------------------------------------------------
func TestBugStrangerCanPublishCalendar(t *testing.T) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1"}, {URI: "principals/user2"},
	}}
	backend := &recordingPublishBackend{}
	backend.Shares = map[caldav.CalendarID][]caldav.Sharee{}
	backend.Calendars = []caldav.CalendarInfo{{
		ID: caldav.CalendarID{Calendar: "1"}, URI: "cal1", PrincipalURI: "principals/user1",
	}}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user2")) // the attacker
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&caldav.SharingPlugin{})

	pub := `<?xml version="1.0"?>` +
		`<cs:publish-calendar xmlns:cs="http://calendarserver.org/ns/"/>`
	post := httptest.NewRequest("POST", "/calendars/user1/cal1", strings.NewReader(pub))
	post.Header.Set("Content-Type", "text/xml")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post)

	if w.Code != 403 {
		t.Fatalf("stranger publish-calendar POST = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if len(backend.publishCalls) != 0 {
		t.Fatalf("stranger publish-calendar mutated publish state: %+v; want no SetPublishStatus call",
			backend.publishCalls)
	}
}

// A read-only sharee must not reach UpdateInvites through their own view of a
// shared calendar: {DAV:}share is granted only on the owner's view, keyed off
// ShareAccess (S3/S10 in docs/review-fixes.md).
func TestReadOnlyShareeCannotReshare(t *testing.T) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1"}, {URI: "principals/user2"},
	}}
	backend := &caldav.MockSharingBackend{}
	backend.Calendars = []caldav.CalendarInfo{{
		ID: caldav.CalendarID{Calendar: "1", Instance: "2"}, URI: "shared", PrincipalURI: "principals/user2",
		ShareAccess: caldav.AccessRead,
		Props:       map[xml.Name]any{{Space: "http://sabredav.org/ns", Local: "read-only"}: "1"},
	}}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user2"))
	srv.AddPlugin(&acl.Plugin{})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&caldav.SharingPlugin{})

	share := `<?xml version="1.0"?><cs:share xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/">` +
		`<cs:set><d:href>principals/user2</d:href><cs:read-write/></cs:set></cs:share>`
	post := httptest.NewRequest("POST", "/calendars/user2/shared", strings.NewReader(share))
	post.Header.Set("Content-Type", "text/xml")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post)

	if w.Code != 403 {
		t.Fatalf("read-only sharee cs:share POST = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if len(backend.Shares) != 0 {
		t.Fatalf("read-only sharee changed the invites: %+v", backend.Shares)
	}
}
