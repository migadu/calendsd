package caldav_test

import (
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
)

type xmlNM = xml.Name

type spyMailer struct {
	from string
	to   []string
	msg  []byte
}

func (m *spyMailer) Send(from string, to []string, msg []byte) error {
	m.from, m.to, m.msg = from, to, msg
	return nil
}

// TestIMipDeliversToExternalAttendee: an invite to a non-local mailto:
// address is emailed rather than delivered locally.
func TestIMipDeliversToExternalAttendee(t *testing.T) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1", Props: map[xmlNM]any{
			{Space: "http://sabredav.org/ns", Local: "email-address"}: "user1@example.org",
		}},
	}}
	backend := &caldav.MockSchedulingBackend{}
	backend.Calendars = []caldav.CalendarInfo{{
		ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1",
	}}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))

	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&caldav.SchedulePlugin{})
	mailer := &spyMailer{}
	srv.AddPlugin(&caldav.IMipPlugin{SenderEmail: "server@example.org", Mailer: mailer})

	invite := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:imip-1\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:Lunch\r\n" +
		"ORGANIZER;CN=User One:mailto:user1@example.org\r\n" +
		"ATTENDEE;CN=External Guest;PARTSTAT=NEEDS-ACTION:mailto:guest@gmail.com\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	req := httptest.NewRequest("PUT", "/calendars/user1/default/e1.ics", strings.NewReader(invite))
	req.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}

	if len(mailer.to) != 1 || mailer.to[0] != "guest@gmail.com" {
		t.Fatalf("email recipient = %v, want [guest@gmail.com]", mailer.to)
	}
	email := string(mailer.msg)
	if !strings.Contains(email, "Subject: Invitation: Lunch") {
		t.Errorf("subject missing:\n%s", email)
	}
	if !strings.Contains(email, "method=REQUEST") || !strings.Contains(email, "BEGIN:VCALENDAR") {
		t.Errorf("iCalendar body/method missing:\n%s", email)
	}
	// The stored object records the iMip delivery status on the attendee.
	get := httptest.NewRequest("GET", "/calendars/user1/default/e1.ics", nil)
	gw := httptest.NewRecorder()
	srv.ServeHTTP(gw, get)
	if !strings.Contains(gw.Body.String(), "SCHEDULE-STATUS=1.1") {
		t.Errorf("stored object missing iMip schedule-status:\n%s", gw.Body.String())
	}
}
