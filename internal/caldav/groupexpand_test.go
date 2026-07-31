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

// TestExpandGroupAttendees: an organizer inviting a group ATTENDEE gets the
// group rewritten to a display-only X-SERVER-GROUP placeholder and one real
// ATTENDEE per member, and each member receives an inbox REQUEST.
func TestExpandGroupAttendees(t *testing.T) {
	sd := "http://sabredav.org/ns"
	principals := &acl.MemoryBackend{
		Principals: []acl.Principal{
			{URI: "principals/user1", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user1@example.org"}},
			{URI: "principals/group1", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "group1@example.org"}},
			{URI: "principals/user6", Props: map[xmlNM]any{
				{Space: sd, Local: "email-address"}:   "user6@example.org",
				{Space: "DAV:", Local: "displayname"}: "User Six",
			}},
			{URI: "principals/user7", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user7@example.org"}},
		},
		Groups: map[string][]string{
			"principals/group1": {"principals/user6", "principals/user7"},
		},
	}
	backend := &caldav.MockSchedulingBackend{}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1"},
		{ID: caldav.CalendarID{Calendar: "6"}, URI: "default", PrincipalURI: "principals/user6"},
		{ID: caldav.CalendarID{Calendar: "7"}, URI: "default", PrincipalURI: "principals/user7"},
	}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&caldav.SchedulePlugin{ExpandGroups: true})

	invite := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:g1\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:Group event\r\n" +
		"ORGANIZER;CN=User One:mailto:user1@example.org\r\n" +
		"ATTENDEE;CN=User One;PARTSTAT=ACCEPTED:mailto:user1@example.org\r\n" +
		"ATTENDEE;CN=Group;RSVP=TRUE;PARTSTAT=NEEDS-ACTION:mailto:group1@example.org\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	req := httptest.NewRequest("PUT", "/calendars/user1/default/g1.ics", strings.NewReader(invite))
	req.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}

	get := httptest.NewRequest("GET", "/calendars/user1/default/g1.ics", nil)
	gw := httptest.NewRecorder()
	srv.ServeHTTP(gw, get)
	stored := strings.ReplaceAll(gw.Body.String(), "\r\n ", "") // unfold

	if !strings.Contains(stored, "CUTYPE=X-SERVER-GROUP") {
		t.Errorf("group attendee not converted to X-SERVER-GROUP:\n%s", stored)
	}
	if !strings.Contains(stored, "SCHEDULE-STATUS=2.7") {
		t.Errorf("group placeholder missing SCHEDULE-STATUS=2.7:\n%s", stored)
	}
	for _, member := range []string{"mailto:user6@example.org", "mailto:user7@example.org"} {
		if !strings.Contains(stored, member) {
			t.Errorf("member %s not added to attendee list:\n%s", member, stored)
		}
	}
	if !strings.Contains(stored, `MEMBER="mailto:group1@example.org"`) {
		t.Errorf("member ATTENDEE missing MEMBER param:\n%s", stored)
	}
	// Members received an inbox REQUEST; the group itself did not.
	if n := len(backend.Scheduling["principals/user6"]); n != 1 {
		t.Errorf("user6 inbox = %d, want 1", n)
	}
	if n := len(backend.Scheduling["principals/group1"]); n != 0 {
		t.Errorf("group1 inbox = %d, want 0 (groups are not scheduled)", n)
	}
}
