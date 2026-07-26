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

// twoPrincipalServer builds a server with user1 owning a "default" calendar,
// authenticated as loggedIn.
func twoPrincipalServer(loggedIn string) (*dav.Server, *caldav.MockSchedulingBackend) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1"}, {URI: "principals/user2"},
	}}
	backend := &caldav.MockSchedulingBackend{}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1"},
	}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin(loggedIn))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&caldav.SchedulePlugin{})
	return srv, backend
}

// TestStrangerCannotAddMember: an Add-Member POST by a non-owner is rejected
// with 403 ({DAV:}bind), matching how a PUT-create is gated, and writes nothing.
func TestStrangerCannotAddMember(t *testing.T) {
	srv, backend := twoPrincipalServer("principals/user2")

	event := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:am1\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:Sneaky\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	post := httptest.NewRequest("POST", "/calendars/user1/default/;add-member", strings.NewReader(event))
	post.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post)

	if w.Code != 403 {
		t.Fatalf("stranger Add-Member = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if n := len(backend.Objects[caldav.CalendarID{Calendar: "1"}]); n != 0 {
		t.Errorf("stranger Add-Member created %d object(s); want 0", n)
	}
}

// TestStrangerCannotProbeOutbox: a free-busy POST to another principal's outbox
// is rejected with 403 (schedule-send), preventing a cross-user free-busy oracle.
func TestStrangerCannotProbeOutbox(t *testing.T) {
	srv, _ := twoPrincipalServer("principals/user2")

	vfb := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VFREEBUSY\r\nUID:fb1\r\nDTSTART:20260101T000000Z\r\nDTEND:20270101T000000Z\r\n" +
		"ORGANIZER:mailto:user2@example.org\r\nATTENDEE:mailto:user1@example.org\r\n" +
		"END:VFREEBUSY\r\nEND:VCALENDAR\r\n"
	post := httptest.NewRequest("POST", "/calendars/user1/outbox", strings.NewReader(vfb))
	post.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post)

	if w.Code != 403 {
		t.Fatalf("stranger outbox POST = %d, want 403:\n%s", w.Code, w.Body.String())
	}
}

// TestOwnerCanAddMember confirms the bind check does not break the legitimate
// owner path.
func TestOwnerCanAddMember(t *testing.T) {
	srv, _ := twoPrincipalServer("principals/user1")

	event := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:am2\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:Mine\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	post := httptest.NewRequest("POST", "/calendars/user1/default/;add-member", strings.NewReader(event))
	post.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post)

	if w.Code != 201 {
		t.Fatalf("owner Add-Member = %d, want 201:\n%s", w.Code, w.Body.String())
	}
}
