package caldav_test

// A PUT using an IANA TZID it doesn't define gets a generated VTIMEZONE
// (RFC 5545 §3.6.5; docs/dst-recurring-events.md §5), and — because the
// stored body then differs from the one sent — no ETag (RFC 4791 §5.3.4).

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

func tzServer(addVTimezones bool) *dav.Server {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/user1"}}}
	backend := &caldav.MockBackend{}
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
	srv.AddPlugin(&caldav.Plugin{AddVTimezones: addVTimezones})
	return srv
}

const zonedEvent = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//calclient//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:tz1\r\nDTSTAMP:20260101T000000Z\r\n" +
	"DTSTART;TZID=Europe/Zurich:20260313T090000\r\nDTEND;TZID=Europe/Zurich:20260313T100000\r\n" +
	"RRULE:FREQ=WEEKLY\r\nSUMMARY:Standup\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func putGet(t *testing.T, srv *dav.Server, path, body string) (put *httptest.ResponseRecorder, stored string) {
	t.Helper()
	r := httptest.NewRequest("PUT", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "text/calendar")
	put = httptest.NewRecorder()
	srv.ServeHTTP(put, r)
	if put.Code != 201 && put.Code != 204 {
		t.Fatalf("PUT %s = %d: %s", path, put.Code, put.Body.String())
	}
	g := httptest.NewRecorder()
	srv.ServeHTTP(g, httptest.NewRequest("GET", path, nil))
	return put, g.Body.String()
}

func TestPutBareTZIDGetsVTimezoneAndNoETag(t *testing.T) {
	srv := tzServer(true)
	put, stored := putGet(t, srv, "/calendars/user1/default/tz1.ics", zonedEvent)
	if !strings.Contains(stored, "BEGIN:VTIMEZONE\r\nTZID:Europe/Zurich\r\n") {
		t.Fatalf("no VTIMEZONE stored:\n%s", stored)
	}
	if etag := put.Header().Get("ETag"); etag != "" {
		t.Errorf("modified PUT returned ETag %q", etag)
	}

	// Sending the stored body back — VTIMEZONE included — is stored as sent
	// and does get its ETag.
	put2, stored2 := putGet(t, srv, "/calendars/user1/default/tz1.ics", stored)
	if stored2 != stored {
		t.Errorf("a body with its VTIMEZONE was rewritten:\n%s", stored2)
	}
	if put2.Header().Get("ETag") == "" {
		t.Error("unmodified PUT returned no ETag")
	}
}

func TestPutKeepsBodyWhenVTimezonesAreOff(t *testing.T) {
	// The sabre-parity composition stores the body exactly as sent.
	srv := tzServer(false)
	put, stored := putGet(t, srv, "/calendars/user1/default/tz2.ics", zonedEvent)
	if stored != zonedEvent {
		t.Errorf("body changed:\n%s", stored)
	}
	if put.Header().Get("ETag") == "" {
		t.Error("unmodified PUT returned no ETag")
	}
}
