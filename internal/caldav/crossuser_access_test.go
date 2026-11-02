package caldav_test

import (
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

// These tests lock in that the cross-user read paths which do NOT carry their
// own privilege check — the ?export GET handler and the calendar-multiget
// REPORT — are nonetheless gated, because the ACL plugin's BeforeMethod hook
// runs before any method/report handler and denies GET/REPORT without
// {DAV:}read on the target. A refactor that reorders BeforeMethod after
// dispatch, or that lets these handlers resolve foreign paths directly, would
// turn them into data-disclosure bugs; these tests fail if that happens.

// twoUserCalendarServer builds a server where user1 and user2 each own a
// "default" calendar; user1's holds one secret event. Authenticated as
// loggedIn.
func twoUserCalendarServer(loggedIn string) *dav.Server {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1"}, {URI: "principals/user2"},
	}}
	backend := &caldav.MockSchedulingBackend{}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1"},
		{ID: caldav.CalendarID{Calendar: "2"}, URI: "default", PrincipalURI: "principals/user2"},
	}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{
		{Calendar: "1"}: {"secret.ics": &caldav.ObjectInfo{URI: "secret.ics", CalendarData: []byte(
			"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
				"BEGIN:VEVENT\r\nUID:secret-evt\r\nDTSTAMP:20260101T000000Z\r\n" +
				"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:TopSecret Meeting\r\n" +
				"END:VEVENT\r\nEND:VCALENDAR\r\n")}},
	}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin(loggedIn))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&caldav.SchedulePlugin{})
	return srv
}

// TestCrossUserExportDenied: a stranger GET ?export of another user's calendar
// is refused and leaks no event data.
func TestCrossUserExportDenied(t *testing.T) {
	srv := twoUserCalendarServer("principals/user2")

	get := httptest.NewRequest("GET", "/calendars/user1/default/?export", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, get)

	if w.Code != 403 {
		t.Fatalf("stranger ?export = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "TopSecret") {
		t.Fatalf("?export leaked event data to stranger:\n%s", w.Body.String())
	}
}

// TestOwnerExportAllowed is the control: the gate must not break the owner's
// own export.
func TestOwnerExportAllowed(t *testing.T) {
	srv := twoUserCalendarServer("principals/user1")

	get := httptest.NewRequest("GET", "/calendars/user1/default/?export", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, get)

	if w.Code != 200 {
		t.Fatalf("owner ?export = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "TopSecret Meeting") {
		t.Fatalf("owner ?export missing own event:\n%s", w.Body.String())
	}
}

// TestCrossUserMultigetDoesNotLeak: a calendar-multiget REPORT issued against
// the requester's OWN calendar (which passes the BeforeMethod read gate) but
// carrying an href that points at another user's object must not return that
// object's data — the per-href PropertiesForPath resolution re-applies the ACL
// read gate, yielding a 403/404 propstat instead.
func TestCrossUserMultigetDoesNotLeak(t *testing.T) {
	srv := twoUserCalendarServer("principals/user2")

	report := `<?xml version="1.0"?>` +
		`<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">` +
		`<d:prop><d:getetag/><c:calendar-data/></d:prop>` +
		`<d:href>/calendars/user1/default/secret.ics</d:href>` +
		`</c:calendar-multiget>`
	req := httptest.NewRequest("REPORT", "/calendars/user2/default", strings.NewReader(report))
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Depth", "1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	// The REPORT on the requester's own calendar is itself allowed — a 207
	// proves we are exercising the per-href gate, not a blanket request-level
	// denial that would make the leak assertions vacuous.
	if w.Code != 207 {
		t.Fatalf("multiget on own calendar = %d, want 207 (per-href gate not exercised otherwise):\n%s",
			w.Code, w.Body.String())
	}
	// The foreign object's data must not appear anywhere in the body...
	if strings.Contains(w.Body.String(), "TopSecret") {
		t.Fatalf("cross-user multiget leaked another user's event data:\n%s", w.Body.String())
	}
	// ...and its href must be reported forbidden, not served.
	if !strings.Contains(w.Body.String(), "403 Forbidden") {
		t.Fatalf("cross-user multiget did not deny the foreign href with 403:\n%s", w.Body.String())
	}
}

// TestReadOnlyShareeExportIsRedacted: a read-only sharee's ?export serves
// CONFIDENTIAL/PRIVATE events redacted, exactly as their GET of the object
// does, whether or not the export is time-filtered.
func TestReadOnlyShareeExportIsRedacted(t *testing.T) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/user2"}}}
	backend := &caldav.MockBackend{}
	id := caldav.CalendarID{Calendar: "1", Instance: "2"}
	backend.Calendars = []caldav.CalendarInfo{{
		ID: id, URI: "shared", PrincipalURI: "principals/user2", ShareAccess: caldav.AccessRead,
		Props: map[xml.Name]any{{Space: "http://sabredav.org/ns", Local: "read-only"}: "1"},
	}}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{
		id: {"therapy.ics": &caldav.ObjectInfo{URI: "therapy.ics", CalendarData: []byte(
			"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
				"BEGIN:VEVENT\r\nUID:conf-evt\r\nDTSTAMP:20260101T000000Z\r\n" +
				"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nCLASS:CONFIDENTIAL\r\n" +
				"SUMMARY:Therapy appointment\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")}},
	}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user2"))
	srv.AddPlugin(&acl.Plugin{})
	srv.AddPlugin(&caldav.Plugin{})

	for _, query := range []string{"?export", "?export&start=1788000000&end=1790000000"} {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest("GET", "/calendars/user2/shared/"+query, nil))
		if w.Code != 200 {
			t.Fatalf("sharee %s = %d, want 200:\n%s", query, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if strings.Contains(body, "Therapy") {
			t.Errorf("sharee %s leaked a CONFIDENTIAL summary:\n%s", query, body)
		}
		if !strings.Contains(body, "conf-evt") {
			t.Errorf("sharee %s dropped the event instead of redacting it:\n%s", query, body)
		}
	}
}
