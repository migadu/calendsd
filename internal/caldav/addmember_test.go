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
)

// TestAddMember exercises RFC 5995: {DAV:}add-member is advertised on a
// calendar, and a POST to "<calendar>/;add-member" creates a server-named
// object and returns 201 + Location that then GETs the stored data.
func TestAddMember(t *testing.T) {
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
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})

	// The add-member property points at <calendar>/;add-member.
	pf := httptest.NewRequest("PROPFIND", "/calendars/user1/default/", strings.NewReader(
		`<propfind xmlns="DAV:"><prop><add-member/></prop></propfind>`))
	pf.Header.Set("Depth", "0")
	pw := httptest.NewRecorder()
	srv.ServeHTTP(pw, pf)
	if !strings.Contains(pw.Body.String(), "/calendars/user1/default/;add-member") {
		t.Fatalf("add-member property missing/wrong:\n%s", pw.Body.String())
	}

	event := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:am1\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:Posted\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	post := httptest.NewRequest("POST", "/calendars/user1/default/;add-member", strings.NewReader(event))
	post.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, post)
	if w.Code != 201 {
		t.Fatalf("POST add-member = %d: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if loc == "" || !strings.HasPrefix(loc, "/calendars/user1/default/") || loc == "/calendars/user1/default/;add-member" {
		t.Fatalf("bad Location %q", loc)
	}

	get := httptest.NewRequest("GET", loc, nil)
	gw := httptest.NewRecorder()
	srv.ServeHTTP(gw, get)
	if gw.Code != 200 || !strings.Contains(gw.Body.String(), "UID:am1") {
		t.Fatalf("GET %s = %d: %s", loc, gw.Code, gw.Body.String())
	}

	// A non-add-member POST (no such handler) must not be swallowed as 201.
	bad := httptest.NewRequest("POST", "/calendars/user1/default/", strings.NewReader(event))
	bw := httptest.NewRecorder()
	srv.ServeHTTP(bw, bad)
	if bw.Code == 201 {
		t.Errorf("plain POST to the calendar should not create via add-member")
	}
}
