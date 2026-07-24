package caldav_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
	"calserver/internal/dav/mock"
)

// An events-only calendar (supported-calendar-component-set = VEVENT) accepts a
// VEVENT PUT but rejects a VTODO with 403, matching sabre's InvalidComponentType.
func TestComponentTypeEnforcedOnPut(t *testing.T) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1"},
	}}
	backend := &caldav.MockBackend{Calendars: []caldav.CalendarInfo{{
		ID:           caldav.CalendarID{Calendar: "1"},
		URI:          "events",
		PrincipalURI: "principals/user1",
		Props: map[xmlNM]any{
			{Space: davxml.NSCalDAV, Local: "supported-calendar-component-set"}: davxml.Raw(`<cal:comp name="VEVENT"/>`),
		},
	}}}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})

	put := func(uri, body string) int {
		req := httptest.NewRequest("PUT", "/calendars/user1/events/"+uri, strings.NewReader(body))
		req.Header.Set("Content-Type", "text/calendar")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Code
	}

	event := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:e1\r\nDTSTART:20260101T120000Z\r\nDTEND:20260101T130000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if code := put("e1.ics", event); code != 201 {
		t.Errorf("VEVENT PUT into events calendar = %d, want 201", code)
	}

	todo := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:t1\r\nDUE:20260101T120000Z\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	if code := put("t1.ics", todo); code != 403 {
		t.Errorf("VTODO PUT into events-only calendar = %d, want 403", code)
	}
}
