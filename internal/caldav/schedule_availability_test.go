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

// workingHours is Mon–Fri 09:00–17:00 Europe/Berlin, as calweb writes it.
const workingHours = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
	"BEGIN:VAVAILABILITY\r\nUID:wh\r\nDTSTAMP:20260101T000000Z\r\n" +
	"BEGIN:AVAILABLE\r\nUID:wh-days\r\nDTSTAMP:20260101T000000Z\r\n" +
	"DTSTART;TZID=Europe/Berlin:20260105T090000\r\n" +
	"DTEND;TZID=Europe/Berlin:20260105T170000\r\n" +
	"RRULE:FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR\r\n" +
	"END:AVAILABLE\r\nEND:VAVAILABILITY\r\nEND:VCALENDAR\r\n"

// availabilityServer: user1 and user2 share a domain, user3 is foreign.
// user1 owns a calendar with one busy hour on Tuesday 2026-09-01. Dead
// properties go to an in-memory store, so all-or-nothing PROPPATCH can be
// observed.
func availabilityServer(loggedIn string) (*dav.Server, *caldav.MockSchedulingBackend, *mock.MemoryPropStorage) {
	sd := "http://sabredav.org/ns"
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user1@example.org"}},
		{URI: "principals/user2", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user2@example.org"}},
		{URI: "principals/user3", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user3@elsewhere.example"}},
	}}
	backend := &caldav.MockSchedulingBackend{}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1"},
	}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{
		{Calendar: "1"}: {"busy.ics": &caldav.ObjectInfo{URI: "busy.ics", CalendarData: []byte(
			"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
				"BEGIN:VEVENT\r\nUID:busy\r\nDTSTAMP:20260101T000000Z\r\n" +
				"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\n" +
				"END:VEVENT\r\nEND:VCALENDAR\r\n")}},
	}
	props := &mock.MemoryPropStorage{}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin(loggedIn))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&caldav.SchedulePlugin{})
	srv.AddPlugin(&dav.PropertyStoragePlugin{Backend: props})
	return srv, backend, props
}

func xmlText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func davRequest(t *testing.T, srv *dav.Server, method, path, body string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

// setAvailability PROPPATCHes value under the given namespace's name; extra
// is any further <prop> content for the same <set>.
func setAvailability(t *testing.T, srv *dav.Server, user, ns, value, extra string) *httptest.ResponseRecorder {
	t.Helper()
	body := `<?xml version="1.0"?><d:propertyupdate xmlns:d="DAV:" xmlns:x="` + ns + `" xmlns:e="urn:example">` +
		`<d:set><d:prop><x:calendar-availability>` + xmlText(value) + `</x:calendar-availability>` + extra +
		`</d:prop></d:set></d:propertyupdate>`
	return davRequest(t, srv, "PROPPATCH", "/calendars/"+user+"/inbox/", body)
}

func propfindAvailability(t *testing.T, srv *dav.Server, user, ns string) *httptest.ResponseRecorder {
	t.Helper()
	body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:x="` + ns + `">` +
		`<d:prop><x:calendar-availability/></d:prop></d:propfind>`
	return davRequest(t, srv, "PROPFIND", "/calendars/"+user+"/inbox/", body, "Depth", "0")
}

const (
	nsCS  = "http://calendarserver.org/ns/"
	nsCal = "urn:ietf:params:xml:ns:caldav"
)

func TestAvailabilityPropPatchAndPropFind(t *testing.T) {
	srv, backend, props := availabilityServer("principals/user1")

	if w := propfindAvailability(t, srv, "user1", nsCS); !strings.Contains(w.Body.String(), "404") {
		t.Fatalf("unset PROPFIND should report 404:\n%s", w.Body.String())
	}

	// The XML body carries the value with LF line endings, as a parser
	// would leave it; it is stored with CRLF.
	w := setAvailability(t, srv, "user1", nsCS, strings.ReplaceAll(workingHours, "\r\n", "\n"), "")
	if w.Code != 207 || !strings.Contains(w.Body.String(), "200") {
		t.Fatalf("PROPPATCH = %d:\n%s", w.Code, w.Body.String())
	}
	if got := string(backend.Availabilities["principals/user1"]); got != workingHours {
		t.Fatalf("stored value = %q, want the CRLF form", got)
	}
	if len(props.Props) != 0 {
		t.Errorf("availability leaked into dead-property storage: %+v", props.Props)
	}

	for _, ns := range []string{nsCS, nsCal} {
		w := propfindAvailability(t, srv, "user1", ns)
		if !strings.Contains(w.Body.String(), "BEGIN:VAVAILABILITY") || !strings.Contains(w.Body.String(), "200") {
			t.Errorf("PROPFIND %s did not return the value:\n%s", ns, w.Body.String())
		}
	}

	// Setting through the RFC 7953 name replaces the same value.
	replaced := strings.Replace(workingHours, "T170000", "T180000", 1)
	if w := setAvailability(t, srv, "user1", nsCal, replaced, ""); w.Code != 207 || strings.Contains(w.Body.String(), "403") {
		t.Fatalf("PROPPATCH via the RFC name = %d:\n%s", w.Code, w.Body.String())
	}
	if got := string(backend.Availabilities["principals/user1"]); got != replaced {
		t.Errorf("RFC-name PROPPATCH stored %q", got)
	}
	if w := propfindAvailability(t, srv, "user1", nsCS); !strings.Contains(w.Body.String(), "T180000") {
		t.Errorf("CS name does not see the value set through the RFC name:\n%s", w.Body.String())
	}

	allprop := davRequest(t, srv, "PROPFIND", "/calendars/user1/inbox/",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>`, "Depth", "0")
	if strings.Contains(allprop.Body.String(), "calendar-availability") {
		t.Errorf("allprop includes calendar-availability:\n%s", allprop.Body.String())
	}

	// Removing through the RFC name clears the one value.
	rm := davRequest(t, srv, "PROPPATCH", "/calendars/user1/inbox/",
		`<?xml version="1.0"?><d:propertyupdate xmlns:d="DAV:" xmlns:c="`+nsCal+`">`+
			`<d:remove><d:prop><c:calendar-availability/></d:prop></d:remove></d:propertyupdate>`)
	if rm.Code != 207 || strings.Contains(rm.Body.String(), "403") {
		t.Fatalf("remove = %d:\n%s", rm.Code, rm.Body.String())
	}
	if _, set := backend.Availabilities["principals/user1"]; set {
		t.Error("remove left the value in place")
	}
}

func TestAvailabilityPropPatchRejects(t *testing.T) {
	cases := map[string]string{
		"not a calendar": "This is not a calendar!",
		"floating":       strings.Replace(workingHours, "DTSTART;TZID=Europe/Berlin:", "DTSTART:", 1),
		"all-day":        strings.Replace(workingHours, "DTSTART;TZID=Europe/Berlin:20260105T090000", "DTSTART;VALUE=DATE:20260105", 1),
		"minutely":       strings.Replace(workingHours, "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR", "FREQ=MINUTELY", 1),
		"no uid":         strings.Replace(workingHours, "UID:wh-days\r\n", "", 1),
		"oversized":      workingHours + strings.Repeat("X", 70<<10),
	}
	for name, value := range cases {
		srv, backend, _ := availabilityServer("principals/user1")
		w := setAvailability(t, srv, "user1", nsCS, value, "")
		if !strings.Contains(w.Body.String(), "403") {
			t.Errorf("%s: PROPPATCH accepted:\n%s", name, w.Body.String())
		}
		if len(backend.Availabilities) != 0 {
			t.Errorf("%s: invalid value stored", name)
		}
	}

	// All-or-nothing: an invalid availability fails the request, and the
	// valid dead property sent with it is not stored either (424).
	srv, _, props := availabilityServer("principals/user1")
	w := setAvailability(t, srv, "user1", nsCS, "garbage", `<e:note>hi</e:note>`)
	if !strings.Contains(w.Body.String(), "403") || !strings.Contains(w.Body.String(), "424") {
		t.Errorf("mixed PROPPATCH should be 403 + 424:\n%s", w.Body.String())
	}
	if len(props.Props) != 0 {
		t.Errorf("dead property stored despite the failed request: %+v", props.Props)
	}

	// Both names at once address one value: rejected.
	srv, backend, _ := availabilityServer("principals/user1")
	both := `<?xml version="1.0"?><d:propertyupdate xmlns:d="DAV:" xmlns:x="` + nsCS + `" xmlns:c="` + nsCal + `">` +
		`<d:set><d:prop><x:calendar-availability>` + xmlText(workingHours) + `</x:calendar-availability>` +
		`<c:calendar-availability>` + xmlText(workingHours) + `</c:calendar-availability></d:prop></d:set></d:propertyupdate>`
	w = davRequest(t, srv, "PROPPATCH", "/calendars/user1/inbox/", both)
	if !strings.Contains(w.Body.String(), "403") || len(backend.Availabilities) != 0 {
		t.Errorf("setting both names should be rejected:\n%s", w.Body.String())
	}
}

// Only the owner reads or writes their working hours through the inbox;
// colleagues see them solely as BUSY-UNAVAILABLE in free-busy.
func TestAvailabilityOtherUserCannotAccessInbox(t *testing.T) {
	srv, backend, _ := availabilityServer("principals/user2")
	backend.Availabilities = map[string][]byte{"principals/user1": []byte(workingHours)}

	if w := propfindAvailability(t, srv, "user1", nsCS); strings.Contains(w.Body.String(), "VAVAILABILITY") {
		t.Errorf("another user read the availability:\n%s", w.Body.String())
	}
	w := setAvailability(t, srv, "user1", nsCS, workingHours, "")
	if w.Code == 207 && !strings.Contains(w.Body.String(), "403") {
		t.Errorf("another user wrote the availability: %d\n%s", w.Code, w.Body.String())
	}
	if string(backend.Availabilities["principals/user1"]) != workingHours {
		t.Error("another user's PROPPATCH changed the value")
	}
}

func probeUser1(t *testing.T, srv *dav.Server, from string) string {
	t.Helper()
	vfb := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VFREEBUSY\r\nUID:probe\r\nDTSTART:20260901T000000Z\r\nDTEND:20260902T000000Z\r\n" +
		"ORGANIZER:mailto:" + from + "@example.org\r\n" +
		"ATTENDEE:mailto:user1@example.org\r\n" +
		"END:VFREEBUSY\r\nEND:VCALENDAR\r\n"
	req := httptest.NewRequest("POST", "/calendars/"+from+"/outbox", strings.NewReader(vfb))
	req.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("outbox POST = %d:\n%s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

func TestAvailabilityInOutboxFreeBusy(t *testing.T) {
	srv, backend, _ := availabilityServer("principals/user2")
	backend.Availabilities = map[string][]byte{"principals/user1": []byte(workingHours)}

	body := probeUser1(t, srv, "user2")
	for _, want := range []string{
		"2.0;Success",
		// Tuesday 2026-09-01, CEST: available 07:00–15:00Z.
		"FREEBUSY;FBTYPE=BUSY-UNAVAILABLE:20260901T000000Z/20260901T070000Z",
		"FREEBUSY;FBTYPE=BUSY-UNAVAILABLE:20260901T150000Z/20260902T000000Z",
		"FREEBUSY:20260901T100000Z/20260901T110000Z",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("free-busy missing %q:\n%s", want, body)
		}
	}

	// A cross-domain requester still learns nothing.
	srv3, backend3, _ := availabilityServer("principals/user3")
	backend3.Availabilities = map[string][]byte{"principals/user1": []byte(workingHours)}
	if body := probeUser1(t, srv3, "user3"); !strings.Contains(body, "3.7;Could not find principal") ||
		strings.Contains(body, "FREEBUSY") {
		t.Errorf("cross-domain probe leaked data:\n%s", body)
	}

	// An unusable stored value (never validated, e.g. imported) is ignored,
	// not turned into an error.
	backend.Availabilities["principals/user1"] = []byte("garbage")
	body = probeUser1(t, srv, "user2")
	if !strings.Contains(body, "2.0;Success") || strings.Contains(body, "BUSY-UNAVAILABLE") ||
		!strings.Contains(body, "FREEBUSY:20260901T100000Z/20260901T110000Z") {
		t.Errorf("corrupt availability should fall back to plain free-busy:\n%s", body)
	}
}

// The per-calendar free-busy-query REPORT is not affected by availability
// (it is per user, not per calendar).
func TestAvailabilityNotInFreeBusyQueryReport(t *testing.T) {
	srv, backend, _ := availabilityServer("principals/user1")
	backend.Availabilities = map[string][]byte{"principals/user1": []byte(workingHours)}
	w := davRequest(t, srv, "REPORT", "/calendars/user1/default/",
		`<?xml version="1.0"?><c:free-busy-query xmlns:c="urn:ietf:params:xml:ns:caldav">`+
			`<c:time-range start="20260901T000000Z" end="20260902T000000Z"/></c:free-busy-query>`, "Depth", "1")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "FREEBUSY:20260901T100000Z/20260901T110000Z") {
		t.Fatalf("free-busy-query = %d:\n%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "BUSY-UNAVAILABLE") {
		t.Errorf("free-busy-query applied availability:\n%s", w.Body.String())
	}
}

func TestAvailabilityAdvertised(t *testing.T) {
	srv, _, _ := availabilityServer("principals/user1")
	w := davRequest(t, srv, "OPTIONS", "/calendars/user1/inbox/", "")
	header := w.Header().Get("DAV")
	for _, token := range []string{"calendar-availability", "inbox-availability"} {
		if !strings.Contains(header, token) {
			t.Errorf("DAV header %q lacks %s", header, token)
		}
	}

	// A backend without an availability store does not advertise it.
	principals := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/user1"}}}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, &caldav.MockBackend{}, "calendars"))
	plain := dav.NewServer("/", root)
	plain.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	plain.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	plain.AddPlugin(&caldav.Plugin{})
	plain.AddPlugin(&caldav.SchedulePlugin{})
	w = davRequest(t, plain, "OPTIONS", "/calendars/user1/", "")
	if h := w.Header().Get("DAV"); strings.Contains(h, "availability") {
		t.Errorf("DAV header %q advertises availability without a store", h)
	}
}

// A room with working hours still auto-accepts a booking outside them: the
// auto-schedule conflict check uses plain free-busy (VAVAILABILITY-PLAN.md §1).
func TestAutoScheduleIgnoresAvailability(t *testing.T) {
	srv, backend := newAutoScheduleServer()
	backend.Availabilities = map[string][]byte{"principals/resource1": []byte(workingHours)}

	// Saturday 2026-09-05 22:00Z, far outside the room's hours.
	req := httptest.NewRequest("PUT", "/calendars/user1/default/late.ics",
		strings.NewReader(bookingICS("book-late", "20260905T220000Z", "20260905T230000Z")))
	req.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}
	for _, obj := range backend.Objects[caldav.CalendarID{Calendar: "2"}] {
		if !strings.Contains(strings.ReplaceAll(string(obj.CalendarData), "\r\n ", ""), "PARTSTAT=ACCEPTED:mailto:resource1@example.org") {
			t.Errorf("room declined an out-of-hours booking:\n%s", obj.CalendarData)
		}
	}
	if len(backend.Objects[caldav.CalendarID{Calendar: "2"}]) != 1 {
		t.Errorf("room calendar has %d objects, want 1", len(backend.Objects[caldav.CalendarID{Calendar: "2"}]))
	}
}
