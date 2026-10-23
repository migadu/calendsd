package caldav_test

// Audit 4.2: local iTIP delivery located the recipient's existing copy by UID
// across every calendar under their home, read-only shared instances
// included, and replaced it with the sender's payload. Anyone who knew a UID
// could overwrite or cancel the recipient's own event, and a sharee could be
// used as a conduit to write into the sharer's calendar.

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

const victimOwnEvent = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:mine\r\nDTSTAMP:20260101T000000Z\r\n" +
	"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:My own plans\r\n" +
	"ORGANIZER:mailto:user2@example.org\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

const sharerEvent = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:shared\r\nDTSTAMP:20260101T000000Z\r\n" +
	"DTSTART:20260902T100000Z\r\nDTEND:20260902T110000Z\r\nSUMMARY:Sharer's event\r\n" +
	"ORGANIZER:mailto:user1@example.org\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

// deliveryServer: user1 (attacker/organizer), user2 (victim) with an own
// event, user3 whose calendar user2 can read through a shared instance.
func deliveryServer(loggedIn string) (*dav.Server, *caldav.MockSchedulingBackend) {
	sd := "http://sabredav.org/ns"
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user1@example.org"}},
		{URI: "principals/user2", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user2@example.org"}},
		{URI: "principals/user3", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user3@example.org"}},
	}}
	backend := &caldav.MockSchedulingBackend{}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1"},
		{ID: caldav.CalendarID{Calendar: "2"}, URI: "default", PrincipalURI: "principals/user2"},
		{ID: caldav.CalendarID{Calendar: "3"}, URI: "default", PrincipalURI: "principals/user3", ShareAccess: caldav.AccessSharedOwner},
		{ID: caldav.CalendarID{Calendar: "3"}, URI: "from-user3", PrincipalURI: "principals/user2", ShareAccess: caldav.AccessRead,
			Props: map[xmlNM]any{{Space: sd, Local: "read-only"}: "1"}},
	}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{
		{Calendar: "1"}: {},
		{Calendar: "2"}: {"mine.ics": &caldav.ObjectInfo{URI: "mine.ics", CalendarData: []byte(victimOwnEvent)}},
		{Calendar: "3"}: {"shared.ics": &caldav.ObjectInfo{URI: "shared.ics", CalendarData: []byte(sharerEvent)}},
	}

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

func inviteICS(uid, summary string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:" + uid + "\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:" + summary + "\r\n" +
		"ORGANIZER:mailto:user1@example.org\r\n" +
		"ATTENDEE;PARTSTAT=ACCEPTED:mailto:user1@example.org\r\n" +
		"ATTENDEE:mailto:user2@example.org\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
}

func TestForgedRequestDoesNotOverwriteRecipientsOwnEvent(t *testing.T) {
	srv, backend := deliveryServer("principals/user1")

	req := httptest.NewRequest("PUT", "/calendars/user1/default/evil.ics", strings.NewReader(inviteICS("mine", "Hijacked")))
	req.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}

	victim := backend.Objects[caldav.CalendarID{Calendar: "2"}]
	if len(victim) != 1 || string(victim["mine.ics"].CalendarData) != victimOwnEvent {
		t.Errorf("victim's own event was changed or a copy was added: %d objects, data:\n%s",
			len(victim), victim["mine.ics"].CalendarData)
	}
	if n := len(backend.Scheduling["principals/user2"]); n != 1 {
		t.Errorf("victim inbox has %d items, want 1 (the message is still delivered to the inbox)", n)
	}
	attacker := backend.Objects[caldav.CalendarID{Calendar: "1"}]["evil.ics"]
	if attacker == nil || !strings.Contains(string(attacker.CalendarData), "SCHEDULE-STATUS=3.8") {
		t.Errorf("organizer's copy should carry SCHEDULE-STATUS 3.8 for user2:\n%s", attacker.CalendarData)
	}

	// A forged CANCEL (DELETE of the attacker's object) must not cancel or
	// delete the victim's event either.
	del := httptest.NewRequest("DELETE", "/calendars/user1/default/evil.ics", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, del)
	if w.Code != 204 {
		t.Fatalf("DELETE = %d: %s", w.Code, w.Body.String())
	}
	victim = backend.Objects[caldav.CalendarID{Calendar: "2"}]
	if len(victim) != 1 || string(victim["mine.ics"].CalendarData) != victimOwnEvent {
		t.Errorf("forged CANCEL altered the victim's event:\n%s", victim["mine.ics"].CalendarData)
	}
}

func TestDeliveryNeverWritesThroughSharedInstance(t *testing.T) {
	srv, backend := deliveryServer("principals/user1")

	req := httptest.NewRequest("PUT", "/calendars/user1/default/re.ics", strings.NewReader(inviteICS("shared", "Reissued")))
	req.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}

	sharer := backend.Objects[caldav.CalendarID{Calendar: "3"}]
	if len(sharer) != 1 || string(sharer["shared.ics"].CalendarData) != sharerEvent {
		t.Errorf("sharer's calendar was written through the sharee's read-only instance:\n%v", sharer)
	}
	// The recipient gets their own copy in their own default calendar.
	own := backend.Objects[caldav.CalendarID{Calendar: "2"}]
	found := false
	for _, o := range own {
		if strings.Contains(string(o.CalendarData), "UID:shared") && strings.Contains(string(o.CalendarData), "SUMMARY:Reissued") {
			found = true
		}
	}
	if !found {
		t.Errorf("recipient's own copy of the invite not created in their calendar: %v", own)
	}
	if o := backend.Objects[caldav.CalendarID{Calendar: "1"}]["re.ics"]; o == nil || !strings.Contains(string(o.CalendarData), "SCHEDULE-STATUS=1.2") {
		t.Errorf("legitimate delivery should report 1.2:\n%s", o.CalendarData)
	}
}

// Control: a REQUEST from the same organizer still updates the stored copy.
func TestRequestFromSameOrganizerUpdatesCopy(t *testing.T) {
	srv, backend := deliveryServer("principals/user1")
	put := func(summary string) {
		t.Helper()
		req := httptest.NewRequest("PUT", "/calendars/user1/default/ok.ics", strings.NewReader(inviteICS("ok", summary)))
		req.Header.Set("Content-Type", "text/calendar")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != 201 && w.Code != 204 {
			t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
		}
	}
	put("First")
	put("Second")
	var copies, second int
	for _, o := range backend.Objects[caldav.CalendarID{Calendar: "2"}] {
		if strings.Contains(string(o.CalendarData), "UID:ok") {
			copies++
			if strings.Contains(string(o.CalendarData), "SUMMARY:Second") {
				second++
			}
		}
	}
	if copies != 1 || second != 1 {
		t.Errorf("recipient has %d copies (%d updated), want exactly one updated copy", copies, second)
	}
}
