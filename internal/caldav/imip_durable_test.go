package caldav_test

// Unit tests for the durable "imip_send" worker (IMipPlugin.DurableHandler): the
// send-identity split (relay vs metered send-as-user), SMTP result handling
// (D13), and the SCHEDULE-STATUS write-back. These drive the handler directly
// with a constructed jobs.Job — no database — so they stay fast and exact.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"net/textproto"
	"sync"
	"testing"

	"calendsd/internal/caldav"
	"calendsd/internal/jobs"
	"calendsd/internal/metrics"
)

// fakeDurable records Enqueue calls without a database, to assert the enqueue-
// side routing (which identity, credential or not) the delivery paths choose.
type fakeDurable struct {
	mu       sync.Mutex
	enq      []fakeEnq
	encrypts bool
}

type fakeEnq struct {
	worker  string
	args    json.RawMessage
	hasCred bool
}

func (f *fakeDurable) Enqueue(_ context.Context, worker string, args any, opts ...jobs.Opt) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// The delivery paths pass exactly one option — WithCredential — for a per-user
	// job and none for a relay job, so the option count is a faithful cred proxy.
	f.enq = append(f.enq, fakeEnq{worker: worker, args: raw, hasCred: len(opts) > 0})
	return nil
}

func (f *fakeDurable) EncryptsCredentials() bool { return f.encrypts }

func (f *fakeDurable) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.enq) }

func (f *fakeDurable) first(t *testing.T) fakeEnq {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.enq) == 0 {
		t.Fatal("no durable job enqueued")
	}
	return f.enq[0]
}

func (e fakeEnq) argMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.args, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// asJob reconstructs the jobs.Job a real worker would claim, so a test can drive
// DurableHandler directly. cred is the decrypted credential the queue would have
// supplied (the test knows the plaintext), or nil for a relay job.
func (e fakeEnq) asJob(cred []byte) jobs.Job {
	return jobs.Job{Worker: e.worker, Args: e.args, Credential: cred, Attempt: 1, MaxAttempts: 6}
}

// writeBackRec records patchScheduleStatus write-backs.
type writeBackRec struct {
	called                     bool
	path, method, rcpt, status string
}

func (w *writeBackRec) fn() func(ctx context.Context, path, method, recipient, status string) {
	return func(_ context.Context, path, method, recipient, status string) {
		w.called = true
		w.path, w.method, w.rcpt, w.status = path, method, recipient, status
	}
}

func durableJob(t *testing.T, args map[string]any, cred []byte, attempt, max int) jobs.Job {
	t.Helper()
	if args["message_b64"] == nil {
		args["message_b64"] = base64.StdEncoding.EncodeToString([]byte("BODY"))
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return jobs.Job{Worker: "imip_send", Args: raw, Credential: cred, Attempt: attempt, MaxAttempts: max}
}

// TestDurableHandlerRelaySend: a credential-free job sends through the shared
// relay (Mailer) and writes statusSent back.
func TestDurableHandlerRelaySend(t *testing.T) {
	mailer := &spyMailer{}
	wb := &writeBackRec{}
	p := &caldav.IMipPlugin{SenderEmail: "server@example.org", Mailer: mailer, Metrics: metrics.New("t"), WriteBack: wb.fn()}

	job := durableJob(t, map[string]any{
		"envelope_from": "server@example.org", "recipient": "a@x.test",
		"method": "REQUEST", "uid": "u1", "outcome": "sent", "object_path": "cal/e.ics",
	}, nil, 1, 6)

	if err := p.DurableHandler(context.Background(), job); err != nil {
		t.Fatalf("handler returned %v, want nil (delivered)", err)
	}
	if mailer.from != "server@example.org" || len(mailer.to) != 1 || mailer.to[0] != "a@x.test" {
		t.Errorf("relay send from=%q to=%v, want server@ / [a@x.test]", mailer.from, mailer.to)
	}
	if string(mailer.msg) != "BODY" {
		t.Errorf("relay body = %q, want BODY", mailer.msg)
	}
	if !wb.called || wb.status != "1.1;Scheduling message is sent via iMip" || wb.rcpt != "mailto:a@x.test" {
		t.Errorf("write-back = %+v, want statusSent to mailto:a@x.test", wb)
	}
}

// TestDurableHandlerPerUserSend: a job carrying a credential submits as the user
// (metered SendAs), not through the relay.
func TestDurableHandlerPerUserSend(t *testing.T) {
	um := &spyUserMailer{}
	wb := &writeBackRec{}
	p := &caldav.IMipPlugin{SenderEmail: "server@example.org", Submission: um, Metrics: metrics.New("t"), WriteBack: wb.fn()}

	job := durableJob(t, map[string]any{
		"envelope_from": "alice@example.org", "username": "alice", "recipient": "a@x.test",
		"method": "REQUEST", "uid": "u2", "outcome": "sent_as_user", "object_path": "cal/e.ics",
	}, []byte("s3cret"), 1, 6)

	if err := p.DurableHandler(context.Background(), job); err != nil {
		t.Fatalf("handler returned %v, want nil", err)
	}
	if !um.called || um.user != "alice" || um.pass != "s3cret" || um.from != "alice@example.org" {
		t.Errorf("SendAs got user=%q pass=%q from=%q, want alice/s3cret/alice@example.org", um.user, um.pass, um.from)
	}
	if !wb.called || wb.status != "1.1;Scheduling message is sent via iMip" {
		t.Errorf("write-back = %+v, want statusSent", wb)
	}
}

// TestDurableHandlerTransientReschedules: a network error is transient — the
// handler returns a non-terminal error (the queue reschedules) and does NOT
// write a terminal status back (the object stays pending).
func TestDurableHandlerTransientReschedules(t *testing.T) {
	um := &spyUserMailer{err: &net.OpError{Op: "dial", Err: errors.New("timeout")}}
	wb := &writeBackRec{}
	p := &caldav.IMipPlugin{SenderEmail: "server@example.org", Submission: um, Metrics: metrics.New("t"), WriteBack: wb.fn()}

	job := durableJob(t, map[string]any{
		"envelope_from": "alice@example.org", "username": "alice", "recipient": "a@x.test",
		"method": "REQUEST", "uid": "u3", "outcome": "sent_as_user", "object_path": "cal/e.ics",
	}, []byte("pw"), 1, 6)

	err := p.DurableHandler(context.Background(), job)
	if err == nil || jobs.IsDiscard(err) {
		t.Fatalf("handler returned %v, want a retryable (non-Discard) error", err)
	}
	if wb.called {
		t.Errorf("write-back fired on a transient failure (%+v); object should stay pending", wb)
	}
}

// TestDurableHandlerPermanentDiscards: a 5xx rejection is permanent — the handler
// discards (no more retries) and writes the classified terminal status back.
func TestDurableHandlerPermanentDiscards(t *testing.T) {
	um := &spyUserMailer{err: &textproto.Error{Code: 550, Msg: "no such user"}}
	wb := &writeBackRec{}
	p := &caldav.IMipPlugin{SenderEmail: "server@example.org", Submission: um, Metrics: metrics.New("t"), WriteBack: wb.fn()}

	job := durableJob(t, map[string]any{
		"envelope_from": "alice@example.org", "username": "alice", "recipient": "a@x.test",
		"method": "REQUEST", "uid": "u4", "outcome": "sent_as_user", "object_path": "cal/e.ics",
	}, []byte("pw"), 1, 6)

	err := p.DurableHandler(context.Background(), job)
	if !jobs.IsDiscard(err) {
		t.Fatalf("handler returned %v, want a Discard (permanent)", err)
	}
	if !wb.called || wb.status != "5.1;Could not deliver invite by email" {
		t.Errorf("write-back = %+v, want statusFailed (5.1)", wb)
	}
}

// TestDurableHandlerExhaustedDiscards: a transient failure on the last attempt
// is terminal — the handler discards and writes a status back rather than
// looping forever.
func TestDurableHandlerExhaustedDiscards(t *testing.T) {
	um := &spyUserMailer{err: &net.OpError{Op: "dial", Err: errors.New("timeout")}}
	wb := &writeBackRec{}
	p := &caldav.IMipPlugin{SenderEmail: "server@example.org", Submission: um, Metrics: metrics.New("t"), WriteBack: wb.fn()}

	job := durableJob(t, map[string]any{
		"envelope_from": "alice@example.org", "username": "alice", "recipient": "a@x.test",
		"method": "REQUEST", "uid": "u5", "outcome": "sent_as_user", "object_path": "cal/e.ics",
	}, []byte("pw"), 6, 6) // attempt == max

	err := p.DurableHandler(context.Background(), job)
	if !jobs.IsDiscard(err) {
		t.Fatalf("handler returned %v, want Discard on exhaustion", err)
	}
	if !wb.called {
		t.Error("write-back should fire on exhaustion so the object doesn't stay pending forever")
	}
}

// TestDeliverAsUserEnqueuesDurable: a per-user send goes straight onto the
// durable queue WITH a credential, framed for send-as-user, off the request
// path — no synchronous SendAs on the request goroutine (D12, single tier).
func TestDeliverAsUserEnqueuesDurable(t *testing.T) {
	set := metrics.New("t-peruser-durable")
	sub := &spyUserMailer{}
	p := sendAsUserPlugin(sub, nil)
	p.Metrics = set
	fake := &fakeDurable{encrypts: true}
	p.Durable = fake

	msg := sendAsUserMsg("mailto:alice@example.org", "mailto:guest@example.com")
	r := userRequest("principals/alice", "alice", "pw")
	if !p.DeliverExternalAsync(r, msg, "cal/e.ics") {
		t.Fatal("DeliverExternalAsync returned false")
	}
	// Enqueue is synchronous on the request goroutine: exactly one job, no SendAs.
	if fake.count() != 1 {
		t.Fatalf("durable jobs = %d, want 1", fake.count())
	}
	if sub.called {
		t.Error("SendAs ran on the request path; the worker should send")
	}
	j := fake.first(t)
	if !j.hasCred {
		t.Error("per-user job must carry a credential")
	}
	m := j.argMap(t)
	if m["identity"] != "user" {
		t.Errorf("identity = %v, want user (audit 3.5)", m["identity"])
	}
	if m["username"] != "alice" {
		t.Errorf("username = %v, want alice", m["username"])
	}
	if m["envelope_from"] != "alice@example.org" {
		t.Errorf("envelope_from = %v, want alice@example.org", m["envelope_from"])
	}
	if m["object_path"] != "cal/e.ics" {
		t.Errorf("object_path = %v, want cal/e.ics", m["object_path"])
	}
}

// TestDeliverViaRelayEnqueuesDurable: a relay-identity send goes straight onto
// the durable queue, credential-free, without an inline Mailer call.
func TestDeliverViaRelayEnqueuesDurable(t *testing.T) {
	set := metrics.New("t-relay-durable")
	relay := &spyMailer{}
	p := &caldav.IMipPlugin{SenderEmail: "server@example.org", Mailer: relay, Metrics: set}
	fake := &fakeDurable{}
	p.Durable = fake

	// No Submission configured → the relay path.
	msg := sendAsUserMsg("mailto:org@example.org", "mailto:guest@example.com")
	if !p.DeliverExternalAsync(httptest.NewRequest("PUT", "/", nil), msg, "cal/e.ics") {
		t.Fatal("DeliverExternalAsync returned false")
	}
	if fake.count() != 1 {
		t.Fatalf("durable jobs = %d, want 1", fake.count())
	}
	j := fake.first(t)
	if j.hasCred {
		t.Error("relay job must not carry a credential")
	}
	if relay.msg != nil {
		t.Error("relay Mailer was called inline; the durable worker should send")
	}
	m := j.argMap(t)
	if m["envelope_from"] != "server@example.org" {
		t.Errorf("envelope_from = %v, want server@example.org", m["envelope_from"])
	}
	if m["identity"] != "relay" {
		t.Errorf("identity = %v, want relay (audit 3.5)", m["identity"])
	}
}
