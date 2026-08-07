package caldav_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	goical "github.com/emersion/go-ical"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	"calserver/internal/metrics"
)

type spyUserMailer struct {
	called           bool
	user, pass, from string
	to               []string
	msg              []byte
	err              error
}

func (m *spyUserMailer) SendAs(user, pass, from string, to []string, msg []byte) error {
	m.called = true
	m.user, m.pass, m.from, m.to, m.msg = user, pass, from, to, msg
	return m.err
}

func sendAsUserMsg(sender, recipient string) *ical.Message {
	ev := &goical.Component{Name: "VEVENT", Props: goical.Props{}}
	ev.Props.SetText("SUMMARY", "Lunch")
	cal := goical.NewCalendar()
	cal.Children = append(cal.Children, ev)
	return &ical.Message{
		Method: "REQUEST", Sender: sender, Recipient: recipient,
		SignificantChange: true, Calendar: cal,
	}
}

// userRequest builds a request as it exists after the auth plugin ran: the
// principal in context, the Basic login header, and the password stashed.
func userRequest(principal, login, password string) *http.Request {
	ctx := dav.ContextWithPrincipal(context.Background(), principal)
	if password != "" {
		dav.SetSubmissionSecret(ctx, password)
	}
	r := httptest.NewRequest("PUT", "/", nil).WithContext(ctx)
	if login != "" {
		r.SetBasicAuth(login, password)
	}
	return r
}

// sendAsUserPlugin is an IMipPlugin wired for the per-user path: alice owns two
// addresses, room1 is a resource, everyone else is a delegated human.
func sendAsUserPlugin(sub caldav.UserMailer, relay caldav.Mailer) *caldav.IMipPlugin {
	return &caldav.IMipPlugin{
		SenderEmail: "server@example.org",
		Mailer:      relay,
		Submission:  sub,
		SenderAddresses: func(principal string) []string {
			if principal == "principals/alice" {
				return []string{"mailto:alice@example.org", "mailto:alice.alias@example.org"}
			}
			return nil
		},
		IsResourceSender: func(addr string) bool { return addr == "mailto:room1@example.org" },
	}
}

func TestDeliverAsUserSendsAsLoggedInUser(t *testing.T) {
	for _, tc := range []struct {
		name, sender, wantFrom string
	}{
		{"primary", "mailto:alice@example.org", "alice@example.org"},
		{"alias", "mailto:alice.alias@example.org", "alice.alias@example.org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := &spyUserMailer{}
			relay := &spyMailer{}
			p := sendAsUserPlugin(sub, relay)
			msg := sendAsUserMsg(tc.sender, "mailto:guest@example.com")
			r := userRequest("principals/alice", "alice@example.org", "hunter2")

			if !p.DeliverExternal(r, msg) {
				t.Fatalf("DeliverExternal returned false; status=%q", msg.ScheduleStatus)
			}
			if !sub.called {
				t.Fatal("submission mailer was not called")
			}
			if relay.msg != nil {
				t.Error("relay mailer was used; want per-user submission only")
			}
			// AUTH is the login; From/envelope is the (possibly alias) sender.
			if sub.user != "alice@example.org" || sub.pass != "hunter2" {
				t.Errorf("auth = (%q, %q), want (alice@example.org, hunter2)", sub.user, sub.pass)
			}
			if sub.from != tc.wantFrom {
				t.Errorf("MAIL FROM = %q, want %q", sub.from, tc.wantFrom)
			}
			if len(sub.to) != 1 || sub.to[0] != "guest@example.com" {
				t.Errorf("recipients = %v, want [guest@example.com]", sub.to)
			}
			if !strings.HasPrefix(msg.ScheduleStatus, "1.1") {
				t.Errorf("status = %q, want 1.1 (sent)", msg.ScheduleStatus)
			}
			if !strings.Contains(string(sub.msg), "From: "+tc.wantFrom) {
				t.Errorf("message From header missing %q:\n%s", tc.wantFrom, sub.msg)
			}
		})
	}
}

func TestDeliverAsUserRejectsDelegatedOrganizer(t *testing.T) {
	sub := &spyUserMailer{}
	relay := &spyMailer{}
	p := sendAsUserPlugin(sub, relay)
	// alice is logged in, but the organizer is bob — neither alice nor a resource.
	msg := sendAsUserMsg("mailto:bob@example.org", "mailto:guest@example.com")
	r := userRequest("principals/alice", "alice@example.org", "hunter2")

	if !p.DeliverExternal(r, msg) {
		t.Fatal("DeliverExternal returned false; want handled")
	}
	if sub.called {
		t.Error("submission mailer called for a delegated organizer; must not send as bob")
	}
	if relay.msg != nil {
		t.Error("relay used for a delegated organizer; must not bypass bob's limits")
	}
	if !strings.HasPrefix(msg.ScheduleStatus, "5.3") {
		t.Errorf("status = %q, want 5.3 (not authorized)", msg.ScheduleStatus)
	}
}

func TestDeliverAsUserRelaysResourceReply(t *testing.T) {
	sub := &spyUserMailer{}
	relay := &spyMailer{}
	p := sendAsUserPlugin(sub, relay)
	// A room auto-replying to a remote organizer. No login for room1.
	msg := sendAsUserMsg("mailto:room1@example.org", "mailto:organizer@remote.example")
	msg.Method = "REPLY"
	// The triggering request was the organizer's; the sender is the resource.
	r := userRequest("principals/alice", "alice@example.org", "hunter2")

	if !p.DeliverExternal(r, msg) {
		t.Fatalf("DeliverExternal returned false; status=%q", msg.ScheduleStatus)
	}
	if sub.called {
		t.Error("submission mailer called for a resource reply; must relay instead")
	}
	if relay.msg == nil {
		t.Fatal("relay mailer not used for the resource reply")
	}
	if relay.from != "server@example.org" {
		t.Errorf("relay envelope sender = %q, want server@example.org", relay.from)
	}
	if !strings.HasPrefix(msg.ScheduleStatus, "1.1") {
		t.Errorf("status = %q, want 1.1 (sent)", msg.ScheduleStatus)
	}
}

func TestDeliverAsUserNoCredential(t *testing.T) {
	sub := &spyUserMailer{}
	p := sendAsUserPlugin(sub, &spyMailer{})
	msg := sendAsUserMsg("mailto:alice@example.org", "mailto:guest@example.com")
	// Principal is set (e.g. a Digest login) but no Basic password was stashed.
	r := httptest.NewRequest("PUT", "/", nil).WithContext(
		dav.ContextWithPrincipal(context.Background(), "principals/alice"))

	if !p.DeliverExternal(r, msg) {
		t.Fatal("DeliverExternal returned false; want handled")
	}
	if sub.called {
		t.Error("submission attempted without a credential")
	}
	if !strings.HasPrefix(msg.ScheduleStatus, "5.3") {
		t.Errorf("status = %q, want 5.3 (no credential)", msg.ScheduleStatus)
	}
}

// submissionServer wires a full server stack — real Basic auth (user1/pw →
// principals/user1), schedule, and an IMipPlugin whose per-user submission goes
// to sub and whose relay goes to relay — so a request exercises the whole chain
// (auth stash → schedule → deliver → DeliverExternal → submission).
func submissionServer(sub caldav.UserMailer, relay caldav.Mailer) *dav.Server {
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
	srv.AddPlugin(auth.NewPlugin(&auth.BasicBackend{
		Realm:    "SabreDAV",
		Validate: func(u, pw string) bool { return u == "user1" && pw == "pw" },
	}))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	schedule := &caldav.SchedulePlugin{}
	srv.AddPlugin(schedule)
	imip := &caldav.IMipPlugin{SenderEmail: "server@example.org", Mailer: relay, Submission: sub}
	imip.UseScheduler(schedule)
	srv.AddPlugin(imip)
	return srv
}

const submissionInvite = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:sau-1\r\nDTSTAMP:20260101T000000Z\r\n" +
	"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:Lunch\r\n" +
	"ORGANIZER;CN=User One:mailto:user1@example.org\r\n" +
	"ATTENDEE;CN=External Guest;PARTSTAT=NEEDS-ACTION:mailto:guest@gmail.com\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

// TestIMipSubmissionSendsAsLoggedInUser is the end-to-end proof: a Basic-
// authenticated PUT that invites an external guest is submitted as the logged-in
// user — AUTH is the login, password the one they presented, From the organizer.
func TestIMipSubmissionSendsAsLoggedInUser(t *testing.T) {
	sub := &spyUserMailer{}
	relay := &spyMailer{}
	srv := submissionServer(sub, relay)

	req := httptest.NewRequest("PUT", "/calendars/user1/default/e1.ics", strings.NewReader(submissionInvite))
	req.Header.Set("Content-Type", "text/calendar")
	req.SetBasicAuth("user1", "pw")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}

	if !sub.called {
		t.Fatal("per-user submission was not used")
	}
	if relay.msg != nil {
		t.Error("shared relay was used; want per-user submission")
	}
	if sub.user != "user1" || sub.pass != "pw" {
		t.Errorf("AUTH = (%q, %q), want the Basic login (user1, pw)", sub.user, sub.pass)
	}
	if sub.from != "user1@example.org" {
		t.Errorf("MAIL FROM = %q, want the organizer user1@example.org", sub.from)
	}
	if len(sub.to) != 1 || sub.to[0] != "guest@gmail.com" {
		t.Errorf("recipients = %v, want [guest@gmail.com]", sub.to)
	}
	email := string(sub.msg)
	if !strings.Contains(email, "From: User One <user1@example.org>") || !strings.Contains(email, "method=REQUEST") {
		t.Errorf("message should be a REQUEST from the organizer:\n%s", email)
	}
}

// TestIMipSubmissionCancelViaDelete: deleting the event (BeforeUnbind) sends the
// CANCEL through the same per-user submission path, as the logged-in user.
func TestIMipSubmissionCancelViaDelete(t *testing.T) {
	sub := &spyUserMailer{}
	srv := submissionServer(sub, &spyMailer{})

	put := httptest.NewRequest("PUT", "/calendars/user1/default/e1.ics", strings.NewReader(submissionInvite))
	put.Header.Set("Content-Type", "text/calendar")
	put.SetBasicAuth("user1", "pw")
	if w := httptest.NewRecorder(); func() int { srv.ServeHTTP(w, put); return w.Code }() != 201 {
		t.Fatalf("PUT failed")
	}

	del := httptest.NewRequest("DELETE", "/calendars/user1/default/e1.ics", nil)
	del.SetBasicAuth("user1", "pw")
	dw := httptest.NewRecorder()
	srv.ServeHTTP(dw, del)
	if dw.Code != 204 {
		t.Fatalf("DELETE = %d: %s", dw.Code, dw.Body.String())
	}

	email := string(sub.msg)
	if !strings.Contains(email, "method=CANCEL") {
		t.Errorf("delete should submit a CANCEL as the user:\n%s", email)
	}
	if sub.user != "user1" || sub.from != "user1@example.org" {
		t.Errorf("CANCEL AUTH/From = (%q, %q), want (user1, user1@example.org)", sub.user, sub.from)
	}
}

// TestIMipSendAsUserOutcomes pins the per-user path's outcome labels so each
// branch stays a distinct series — a rate-limited or delegated send must not
// fold into the alert-worthy smtp_error/auth_error series.
func TestIMipSendAsUserOutcomes(t *testing.T) {
	set := metrics.New("test")
	run := func(sub *spyUserMailer, sender string, r *http.Request) {
		p := sendAsUserPlugin(sub, &spyMailer{})
		p.Metrics = set
		p.DeliverExternal(r, sendAsUserMsg(sender, "mailto:guest@example.com"))
	}
	alice := func() *http.Request { return userRequest("principals/alice", "alice@example.org", "hunter2") }
	noCred := httptest.NewRequest("PUT", "/", nil).WithContext(
		dav.ContextWithPrincipal(context.Background(), "principals/alice"))

	run(&spyUserMailer{}, "mailto:alice@example.org", alice())                                 // sent_as_user
	run(&spyUserMailer{}, "mailto:bob@example.org", alice())                                   // not_authorized_sender
	run(&spyUserMailer{}, "mailto:room1@example.org", alice())                                 // resource_relay
	run(&spyUserMailer{}, "mailto:alice@example.org", noCred)                                  // no_credential
	run(&spyUserMailer{err: &textproto.Error{Code: 535}}, "mailto:alice@example.org", alice()) // auth_error
	run(&spyUserMailer{err: &textproto.Error{Code: 450}}, "mailto:alice@example.org", alice()) // rate_limited

	want := `
# HELP calserver_imip_deliveries_total External iTIP deliveries by outcome; smtp_error, auth_error, and retry_exhausted are alert-worthy (delivery/auth infrastructure failures). rate_limited, not_authorized_sender, and no_credential are expected client-side outcomes. A message that is queued for retry counts once as queued and once more at its terminal outcome.
# TYPE calserver_imip_deliveries_total counter
calserver_imip_deliveries_total{outcome="auth_error"} 1
calserver_imip_deliveries_total{outcome="no_credential"} 1
calserver_imip_deliveries_total{outcome="not_authorized_sender"} 1
calserver_imip_deliveries_total{outcome="rate_limited"} 1
calserver_imip_deliveries_total{outcome="resource_relay"} 1
calserver_imip_deliveries_total{outcome="sent_as_user"} 1
`
	if err := testutil.GatherAndCompare(set.Registry(), strings.NewReader(want),
		"calserver_imip_deliveries_total"); err != nil {
		t.Error(err)
	}
}

func TestDeliverAsUserErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus string
	}{
		{"auth", &textproto.Error{Code: 535, Msg: "auth failed"}, "5.3"},
		{"ratelimit", &textproto.Error{Code: 450, Msg: "too many"}, "5.3"},
		{"other", &textproto.Error{Code: 550, Msg: "rejected"}, "5.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := &spyUserMailer{err: tc.err}
			p := sendAsUserPlugin(sub, &spyMailer{})
			msg := sendAsUserMsg("mailto:alice@example.org", "mailto:guest@example.com")
			r := userRequest("principals/alice", "alice@example.org", "hunter2")

			if !p.DeliverExternal(r, msg) {
				t.Fatal("DeliverExternal returned false; want handled")
			}
			if !strings.HasPrefix(msg.ScheduleStatus, tc.wantStatus) {
				t.Errorf("status = %q, want prefix %q", msg.ScheduleStatus, tc.wantStatus)
			}
		})
	}
}
