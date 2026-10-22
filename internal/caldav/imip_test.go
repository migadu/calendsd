package caldav_test

import (
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/caldav/ical"
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

// TestIMipSendAsOrganizer: with SendAsOrganizer, the From header carries the
// organizer's own address while the SMTP envelope sender stays the server's
// address (a split sender), and no now-redundant Reply-To is emitted.
func TestIMipSendAsOrganizer(t *testing.T) {
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
	srv.AddPlugin(&caldav.IMipPlugin{SenderEmail: "server@example.org", SendAsOrganizer: true, Mailer: mailer})

	invite := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:imip-2\r\nDTSTAMP:20260101T000000Z\r\n" +
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

	// Envelope sender (MAIL FROM) stays the server address.
	if mailer.from != "server@example.org" {
		t.Errorf("envelope sender = %q, want server@example.org", mailer.from)
	}
	email := string(mailer.msg)
	if !strings.Contains(email, `From: "User One" <user1@example.org>`) {
		t.Errorf("From header should carry the organizer:\n%s", email)
	}
	if strings.Contains(email, "Reply-To:") {
		t.Errorf("Reply-To should be omitted when From is the organizer:\n%s", email)
	}
}

// TestIMipSendAsOrganizerMalformedSender: with SendAsOrganizer, a malformed
// sender address (a mis-provisioned principal) falls back to the server From —
// with the sender still reachable via Reply-To — so a strict relay that would
// reject an invalid From still accepts the message.
func TestIMipSendAsOrganizerMalformedSender(t *testing.T) {
	cal, err := ical.Decode([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"METHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:x\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260901T100000Z\r\nSUMMARY:Lunch\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	mailer := &spyMailer{}
	p := &caldav.IMipPlugin{SenderEmail: "server@example.org", SendAsOrganizer: true, Mailer: mailer}
	msg := &ical.Message{
		Method:            "REQUEST",
		Sender:            "mailto:not-an-email",
		Recipient:         "mailto:guest@example.com",
		SignificantChange: true,
		Calendar:          cal,
	}
	if !p.DeliverExternal(httptest.NewRequest("POST", "/", nil), msg) {
		t.Fatal("DeliverExternal returned false, want true")
	}
	// Envelope sender is unchanged; only the From framing falls back.
	if mailer.from != "server@example.org" {
		t.Errorf("envelope sender = %q, want server@example.org", mailer.from)
	}
	email := string(mailer.msg)
	if !strings.Contains(email, "From: <server@example.org>") {
		t.Errorf("From should fall back to the server address:\n%s", email)
	}
	if !strings.Contains(email, "Reply-To: not-an-email") {
		t.Errorf("Reply-To should carry the sender under fallback:\n%s", email)
	}
}
