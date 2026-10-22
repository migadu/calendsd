package caldav_test

import (
	"mime"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
	"time"

	goical "github.com/emersion/go-ical"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
)

// TestIMipHeaderInjectionStripped verifies that CR/LF in attacker-controlled
// calendar data (the event SUMMARY and the organizer display name) cannot
// inject extra email headers into an outgoing iMIP message.
func TestIMipHeaderInjectionStripped(t *testing.T) {
	ev := &goical.Component{Name: "VEVENT", Props: goical.Props{}}
	ev.Props.SetText("SUMMARY", "Lunch\r\nBcc: victim@evil.example")
	cal := goical.NewCalendar()
	cal.Children = append(cal.Children, ev)

	msg := &ical.Message{
		Method:            "REQUEST",
		Sender:            "mailto:organizer@example.org",
		SenderName:        "Organizer\r\nX-Injected: yes",
		Recipient:         "mailto:guest@example.com",
		SignificantChange: true,
		Calendar:          cal,
	}
	spy := &spyMailer{}
	plugin := &caldav.IMipPlugin{SenderEmail: "server@example.org", Mailer: spy}
	if !plugin.DeliverExternal(httptest.NewRequest("POST", "/", nil), msg) {
		t.Fatalf("DeliverExternal returned false; status=%q", msg.ScheduleStatus)
	}

	email := string(spy.msg)
	headerBlock := email
	if i := strings.Index(email, "\r\n\r\n"); i >= 0 {
		headerBlock = email[:i]
	}
	for _, line := range strings.Split(headerBlock, "\r\n") {
		low := strings.ToLower(line)
		if strings.HasPrefix(low, "bcc:") || strings.HasPrefix(low, "x-injected:") {
			t.Errorf("injected header line survived: %q\nfull headers:\n%s", line, headerBlock)
		}
	}
	// The subject should still carry the (sanitized, single-line) summary.
	if !strings.Contains(headerBlock, "Subject: Invitation: Lunch") {
		t.Errorf("sanitized subject missing:\n%s", headerBlock)
	}
}

// TestIMipHeadersEncoded verifies that non-ASCII names and summaries are
// RFC 2047-encoded, and that the message carries Date and Message-ID.
func TestIMipHeadersEncoded(t *testing.T) {
	ev := &goical.Component{Name: "VEVENT", Props: goical.Props{}}
	ev.Props.SetText("SUMMARY", "Café \"Zürich\"")
	cal := goical.NewCalendar()
	cal.Children = append(cal.Children, ev)
	msg := &ical.Message{
		Method:            "REQUEST",
		Sender:            "mailto:organizer@example.org",
		SenderName:        "Jörg, Müller",
		Recipient:         "mailto:guest@example.com",
		RecipientName:     "Guest <x>",
		SignificantChange: true,
		Calendar:          cal,
	}
	spy := &spyMailer{}
	now := time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)
	plugin := &caldav.IMipPlugin{SenderEmail: "server@example.org", Mailer: spy, Now: func() time.Time { return now }}
	if !plugin.DeliverExternal(httptest.NewRequest("POST", "/", nil), msg) {
		t.Fatalf("DeliverExternal returned false; status=%q", msg.ScheduleStatus)
	}

	m, err := mail.ReadMessage(strings.NewReader(string(spy.msg)))
	if err != nil {
		t.Fatalf("message does not parse: %v\n%s", err, spy.msg)
	}
	for _, raw := range []string{m.Header.Get("Subject"), m.Header.Get("Reply-To")} {
		for _, r := range raw {
			if r > 0x7e {
				t.Errorf("header carries raw non-ASCII: %q", raw)
				break
			}
		}
	}
	dec := new(mime.WordDecoder)
	if subject, _ := dec.DecodeHeader(m.Header.Get("Subject")); subject != "Invitation: Café \"Zürich\"" {
		t.Errorf("Subject decodes to %q", subject)
	}
	if replyTo, err := mail.ParseAddress(m.Header.Get("Reply-To")); err != nil || replyTo.Name != "Jörg, Müller" || replyTo.Address != "organizer@example.org" {
		t.Errorf("Reply-To = %q (%v)", m.Header.Get("Reply-To"), err)
	}
	if to, err := mail.ParseAddress(m.Header.Get("To")); err != nil || to.Name != "Guest <x>" || to.Address != "guest@example.com" {
		t.Errorf("To = %q (%v)", m.Header.Get("To"), err)
	}
	if date, err := m.Header.Date(); err != nil || !date.Equal(now) {
		t.Errorf("Date = %q (%v)", m.Header.Get("Date"), err)
	}
	if id := m.Header.Get("Message-ID"); !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@example.org>") {
		t.Errorf("Message-ID = %q", id)
	}
}

// TestCalendarQuerySkipsMalformedObject verifies that a single stored object
// with a malformed DURATION does not turn a whole calendar-query REPORT into a
// 500 — the bad object is skipped and valid ones still match.
func TestCalendarQuerySkipsMalformedObject(t *testing.T) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1"},
	}}
	backend := &caldav.MockSchedulingBackend{}
	calID := caldav.CalendarID{Calendar: "1"}
	backend.Calendars = []caldav.CalendarInfo{{
		ID: calID, URI: "default", PrincipalURI: "principals/user1",
	}}
	bad := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:bad\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260901T100000Z\r\nDURATION:PT1X\r\nSUMMARY:Bad\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	good := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:good\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:Good\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{
		calID: {
			"bad.ics":  {URI: "bad.ics", CalendarID: calID, CalendarData: []byte(bad)},
			"good.ics": {URI: "good.ics", CalendarID: calID, CalendarData: []byte(good)},
		},
	}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})

	report := `<?xml version="1.0"?>` +
		`<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">` +
		`<d:prop><d:getetag/></d:prop>` +
		`<c:filter><c:comp-filter name="VCALENDAR">` +
		`<c:comp-filter name="VEVENT">` +
		`<c:time-range start="20260101T000000Z" end="20270101T000000Z"/>` +
		`</c:comp-filter></c:comp-filter></c:filter></c:calendar-query>`
	req := httptest.NewRequest("REPORT", "/calendars/user1/default/", strings.NewReader(report))
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Depth", "1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 207 {
		t.Fatalf("REPORT = %d, want 207 (bad object should be skipped, not 500):\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "good.ics") {
		t.Errorf("valid object missing from result:\n%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "bad.ics") {
		t.Errorf("malformed object should have been skipped:\n%s", w.Body.String())
	}
}

// TestParseCalendarQueryRejectsDeepNesting verifies the nesting-depth guard
// rejects a deeply-nested filter body (stack-exhaustion DoS) while still
// accepting a normal shallow query.
func TestParseCalendarQueryRejectsDeepNesting(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><c:filter>`)
	const depth = 400
	for i := 0; i < depth; i++ {
		b.WriteString(`<c:comp-filter name="X">`)
	}
	for i := 0; i < depth; i++ {
		b.WriteString(`</c:comp-filter>`)
	}
	b.WriteString(`</c:filter></c:calendar-query>`)
	if _, err := caldav.ParseCalendarQuery([]byte(b.String())); err == nil {
		t.Fatal("expected deeply-nested calendar-query to be rejected")
	}

	ok := `<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">` +
		`<d:prop><d:getetag/></d:prop><c:filter><c:comp-filter name="VCALENDAR">` +
		`<c:comp-filter name="VEVENT"/></c:comp-filter></c:filter></c:calendar-query>`
	if _, err := caldav.ParseCalendarQuery([]byte(ok)); err != nil {
		t.Fatalf("valid shallow query rejected: %v", err)
	}
}

// TestCalendarQueryRejectsInvertedTimeRange verifies a time-range whose end is
// not after its start is a parse error (400), matching sabre's CompFilter /
// PropFilter "The end-date must be larger than the start-date". An open-ended
// range (only one bound) stays valid.
func TestCalendarQueryRejectsInvertedTimeRange(t *testing.T) {
	inverted := `<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">` +
		`<d:prop><d:getetag/></d:prop><c:filter><c:comp-filter name="VCALENDAR">` +
		`<c:comp-filter name="VEVENT">` +
		`<c:time-range start="20260201T000000Z" end="20260101T000000Z"/>` +
		`</c:comp-filter></c:comp-filter></c:filter></c:calendar-query>`
	if _, err := caldav.ParseCalendarQuery([]byte(inverted)); err == nil {
		t.Error("expected inverted time-range (end <= start) to be rejected")
	}

	openEnded := `<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">` +
		`<d:prop><d:getetag/></d:prop><c:filter><c:comp-filter name="VCALENDAR">` +
		`<c:comp-filter name="VEVENT">` +
		`<c:time-range start="20260101T000000Z"/>` +
		`</c:comp-filter></c:comp-filter></c:filter></c:calendar-query>`
	if _, err := caldav.ParseCalendarQuery([]byte(openEnded)); err != nil {
		t.Errorf("open-ended time-range must stay valid: %v", err)
	}
}

// TestCalendarQueryRejectsUnknownCollation verifies an unsupported text-match
// collation is a parse error (400), matching sabre's StringUtil::textMatch,
// rather than being silently treated as case-insensitive.
func TestCalendarQueryRejectsUnknownCollation(t *testing.T) {
	q := `<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">` +
		`<d:prop><d:getetag/></d:prop><c:filter><c:comp-filter name="VCALENDAR">` +
		`<c:comp-filter name="VEVENT"><c:prop-filter name="SUMMARY">` +
		`<c:text-match collation="i;made-up">x</c:text-match>` +
		`</c:prop-filter></c:comp-filter></c:comp-filter></c:filter></c:calendar-query>`
	if _, err := caldav.ParseCalendarQuery([]byte(q)); err == nil {
		t.Error("expected unknown collation to be rejected")
	}
}
