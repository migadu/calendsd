package caldav_test

// Audit 4.4: one PUT or DELETE fanned out one iTIP message, and for external
// addresses one email, per ATTENDEE with no bound but the group-expansion
// cap. The cap is now enforced on PUT (403 max-attendees-per-instance) and
// backstopped at fan-out for objects that already exceed it.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"calendsd/internal/acl"
	"calendsd/internal/auth"
	"calendsd/internal/caldav"
	"calendsd/internal/caldav/ical"
	"calendsd/internal/dav"
	"calendsd/internal/dav/mock"
)

// recordingDeliverer stands in for the iMIP plugin: it records every
// external message and claims delivery.
type recordingDeliverer struct {
	mu   sync.Mutex
	msgs []*ical.Message
}

func (d *recordingDeliverer) PluginName() string { return "recording-deliverer" }
func (d *recordingDeliverer) Init(*dav.Server)   {}
func (d *recordingDeliverer) DeliverExternal(_ *http.Request, msg *ical.Message) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.msgs = append(d.msgs, msg)
	msg.ScheduleStatus = "1.1;Scheduling message is sent via iMip"
	return true
}

func (d *recordingDeliverer) count(method string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, m := range d.msgs {
		if m.Method == method {
			n++
		}
	}
	return n
}

func manyAttendeesICS(uid string, n int) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:" + uid + "\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:Big\r\n" +
		"ORGANIZER:mailto:user1@example.org\r\n" +
		"ATTENDEE;PARTSTAT=ACCEPTED:mailto:user1@example.org\r\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "ATTENDEE:mailto:guest%d@elsewhere.example\r\n", i)
	}
	b.WriteString("END:VEVENT\r\nEND:VCALENDAR\r\n")
	return b.String()
}

const testAttendeeCap = 5

func attendeeCapServer(seed map[string]string) (*dav.Server, *caldav.MockSchedulingBackend, *recordingDeliverer) {
	sd := "http://sabredav.org/ns"
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user1@example.org"}},
	}}
	backend := &caldav.MockSchedulingBackend{}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1"},
	}
	objs := map[string]*caldav.ObjectInfo{}
	for uri, data := range seed {
		objs[uri] = &caldav.ObjectInfo{URI: uri, CalendarData: []byte(data)}
	}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{{Calendar: "1"}: objs}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{MaxAttendees: testAttendeeCap})
	srv.AddPlugin(&caldav.SchedulePlugin{MaxAttendees: testAttendeeCap})
	deliverer := &recordingDeliverer{}
	srv.AddPlugin(deliverer)
	return srv, backend, deliverer
}

func TestPutRefusesTooManyAttendees(t *testing.T) {
	srv, backend, deliverer := attendeeCapServer(nil)
	// The organizer is also an attendee, so cap-1 guests is exactly the cap.
	put := func(uri string, guests int) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/calendars/user1/default/"+uri, strings.NewReader(manyAttendeesICS(uri, guests)))
		req.Header.Set("Content-Type", "text/calendar")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}

	w := put("over.ics", testAttendeeCap) // cap+1 distinct attendees
	if w.Code != 403 || !strings.Contains(w.Body.String(), "max-attendees-per-instance") {
		t.Fatalf("over-cap PUT = %d, want 403 with the max-attendees-per-instance precondition:\n%s", w.Code, w.Body.String())
	}
	if backend.Objects[caldav.CalendarID{Calendar: "1"}]["over.ics"] != nil {
		t.Error("over-cap object was stored")
	}
	if deliverer.count("REQUEST") != 0 {
		t.Errorf("%d invitations sent for a refused PUT", deliverer.count("REQUEST"))
	}

	w = put("ok.ics", testAttendeeCap-1) // exactly the cap
	if w.Code != 201 {
		t.Fatalf("at-cap PUT = %d: %s", w.Code, w.Body.String())
	}
	if got := deliverer.count("REQUEST"); got != testAttendeeCap-1 {
		t.Errorf("%d invitations sent, want %d", got, testAttendeeCap-1)
	}
	stored := string(backend.Objects[caldav.CalendarID{Calendar: "1"}]["ok.ics"].CalendarData)
	if n := strings.Count(stored, "SCHEDULE-STATUS=1.1"); n != testAttendeeCap-1 {
		t.Errorf("stored object carries %d sent statuses, want %d:\n%s", n, testAttendeeCap-1, stored)
	}
}

// An object that already exceeds the cap (written by sabre, or before the
// cap existed) fans out at most the cap on DELETE, and its excess attendees
// are reported as 5.3 rather than silently skipped on a rewrite.
func TestAttendeeCapBackstopOnExistingObject(t *testing.T) {
	const existing = testAttendeeCap + 3 // guests; plus the organizer
	srv, backend, deliverer := attendeeCapServer(map[string]string{"big.ics": manyAttendeesICS("big", existing)})

	del := httptest.NewRequest("DELETE", "/calendars/user1/default/big.ics", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, del)
	if w.Code != 204 {
		t.Fatalf("DELETE = %d: %s", w.Code, w.Body.String())
	}
	if got := deliverer.count("CANCEL"); got != testAttendeeCap-1 {
		t.Errorf("%d CANCELs sent for a %d-guest object, want %d (cap minus the organizer)", got, existing, testAttendeeCap-1)
	}

	// Rewriting the over-cap object as its organizer is refused outright.
	req := httptest.NewRequest("PUT", "/calendars/user1/default/big.ics",
		strings.NewReader(strings.Replace(manyAttendeesICS("big", existing), "SUMMARY:Big", "SUMMARY:Bigger", 1)))
	req.Header.Set("Content-Type", "text/calendar")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("organizer rewrite of an over-cap object = %d, want 403: %s", w.Code, w.Body.String())
	}
	_ = backend
}

// An attendee's own copy of a large meeting (delivered to them; organized
// elsewhere) is theirs to save: accepting it sends one REPLY, not a fan-out,
// so the cap must not apply and nothing may be marked 5.3 on their copy.
func TestAttendeeCopyOfLargeMeetingIsWritable(t *testing.T) {
	copy := strings.Replace(manyAttendeesICS("theirs", testAttendeeCap+10),
		"ORGANIZER:mailto:user1@example.org", "ORGANIZER:mailto:boss@elsewhere.example", 1)
	srv, backend, deliverer := attendeeCapServer(map[string]string{"theirs.ics": copy})

	accepted := strings.Replace(copy, "ATTENDEE;PARTSTAT=ACCEPTED:mailto:user1@example.org",
		"ATTENDEE;PARTSTAT=ACCEPTED;X-TOUCHED=1:mailto:user1@example.org", 1)
	req := httptest.NewRequest("PUT", "/calendars/user1/default/theirs.ics", strings.NewReader(accepted))
	req.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("attendee PUT of their own copy = %d, want 204: %s", w.Code, w.Body.String())
	}
	stored := string(backend.Objects[caldav.CalendarID{Calendar: "1"}]["theirs.ics"].CalendarData)
	if strings.Contains(stored, "SCHEDULE-STATUS=5.3") {
		t.Errorf("attendee's copy was marked 5.3:\n%s", stored)
	}
	if n := deliverer.count("REQUEST"); n != 0 {
		t.Errorf("%d REQUESTs sent from an attendee's copy", n)
	}
}

func TestMaxAttendeesAdvertised(t *testing.T) {
	srv, _, _ := attendeeCapServer(nil)
	w := davRequest(t, srv, "PROPFIND", "/calendars/user1/default/",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`+
			`<d:prop><c:max-attendees-per-instance/></d:prop></d:propfind>`, "Depth", "0")
	if w.Code != 207 || !strings.Contains(w.Body.String(), fmt.Sprintf(">%d<", testAttendeeCap)) {
		t.Errorf("PROPFIND max-attendees-per-instance = %d:\n%s", w.Code, w.Body.String())
	}
}
