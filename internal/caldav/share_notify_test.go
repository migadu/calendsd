package caldav_test

// Share-invite email (docs/share-invite-email.md): which {cs}share requests
// produce a notice, how the ShareMailer screens and enqueues it, how the
// durable worker revalidates before sending, and what the email looks like.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	"calserver/internal/jobs"
	"calserver/internal/metrics"
)

// captureNotifier records every notice the sharing plugin hands over.
type captureNotifier struct{ notices []caldav.ShareNotice }

func (c *captureNotifier) NotifyShare(_ *http.Request, n caldav.ShareNotice) {
	c.notices = append(c.notices, n)
}

// shareServer is the sharedSharing fixture with a notifier on user1's server.
func shareServer(t *testing.T, notifier caldav.ShareNotifier) *dav.Server {
	t.Helper()
	dn := xmlNM{Space: "DAV:", Local: "displayname"}
	email := xmlNM{Space: "http://sabredav.org/ns", Local: "email-address"}
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1", Props: map[xmlNM]any{dn: "User One", email: "user1@example.org"}},
		{URI: "principals/user2", Props: map[xmlNM]any{dn: "User Two", email: "user2@example.org"}},
	}}
	backend := &caldav.MockSharingBackend{Shares: map[caldav.CalendarID][]caldav.Sharee{}}
	backend.Calendars = []caldav.CalendarInfo{{
		ID: caldav.CalendarID{Calendar: "1"}, URI: "cal1", PrincipalURI: "principals/user1",
	}}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&caldav.SharingPlugin{Notifier: notifier})
	srv.AddPlugin(&caldav.NotificationsPlugin{})
	return srv
}

func sharePOST(t *testing.T, srv *dav.Server, body string) {
	t.Helper()
	doc := `<?xml version="1.0"?><cs:share xmlns:cs="http://calendarserver.org/ns/" xmlns:d="DAV:">` + body + `</cs:share>`
	if w := req(t, srv, "POST", "/calendars/user1/cal1", doc, "Content-Type", "text/xml"); w.Code != 200 {
		t.Fatalf("share POST = %d: %s", w.Code, w.Body.String())
	}
}

const (
	setUser2   = `<cs:set><d:href>mailto:user2@example.org</d:href><cs:summary>Team rota</cs:summary></cs:set>`
	setUser2RW = `<cs:set><d:href>mailto:user2@example.org</d:href><cs:read-write/></cs:set>`
	rmUser2    = `<cs:remove><d:href>mailto:user2@example.org</d:href></cs:remove>`
)

// TestShareNoticeOnlyForNewLocalShares walks the "when an email is sent"
// table: only a new usable local share produces a notice.
func TestShareNoticeOnlyForNewLocalShares(t *testing.T) {
	c := &captureNotifier{}
	srv := shareServer(t, c)

	sharePOST(t, srv, setUser2)
	if len(c.notices) != 1 {
		t.Fatalf("new local sharee: notices = %d, want 1", len(c.notices))
	}
	n := c.notices[0]
	if n.Principal != "principals/user2" || n.OwnerPrincipal != "principals/user1" ||
		n.CalendarID != "1" || n.InstanceURI == "" || n.Comment != "Team rota" {
		t.Fatalf("notice = %+v", n)
	}

	steps := []struct{ name, body string }{
		{"re-POST of the same roster", setUser2},
		{"alias spelling of the same sharee", `<cs:set><d:href>mailto:USER2@example.org</d:href></cs:set>`},
		{"access change", setUser2RW},
		{"unresolved address", `<cs:set><d:href>mailto:nobody@example.org</d:href></cs:set>`},
		{"the owner themselves", `<cs:set><d:href>mailto:user1@example.org</d:href></cs:set>`},
		{"removal", rmUser2},
		{"set and remove in one POST", setUser2 + rmUser2},
	}
	for _, s := range steps {
		before := len(c.notices)
		sharePOST(t, srv, s.body)
		if got := len(c.notices) - before; got != 0 {
			t.Errorf("%s: %d new notice(s), want 0", s.name, got)
		}
	}

	// Removed above, so sharing again is a genuinely new share.
	sharePOST(t, srv, setUser2)
	if len(c.notices) != 2 {
		t.Fatalf("re-share after removal: notices = %d, want 2", len(c.notices))
	}
}

// ---- ShareMailer ----------------------------------------------------------

type fakePolicy struct {
	byDomain map[string]string
	err      error
	asked    []string
}

func (f *fakePolicy) ShareNotify(ctx context.Context, domain string) (string, error) {
	f.asked = append(f.asked, domain)
	if f.err != nil {
		return "", f.err
	}
	if v, ok := f.byDomain[domain]; ok {
		return v, nil
	}
	return "off", nil
}

type fakeShares struct {
	inst *caldav.SharedInstance
	err  error
}

func (f *fakeShares) SharedInstance(ctx context.Context, calendarID, principal string) (*caldav.SharedInstance, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.inst == nil {
		return nil, nil
	}
	cp := *f.inst
	return &cp, nil
}

type latestCall struct {
	worker, key string
	generation  int64
	args        []byte
}

type fakeLatestQueue struct {
	calls []latestCall
	err   error
}

func (f *fakeLatestQueue) EnqueueLatest(_ context.Context, worker, key string, generation int64, args any, _ ...jobs.Opt) error {
	if f.err != nil {
		return f.err
	}
	raw, _ := json.Marshal(args)
	f.calls = append(f.calls, latestCall{worker, key, generation, raw})
	return nil
}

type shareFixture struct {
	mailer   *caldav.ShareMailer
	policy   *fakePolicy
	shares   *fakeShares
	queue    *fakeLatestQueue
	spy      *spyMailer
	metrics  *metrics.Set
	notice   caldav.ShareNotice
	resource map[string]bool
}

func newShareFixture() *shareFixture {
	dn := xmlNM{Space: "DAV:", Local: "displayname"}
	email := xmlNM{Space: "http://sabredav.org/ns", Local: "email-address"}
	f := &shareFixture{
		policy: &fakePolicy{byDomain: map[string]string{"example.org": "email"}},
		shares: &fakeShares{inst: &caldav.SharedInstance{
			ID: 7, URI: "abc-123", Access: caldav.AccessRead,
			OwnerPrincipal: "principals/alice", CalendarName: "Work",
		}},
		queue:    &fakeLatestQueue{},
		spy:      &spyMailer{},
		metrics:  metrics.New("t"),
		resource: map[string]bool{},
		notice: caldav.ShareNotice{
			CalendarID: "42", CalendarPath: "calendars/alice/work", OwnerPrincipal: "principals/alice",
			Principal: "principals/bob", InstanceID: 7, InstanceURI: "abc-123", Comment: "Please add the rota",
		},
	}
	f.mailer = &caldav.ShareMailer{
		Mailer: f.spy, SenderEmail: "calendar@server.example", WebURL: "https://calendar.example.org/",
		Policy: f.policy, Shares: f.shares,
		Principals: &acl.MemoryBackend{Principals: []acl.Principal{
			{URI: "principals/alice", Props: map[xmlNM]any{dn: "Alice Example", email: "alice@example.org"}},
			// The canonical mailbox deliberately differs in case from any
			// address a sharer might have typed.
			{URI: "principals/bob", Props: map[xmlNM]any{dn: "Bob", email: "Bob@Example.org"}},
			{URI: "principals/room1", Props: map[xmlNM]any{email: "room1@example.org"}},
			{URI: "principals/noaddr", Props: map[xmlNM]any{dn: "No Address"}},
		}},
		IsResource: func(p string) bool { return f.resource[p] },
		Durable:    f.queue,
		Metrics:    f.metrics,
		Now:        func() time.Time { return time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC) },
	}
	return f
}

// shareCount reads calserver_share_notify_total{outcome}.
func shareCount(t *testing.T, set *metrics.Set, outcome string) float64 {
	t.Helper()
	mfs, err := set.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "calserver_share_notify_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "outcome" && l.GetValue() == outcome {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// TestShareMailerEnqueuesCanonicalMailbox: an opted-in recipient is queued
// once, keyed per (calendar, principal) with the instance id as generation,
// addressed to the principal's canonical mailbox — and the policy is looked
// up on that mailbox's lowercased domain.
func TestShareMailerEnqueuesCanonicalMailbox(t *testing.T) {
	f := newShareFixture()
	f.mailer.NotifyShare(httptest.NewRequest("POST", "/", nil), f.notice)

	if len(f.queue.calls) != 1 {
		t.Fatalf("enqueues = %d, want 1", len(f.queue.calls))
	}
	c := f.queue.calls[0]
	if c.worker != caldav.ShareNotifyWorker || c.key != "42|principals/bob" || c.generation != 7 {
		t.Fatalf("enqueue = %+v", c)
	}
	var args map[string]any
	if err := json.Unmarshal(c.args, &args); err != nil {
		t.Fatal(err)
	}
	if args["mailbox"] != "Bob@Example.org" || args["instance_uri"] != "abc-123" {
		t.Fatalf("args = %v", args)
	}
	if strings.Join(f.policy.asked, ",") != "example.org" {
		t.Fatalf("policy asked for %v, want the canonical mailbox's lowercased domain", f.policy.asked)
	}
	if got := shareCount(t, f.metrics, "queued"); got != 1 {
		t.Fatalf("queued = %v", got)
	}
	if f.spy.msg != nil {
		t.Fatal("the request path must not send when a queue is configured")
	}
}

// TestShareMailerSkips: every screen that stops a notice leaves the share
// alone (NotifyShare has no error to return) and meters the reason.
func TestShareMailerSkips(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(f *shareFixture)
		outcome string
	}{
		{"domain not opted in", func(f *shareFixture) { f.policy.byDomain = nil }, "skipped_policy"},
		{"policy lookup fails", func(f *shareFixture) { f.policy.err = errors.New("db down") }, "policy_error"},
		{"no web url", func(f *shareFixture) { f.mailer.WebURL = "" }, "no_web_url"},
		{"no relay", func(f *shareFixture) { f.mailer.Mailer = nil }, "no_mailer"},
		{"resource principal", func(f *shareFixture) {
			f.notice.Principal = "principals/room1"
			f.resource["principals/room1"] = true
		}, "skipped_excluded"},
		{"owner", func(f *shareFixture) { f.notice.Principal = "principals/alice" }, "skipped_excluded"},
		{"no canonical mailbox", func(f *shareFixture) { f.notice.Principal = "principals/noaddr" }, "invalid_recipient"},
		{"enqueue fails", func(f *shareFixture) { f.queue.err = errors.New("queue down") }, "enqueue_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFixture()
			tc.mutate(f)
			f.mailer.NotifyShare(httptest.NewRequest("POST", "/", nil), f.notice)
			if len(f.queue.calls) != 0 {
				t.Fatalf("enqueued %d notice(s), want none", len(f.queue.calls))
			}
			if got := shareCount(t, f.metrics, tc.outcome); got != 1 {
				t.Fatalf("%s = %v, want 1", tc.outcome, got)
			}
		})
	}
}

// runJob hands the enqueued args to the durable handler, as the queue would.
func runJob(t *testing.T, f *shareFixture, attempt int) error {
	t.Helper()
	if len(f.queue.calls) == 0 {
		f.mailer.NotifyShare(httptest.NewRequest("POST", "/", nil), f.notice)
	}
	if len(f.queue.calls) == 0 {
		t.Fatal("nothing was enqueued")
	}
	c := f.queue.calls[len(f.queue.calls)-1]
	return f.mailer.DurableHandler(context.Background(), jobs.Job{
		Worker: c.worker, Args: c.args, Attempt: attempt, MaxAttempts: 6,
	})
}

// TestShareWorkerFramesAndSends pins the email: plain text only, the relay
// identity with the sharer in the display name and Reply-To, and a link to
// the sharee's own instance.
func TestShareWorkerFramesAndSends(t *testing.T) {
	f := newShareFixture()
	if err := runJob(t, f, 1); err != nil {
		t.Fatalf("handler = %v", err)
	}
	if f.spy.from != "calendar@server.example" || strings.Join(f.spy.to, ",") != "Bob@Example.org" {
		t.Fatalf("envelope from=%q to=%v", f.spy.from, f.spy.to)
	}
	msg := string(f.spy.msg)
	head, body, _ := strings.Cut(msg, "\r\n\r\n")
	for _, want := range []string{
		`From: "Alice Example via Calendar" <calendar@server.example>`,
		"Reply-To: <alice@example.org>",
		"To: <Bob@Example.org>",
		"Content-Type: text/plain; charset=UTF-8",
		"Date: Mon, 28 Sep 2026 10:00:00 +0000",
	} {
		if !strings.Contains(head, want) {
			t.Errorf("headers missing %q:\n%s", want, head)
		}
	}
	if strings.Contains(msg, "text/calendar") || strings.Contains(msg, "METHOD") {
		t.Errorf("a share notice must carry no calendar part:\n%s", msg)
	}
	for _, want := range []string{
		"Alice Example (alice@example.org) shared the calendar \u201cWork\u201d with you.",
		"You can view events.",
		"Please add the rota",
		"Open it: https://calendar.example.org/cal/calendar?cal=abc-123",
		"choose Decline.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if got := shareCount(t, f.metrics, "sent"); got != 1 {
		t.Fatalf("sent = %v", got)
	}
}

// TestShareWorkerUsesCurrentAccess: a notice queued for a read-write share
// that was downgraded before sending must not promise edit rights (and the
// converse).
func TestShareWorkerUsesCurrentAccess(t *testing.T) {
	f := newShareFixture()
	f.shares.inst.Access = caldav.AccessReadWrite
	if err := runJob(t, f, 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.spy.msg), "You can view and edit events.") {
		t.Fatalf("read-write wording missing:\n%s", f.spy.msg)
	}
}

// TestShareWorkerRevalidates: before every attempt the worker requires the
// same instance to still exist with the same mailbox and an opted-in domain.
// Anything else completes the job without sending; lookup failures retry.
func TestShareWorkerRevalidates(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(f *shareFixture)
		retry   bool
		outcome string
	}{
		{"share removed", func(f *shareFixture) { f.shares.inst = nil }, false, "skipped_obsolete"},
		{"superseded by a re-share", func(f *shareFixture) { f.shares.inst.ID, f.shares.inst.URI = 9, "def-456" }, false, "skipped_obsolete"},
		{"mailbox changed", func(f *shareFixture) {
			// PrincipalByPath copies the struct but shares its Props map.
			p, _ := f.mailer.Principals.PrincipalByPath(t.Context(), "principals/bob")
			p.Props[xmlNM{Space: "http://sabredav.org/ns", Local: "email-address"}] = "robert@example.org"
		}, false, "skipped_obsolete"},
		{"policy turned off", func(f *shareFixture) { f.policy.byDomain = nil }, false, "skipped_policy"},
		{"share lookup fails", func(f *shareFixture) { f.shares.err = errors.New("db down") }, true, ""},
		{"policy lookup fails", func(f *shareFixture) { f.policy.err = errors.New("db down") }, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFixture()
			f.mailer.NotifyShare(httptest.NewRequest("POST", "/", nil), f.notice) // enqueue first
			tc.mutate(f)
			err := runJob(t, f, 1)
			if f.spy.msg != nil {
				t.Fatalf("sent despite %s", tc.name)
			}
			if tc.retry {
				if err == nil || jobs.IsDiscard(err) {
					t.Fatalf("handler = %v, want a retryable error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("handler = %v, want the job completed without a send", err)
			}
			if got := shareCount(t, f.metrics, tc.outcome); got != 1 {
				t.Fatalf("%s = %v, want 1", tc.outcome, got)
			}
		})
	}
}

type errMailer struct{ err error }

func (m errMailer) Send(string, []string, []byte) error { return m.err }

// TestShareWorkerSendFailures: a transient SMTP failure retries until the
// attempts run out; a permanent one discards at once.
func TestShareWorkerSendFailures(t *testing.T) {
	transient := &textproto.Error{Code: 451, Msg: "try later"}
	permanent := &textproto.Error{Code: 550, Msg: "no such user"}

	f := newShareFixture()
	f.mailer.Mailer = errMailer{transient}
	if err := runJob(t, f, 1); err == nil || jobs.IsDiscard(err) {
		t.Fatalf("transient, attempt 1: %v, want retry", err)
	}
	if err := runJob(t, f, 6); !jobs.IsDiscard(err) {
		t.Fatalf("transient, last attempt: %v, want discard", err)
	}
	if got := shareCount(t, f.metrics, "retry_exhausted"); got != 1 {
		t.Fatalf("retry_exhausted = %v", got)
	}

	f = newShareFixture()
	f.mailer.Mailer = errMailer{permanent}
	if err := runJob(t, f, 1); !jobs.IsDiscard(err) {
		t.Fatalf("permanent: %v, want discard", err)
	}
	if got := shareCount(t, f.metrics, "smtp_error"); got != 1 {
		t.Fatalf("smtp_error = %v", got)
	}
}

// TestShareMailerSynchronousWithoutQueue: with no durable queue the notice is
// validated and sent inline, once.
func TestShareMailerSynchronousWithoutQueue(t *testing.T) {
	f := newShareFixture()
	f.mailer.Durable = nil
	f.mailer.NotifyShare(httptest.NewRequest("POST", "/", nil), f.notice)
	if f.spy.msg == nil {
		t.Fatal("no email sent")
	}
	if got := shareCount(t, f.metrics, "sent"); got != 1 {
		t.Fatalf("sent = %v", got)
	}
}

// TestShareEmailHeaderSafety: sharer-controlled values (names, calendar name,
// comment) cannot add headers, and the comment is capped.
func TestShareEmailHeaderSafety(t *testing.T) {
	f := newShareFixture()
	f.mailer.Durable = nil
	f.shares.inst.CalendarName = "Work\r\nBcc: victim@example.net"
	f.notice.Comment = strings.Repeat("x", 2000) + "\r\nBcc: victim@example.net"
	f.mailer.NotifyShare(httptest.NewRequest("POST", "/", nil), f.notice)

	head, body, _ := strings.Cut(string(f.spy.msg), "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatalf("injected header:\n%s", head)
		}
	}
	// The calendar name keeps its text, collapsed onto the sentence line; the
	// comment's tail past the cap is gone.
	if strings.Count(body, "victim") != 1 || !strings.Contains(body, "\u201cWork Bcc: victim@example.net\u201d") {
		t.Fatalf("name not collapsed or comment not capped:\n%s", body)
	}
	if !strings.Contains(body, strings.Repeat("x", 500)+"\u2026") {
		t.Fatalf("comment not truncated to 500 runes:\n%s", body)
	}
}
