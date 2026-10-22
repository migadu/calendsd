package caldav_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"calendsd/internal/acl"
	"calendsd/internal/auth"
	"calendsd/internal/caldav"
	"calendsd/internal/dav"
	"calendsd/internal/dav/mock"
)

// TestPrivilegeCheckerFailsClosed guards the fail-closed contract: the
// self-enforcing ACL paths (Add-Member here) resolve a PrivilegeChecker via
// caldav.RequirePrivilegeChecker, which denies when none is registered. If the
// acl plugin is dropped from the stack, an Add-Member POST must be refused, not
// silently allowed — the same resolver gates free-busy, outbox schedule-send,
// and the sharing actions, so this one case covers the shared bypass.
func TestPrivilegeCheckerFailsClosed(t *testing.T) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/user1"}}}
	backend := &caldav.MockSchedulingBackend{}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1"},
	}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	// Deliberately no acl.Plugin: the stack has no PrivilegeChecker.
	srv.AddPlugin(&caldav.Plugin{})

	event := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:am1\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:Posted\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	post := httptest.NewRequest("POST", "/calendars/user1/default/;add-member", strings.NewReader(event))
	post.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post)

	if w.Code != 403 {
		t.Fatalf("Add-Member with no ACL plugin = %d, want 403 (fail closed): %s", w.Code, w.Body.String())
	}
	// The write must not have reached the backend.
	if objs := backend.Objects[caldav.CalendarID{Calendar: "1"}]; len(objs) != 0 {
		t.Fatalf("Add-Member created %d object(s) despite the denial", len(objs))
	}
}
