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

// Malformed iCalendar must fail as a clean client error, never a 500 leaking an
// internal parse error. A body that is not iCalendar at all is a 415; a body
// that parses but is semantically invalid (bad DURATION) is a 400 carrying the
// RFC 4791 {CALDAV:}valid-calendar-data precondition. Regression test for the
// CalDAVTester finding where an invalid DURATION returned HTTP 500.
func TestMalformedCalendarDataRejectedCleanly(t *testing.T) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/user1"}}}
	backend := &caldav.MockBackend{Calendars: []caldav.CalendarInfo{{
		ID: caldav.CalendarID{Calendar: "1"}, URI: "calendar", PrincipalURI: "principals/user1",
	}}}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})

	put := func(uri, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/calendars/user1/calendar/"+uri, strings.NewReader(body))
		req.Header.Set("Content-Type", "text/calendar")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}

	if w := put("garbage.ics", "this is not iCalendar"); w.Code != 415 {
		t.Errorf("non-iCalendar PUT = %d, want 415\n%s", w.Code, w.Body.String())
	}

	badDur := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:bad\r\nDTSTART:20260101T120000Z\r\nDURATION:PTH\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	w := put("bad.ics", badDur)
	if w.Code != 400 {
		t.Fatalf("invalid-DURATION PUT = %d, want 400 (must never be 500)\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), davxml.NSCalDAV) ||
		!strings.Contains(w.Body.String(), "valid-calendar-data") {
		t.Errorf("400 body missing valid-calendar-data precondition:\n%s", w.Body.String())
	}
}

// A calendar object over max-resource-size is refused with 403 and the
// {CALDAV:}max-resource-size precondition, by PUT and by Add-Member POST; one
// at the limit is stored.
func TestMaxResourceSizeEnforced(t *testing.T) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/user1"}}}
	backend := &caldav.MockBackend{Calendars: []caldav.CalendarInfo{{
		ID: caldav.CalendarID{Calendar: "1"}, URI: "calendar", PrincipalURI: "principals/user1",
	}}}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})

	// An event padded with a DESCRIPTION to exactly size bytes.
	event := func(uid string, size int) string {
		head := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:" + uid +
			"\r\nDTSTART:20260101T120000Z\r\nDTEND:20260101T130000Z\r\nDESCRIPTION:"
		tail := "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		return head + strings.Repeat("x", size-len(head)-len(tail)) + tail
	}
	send := func(method, uri, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/calendars/user1/calendar/"+uri, strings.NewReader(body))
		req.Header.Set("Content-Type", "text/calendar")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}

	if w := send("PUT", "max.ics", event("max", caldav.MaxResourceSize)); w.Code != 201 {
		t.Errorf("PUT at the limit = %d, want 201: %s", w.Code, w.Body.String())
	}
	for _, c := range []struct{ method, uri string }{{"PUT", "over.ics"}, {"POST", ";add-member"}} {
		w := send(c.method, c.uri, event("over", caldav.MaxResourceSize+1))
		if w.Code != 403 || !strings.Contains(w.Body.String(), "max-resource-size") {
			t.Errorf("%s over the limit = %d, want 403 with max-resource-size:\n%s", c.method, w.Code, w.Body.String())
		}
	}
}
