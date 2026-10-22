package acl_test

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

// buildServer creates a two-user server with one calendar owned by user1.
func buildServer(principal string) *dav.Server {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1"}, {URI: "principals/user2"}, {URI: "principals/admin"},
	}}
	backend := &caldav.MockBackend{
		Calendars: []caldav.CalendarInfo{{
			ID: caldav.CalendarID{Calendar: "1"}, URI: "cal", PrincipalURI: "principals/user1",
		}},
		Objects: map[caldav.CalendarID]map[string]*caldav.ObjectInfo{
			{Calendar: "1"}: {"event.ics": {
				URI: "event.ics", CalendarID: caldav.CalendarID{Calendar: "1"},
				CalendarData: []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:x\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260101T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"),
			}},
		},
	}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))

	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin(principal))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	return srv
}

func request(t *testing.T, srv *dav.Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func TestOwnerCanReadOwnCalendar(t *testing.T) {
	w := request(t, buildServer("principals/user1"), "GET", "/calendars/user1/cal/event.ics", "")
	if w.Code != 200 {
		t.Errorf("owner GET = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestAdminCanReadAnyCalendar(t *testing.T) {
	w := request(t, buildServer("principals/admin"), "GET", "/calendars/user1/cal/event.ics", "")
	if w.Code != 200 {
		t.Errorf("admin GET = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestStrangerCannotReadCalendar(t *testing.T) {
	srv := buildServer("principals/user2")
	// Calendar collection: gated by the calendar's own ACL.
	w := request(t, srv, "GET", "/calendars/user1/cal/event.ics", "")
	if w.Code != 403 {
		t.Errorf("stranger GET object = %d, want 403: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NeedPrivileges") {
		t.Errorf("expected NeedPrivileges error body, got: %s", w.Body.String())
	}
	// PROPFIND is not request-level gated (sabre parity): it answers 207
	// with every property in a 403 propstat instead.
	w = request(t, srv, "PROPFIND", "/calendars/user1/cal/", "")
	if w.Code != 207 {
		t.Errorf("stranger PROPFIND calendar = %d, want 207", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "HTTP/1.1 403 Forbidden") ||
		strings.Contains(body, "HTTP/1.1 200 OK") {
		t.Errorf("stranger PROPFIND should return only 403 propstats:\n%s", body)
	}
}

func TestStrangerCannotWriteCalendar(t *testing.T) {
	srv := buildServer("principals/user2")
	w := request(t, srv, "PUT", "/calendars/user1/cal/new.ics",
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:y\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260101T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	if w.Code != 403 {
		t.Errorf("stranger PUT = %d, want 403: %s", w.Code, w.Body.String())
	}
	w = request(t, srv, "DELETE", "/calendars/user1/cal/event.ics", "")
	if w.Code != 403 {
		t.Errorf("stranger DELETE = %d, want 403: %s", w.Code, w.Body.String())
	}
}

func TestOwnerCanWriteOwnCalendar(t *testing.T) {
	srv := buildServer("principals/user1")
	w := request(t, srv, "PUT", "/calendars/user1/cal/new.ics",
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:y\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260101T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	if w.Code != 201 {
		t.Errorf("owner PUT = %d, want 201: %s", w.Code, w.Body.String())
	}
}

func TestStrangerQueryReportForbidden(t *testing.T) {
	query := `<?xml version="1.0"?><c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/></d:prop><c:filter><c:comp-filter name="VCALENDAR"/></c:filter></c:calendar-query>`
	req := httptest.NewRequest("REPORT", "/calendars/user1/cal/", strings.NewReader(query))
	req.Header.Set("Depth", "1")
	w := httptest.NewRecorder()
	buildServer("principals/user2").ServeHTTP(w, req)
	if w.Code != 403 {
		t.Errorf("stranger calendar-query = %d, want 403: %s", w.Code, w.Body.String())
	}
}

// A COPY/MOVE whose source does not exist must still be authorized against the
// destination, and must 404 without touching it.
func TestCopyMoveMissingSourceCannotClobberForeignDestination(t *testing.T) {
	for _, method := range []string{"COPY", "MOVE"} {
		srv := buildServer("principals/user2")
		req := httptest.NewRequest(method, "/calendars/user2/nope.ics", nil)
		req.Header.Set("Destination", "/calendars/user1/cal/event.ics")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Errorf("%s missing source onto foreign object = %d, want 403: %s", method, w.Code, w.Body.String())
		}
		if !srv.Tree.NodeExists("calendars/user1/cal/event.ics") {
			t.Errorf("%s deleted the foreign destination", method)
		}
	}
}

func TestCopyMoveMissingSourceIs404(t *testing.T) {
	for _, method := range []string{"COPY", "MOVE"} {
		srv := buildServer("principals/user1")
		req := httptest.NewRequest(method, "/calendars/user1/cal/nope.ics", nil)
		req.Header.Set("Destination", "/calendars/user1/cal/event.ics")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != 404 {
			t.Errorf("%s missing source = %d, want 404: %s", method, w.Code, w.Body.String())
		}
		if !srv.Tree.NodeExists("calendars/user1/cal/event.ics") {
			t.Errorf("%s with a missing source deleted the destination", method)
		}
	}
}
