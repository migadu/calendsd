package caldav_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/auth"
	"github.com/migadu/calendsd/internal/caldav"
	"github.com/migadu/calendsd/internal/dav"
	"github.com/migadu/calendsd/internal/dav/mock"
	davxml "github.com/migadu/calendsd/internal/dav/xml"
)

// privateCalServer builds a server where user1 and user2 share a domain and
// user1 owns one calendar holding a busy event. When private is true, that
// calendar is marked schedule-calendar-transp:transparent (the "private"
// toggle), which must exclude it from all free-busy reporting.
func privateCalServer(loggedIn string, private bool) *dav.Server {
	sd := "http://sabredav.org/ns"
	props := map[xmlNM]any{}
	if private {
		props[xmlNM{Space: davxml.NSCalDAV, Local: "schedule-calendar-transp"}] =
			davxml.Raw("<cal:transparent/>")
	}
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user1@example.org"}},
		{URI: "principals/user2", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user2@example.org"}},
	}}
	backend := &caldav.MockSchedulingBackend{}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1", Props: props},
	}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{
		{Calendar: "1"}: {"busy.ics": &caldav.ObjectInfo{URI: "busy.ics", CalendarData: []byte(
			"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
				"BEGIN:VEVENT\r\nUID:fb-busy\r\nDTSTAMP:20260101T000000Z\r\n" +
				"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:Busy\r\n" +
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

const fbBusyPeriod = "FREEBUSY:20260901T100000Z/20260901T110000Z"

// TestPrivateCalendarHiddenFromOutboxFreeBusy: a same-domain colleague probing
// user1's free-busy through the outbox sees no busy time from a private
// calendar, yet the principal still reports as schedulable (success, not the
// "could not find principal" denial used for a genuine cross-domain block).
func TestPrivateCalendarHiddenFromOutboxFreeBusy(t *testing.T) {
	srv := privateCalServer("principals/user2", true)

	req := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VFREEBUSY\r\nUID:fb-probe\r\nDTSTART:20260801T000000Z\r\nDTEND:20261001T000000Z\r\n" +
		"ORGANIZER:mailto:user2@example.org\r\nATTENDEE:mailto:user1@example.org\r\n" +
		"END:VFREEBUSY\r\nEND:VCALENDAR\r\n"
	post := httptest.NewRequest("POST", "/calendars/user2/outbox", strings.NewReader(req))
	post.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post)
	if w.Code != 200 {
		t.Fatalf("outbox POST = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "2.0;Success") {
		t.Errorf("private-calendar probe should still be schedulable (2.0;Success):\n%s", body)
	}
	if strings.Contains(body, fbBusyPeriod) {
		t.Errorf("private calendar leaked its busy period through free-busy:\n%s", body)
	}
}

// TestOpaqueCalendarStillReportsBusy is the control: the same setup without the
// private flag does surface the busy period, proving the toggle is what hides
// it (not some unrelated gate).
func TestOpaqueCalendarStillReportsBusy(t *testing.T) {
	srv := privateCalServer("principals/user2", false)

	req := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VFREEBUSY\r\nUID:fb-probe\r\nDTSTART:20260801T000000Z\r\nDTEND:20261001T000000Z\r\n" +
		"ORGANIZER:mailto:user2@example.org\r\nATTENDEE:mailto:user1@example.org\r\n" +
		"END:VFREEBUSY\r\nEND:VCALENDAR\r\n"
	post := httptest.NewRequest("POST", "/calendars/user2/outbox", strings.NewReader(req))
	post.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post)

	if !strings.Contains(w.Body.String(), fbBusyPeriod) {
		t.Errorf("opaque calendar should report its busy period:\n%s", w.Body.String())
	}
}

// TestPrivateCalendarFreeBusyQueryEmpty: a direct free-busy-query REPORT on a
// private calendar (even by the owner) returns an empty VFREEBUSY, not the
// event's busy time.
func TestPrivateCalendarFreeBusyQueryEmpty(t *testing.T) {
	srv := privateCalServer("principals/user1", true)

	report := `<?xml version="1.0"?>` +
		`<c:free-busy-query xmlns:c="urn:ietf:params:xml:ns:caldav">` +
		`<c:time-range start="20260801T000000Z" end="20261001T000000Z"/>` +
		`</c:free-busy-query>`
	req := httptest.NewRequest("REPORT", "/calendars/user1/default", strings.NewReader(report))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("owner free-busy-query on private calendar = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), fbBusyPeriod) {
		t.Errorf("private calendar free-busy-query leaked the busy period:\n%s", w.Body.String())
	}
}
