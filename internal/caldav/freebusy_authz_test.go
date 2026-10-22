package caldav_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
)

// freeBusyServer builds a server where user1 and user2 share a mail domain,
// user3 lives on a foreign domain, and user4 has no email address at all.
// user1 owns one calendar with one busy slot.
func freeBusyServer(loggedIn string) *dav.Server {
	sd := "http://sabredav.org/ns"
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user1@example.org"}},
		{URI: "principals/user2", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user2@example.org"}},
		{URI: "principals/user3", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user3@elsewhere.example"}},
		{URI: "principals/user4"},
	}}
	backend := &caldav.MockSchedulingBackend{}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1"},
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

// probeFreeBusy POSTs a VFREEBUSY REQUEST for user1 to the logged-in user's
// own outbox and returns the response.
func probeFreeBusy(t *testing.T, srv *dav.Server, outboxUser string) *httptest.ResponseRecorder {
	t.Helper()
	vfb := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VFREEBUSY\r\nUID:fb-probe\r\nDTSTART:20260801T000000Z\r\nDTEND:20261001T000000Z\r\n" +
		"ORGANIZER:mailto:" + outboxUser + "@example.org\r\n" +
		"ATTENDEE:mailto:user1@example.org\r\n" +
		"END:VFREEBUSY\r\nEND:VCALENDAR\r\n"
	post := httptest.NewRequest("POST", "/calendars/"+outboxUser+"/outbox", strings.NewReader(vfb))
	post.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post)
	if w.Code != 200 {
		t.Fatalf("outbox POST = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	return w
}

// TestFreeBusySameDomainAllowed: a user in the calendar owner's mail domain
// gets the owner's busy periods through the outbox.
func TestFreeBusySameDomainAllowed(t *testing.T) {
	srv := freeBusyServer("principals/user2")
	w := probeFreeBusy(t, srv, "user2")

	body := w.Body.String()
	if !strings.Contains(body, "2.0;Success") {
		t.Errorf("same-domain probe missing success status:\n%s", body)
	}
	if !strings.Contains(body, "FREEBUSY:20260901T100000Z/20260901T110000Z") {
		t.Errorf("same-domain probe missing the busy period:\n%s", body)
	}
}

// TestFreeBusyCrossDomainDenied: a user on another domain gets no free-busy
// data, and the denial is indistinguishable from an unknown address so it
// cannot be used to enumerate users.
func TestFreeBusyCrossDomainDenied(t *testing.T) {
	srv := freeBusyServer("principals/user3")
	w := probeFreeBusy(t, srv, "user3")

	body := w.Body.String()
	if !strings.Contains(body, "3.7;Could not find principal") {
		t.Errorf("cross-domain probe status = success, want 3.7:\n%s", body)
	}
	if strings.Contains(body, "FREEBUSY") || strings.Contains(body, "calendar-data") {
		t.Errorf("cross-domain probe leaked free-busy data:\n%s", body)
	}
}

// TestFreeBusyNoEmailDenied: a principal without an email address has no
// domain, so the same-domain grant fails closed.
func TestFreeBusyNoEmailDenied(t *testing.T) {
	srv := freeBusyServer("principals/user4")
	w := probeFreeBusy(t, srv, "user4")

	body := w.Body.String()
	if !strings.Contains(body, "3.7;Could not find principal") {
		t.Errorf("no-email probe status = success, want 3.7:\n%s", body)
	}
	if strings.Contains(body, "FREEBUSY") {
		t.Errorf("no-email probe leaked free-busy data:\n%s", body)
	}
}

// TestFreeBusyOwnerSelfProbe: the owner keeps seeing their own availability.
func TestFreeBusyOwnerSelfProbe(t *testing.T) {
	srv := freeBusyServer("principals/user1")
	w := probeFreeBusy(t, srv, "user1")

	body := w.Body.String()
	if !strings.Contains(body, "FREEBUSY:20260901T100000Z/20260901T110000Z") {
		t.Errorf("owner self-probe missing the busy period:\n%s", body)
	}
}

// TestFreeBusyQueryReportCrossDomain: the free-busy-query REPORT on a foreign
// calendar stays denied for a cross-domain user.
func TestFreeBusyQueryReportCrossDomain(t *testing.T) {
	srv := freeBusyServer("principals/user3")

	report := `<?xml version="1.0"?>` +
		`<c:free-busy-query xmlns:c="urn:ietf:params:xml:ns:caldav">` +
		`<c:time-range start="20260801T000000Z" end="20261001T000000Z"/>` +
		`</c:free-busy-query>`
	req := httptest.NewRequest("REPORT", "/calendars/user1/default", strings.NewReader(report))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 403 {
		t.Fatalf("cross-domain free-busy-query = %d, want 403:\n%s", w.Code, w.Body.String())
	}
}

// TestFreeBusyOrganizerParamsQuoted: the request's ORGANIZER is echoed with
// its parameters quoted, so a CN containing a comma stays one value.
func TestFreeBusyOrganizerParamsQuoted(t *testing.T) {
	srv := freeBusyServer("principals/user2")
	vfb := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VFREEBUSY\r\nUID:fb-probe\r\nDTSTART:20260801T000000Z\r\nDTEND:20261001T000000Z\r\n" +
		"ORGANIZER;CN=\"Two, User\":mailto:user2@example.org\r\n" +
		"ATTENDEE:mailto:user1@example.org\r\n" +
		"END:VFREEBUSY\r\nEND:VCALENDAR\r\n"
	post := httptest.NewRequest("POST", "/calendars/user2/outbox", strings.NewReader(vfb))
	post.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post)
	if w.Code != 200 {
		t.Fatalf("outbox POST = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	body := strings.ReplaceAll(w.Body.String(), "&#34;", `"`)
	body = strings.ReplaceAll(body, "&quot;", `"`)
	for _, want := range []string{
		`ORGANIZER;CN="Two, User":mailto:user2@example.org`,
		"UID:fb-probe",
		"ATTENDEE:mailto:user1@example.org",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %q:\n%s", want, body)
		}
	}
}
