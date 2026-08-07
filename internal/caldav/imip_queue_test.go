package caldav_test

import (
	"errors"
	"net"
	"net/textproto"
	"sync"
	"testing"
	"time"

	"calserver/internal/caldav"
	"calserver/internal/metrics"
)

// netErr builds a transient network-level failure as the classifier sees one.
func netErr(op, msg string) error {
	return &net.OpError{Op: op, Err: errors.New(msg)}
}

// scriptedUserMailer fails SendAs with the scripted errors in order, then
// succeeds; safe for the queue worker and the test to share.
type scriptedUserMailer struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (m *scriptedUserMailer) SendAs(user, pass, from string, to []string, msg []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if len(m.errs) > 0 {
		err := m.errs[0]
		m.errs = m.errs[1:]
		return err
	}
	return nil
}

func (m *scriptedUserMailer) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// fastQueue returns a queue with millisecond backoff so retries resolve within
// the test's polling window.
func fastQueue(set *metrics.Set) *caldav.SendQueue {
	q := caldav.NewSendQueue(set, nil)
	q.Delays = []time.Duration{time.Millisecond, time.Millisecond}
	return q
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func outcomeCount(t *testing.T, set *metrics.Set, outcome string) float64 {
	t.Helper()
	mfs, err := set.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "calserver_imip_deliveries_total" {
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

// A transient failure on the synchronous attempt must queue the message (status
// pending, outcome queued) and the background retry must eventually deliver it.
func TestQueueRetriesTransientFailureToSuccess(t *testing.T) {
	set := metrics.New("test-queue-success")
	sub := &scriptedUserMailer{errs: []error{
		netErr("dial", "context deadline exceeded"),
		netErr("read", "i/o timeout"),
	}}
	plugin := sendAsUserPlugin(sub, nil)
	plugin.Metrics = set
	plugin.Queue = fastQueue(set)
	defer plugin.Queue.Stop()

	msg := sendAsUserMsg("mailto:alice@example.org", "mailto:guest@example.com")
	if !plugin.DeliverExternal(userRequest("principals/alice", "alice", "pw"), msg) {
		t.Fatalf("DeliverExternal returned false; status=%q", msg.ScheduleStatus)
	}
	if want := "1.0;Scheduling message is queued for delivery"; msg.ScheduleStatus != want {
		t.Fatalf("status = %q, want %q", msg.ScheduleStatus, want)
	}
	if got := outcomeCount(t, set, "queued"); got != 1 {
		t.Fatalf("queued outcome = %v, want 1", got)
	}

	// Attempt 1 was synchronous; the queue retries immediately (fails again),
	// then once more after the backoff and succeeds.
	waitFor(t, "delivery after retries", func() bool { return outcomeCount(t, set, "sent_as_user") == 1 })
	if got := sub.callCount(); got != 3 {
		t.Fatalf("SendAs calls = %d, want 3", got)
	}
}

// A permanent SMTP rejection during a queued retry must drop the message
// (retry_exhausted), not spin on it.
func TestQueueDropsOnPermanentError(t *testing.T) {
	set := metrics.New("test-queue-permanent")
	sub := &scriptedUserMailer{errs: []error{
		netErr("dial", "timeout"),                               // sync attempt: transient → queued
		&textproto.Error{Code: 550, Msg: "relay access denied"}, // retry: permanent → dropped
		errors.New("should never be attempted"),
	}}
	plugin := sendAsUserPlugin(sub, nil)
	plugin.Metrics = set
	plugin.Queue = fastQueue(set)
	defer plugin.Queue.Stop()

	msg := sendAsUserMsg("mailto:alice@example.org", "mailto:guest@example.com")
	plugin.DeliverExternal(userRequest("principals/alice", "alice", "pw"), msg)

	waitFor(t, "drop on permanent error", func() bool { return outcomeCount(t, set, "retry_exhausted") == 1 })
	if got := sub.callCount(); got != 2 {
		t.Fatalf("SendAs calls = %d, want 2 (no attempts after a 5xx)", got)
	}
}

// When every retry fails transiently, the backoff schedule bounds the attempts
// and the message is eventually dropped.
func TestQueueExhaustsBackoffSchedule(t *testing.T) {
	set := metrics.New("test-queue-exhausted")
	sub := &scriptedUserMailer{errs: []error{
		netErr("dial", "down"), netErr("dial", "down"), netErr("dial", "down"),
		netErr("dial", "down"), netErr("dial", "down"),
	}}
	plugin := sendAsUserPlugin(sub, nil)
	plugin.Metrics = set
	plugin.Queue = fastQueue(set) // 2 backoff slots → immediate retry + 2 delayed
	defer plugin.Queue.Stop()

	msg := sendAsUserMsg("mailto:alice@example.org", "mailto:guest@example.com")
	plugin.DeliverExternal(userRequest("principals/alice", "alice", "pw"), msg)

	waitFor(t, "retry exhaustion", func() bool { return outcomeCount(t, set, "retry_exhausted") == 1 })
	// 1 synchronous + 1 immediate + len(Delays)=2 delayed = 4 total attempts.
	if got := sub.callCount(); got != 4 {
		t.Fatalf("SendAs calls = %d, want 4", got)
	}
}

// An error the classifier cannot identify (a plain errors.New — e.g. a refused
// plaintext connection or broken config) gets one retry, not the whole backoff
// schedule.
func TestUnclassifiedErrorGetsOneRetry(t *testing.T) {
	set := metrics.New("test-queue-unclassified")
	sub := &scriptedUserMailer{errs: []error{
		errors.New("weird"), errors.New("weird"), errors.New("weird"),
	}}
	plugin := sendAsUserPlugin(sub, nil)
	plugin.Metrics = set
	plugin.Queue = fastQueue(set)
	defer plugin.Queue.Stop()

	msg := sendAsUserMsg("mailto:alice@example.org", "mailto:guest@example.com")
	plugin.DeliverExternal(userRequest("principals/alice", "alice", "pw"), msg)

	waitFor(t, "drop of unclassified error", func() bool { return outcomeCount(t, set, "retry_exhausted") == 1 })
	if got := sub.callCount(); got != 2 {
		t.Fatalf("SendAs calls = %d, want 2 (sync + one retry)", got)
	}
}

// The zero value must be usable, Stop must be idempotent (including on a queue
// that never started), and Enqueue after Stop must report the rejection
// instead of silently dropping the message.
func TestQueueZeroValueAndStopSemantics(t *testing.T) {
	var never caldav.SendQueue
	never.Stop() // never started: must not panic
	never.Stop()

	var q caldav.SendQueue
	done := make(chan struct{})
	if !q.Enqueue(func() error { close(done); return nil }, "sent", "REQUEST", "a", "b") {
		t.Fatal("enqueue on a zero-value queue rejected")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("zero-value queue never ran the attempt")
	}
	q.Stop()
	q.Stop() // idempotent
	if q.Enqueue(func() error { return nil }, "sent", "REQUEST", "a", "b") {
		t.Fatal("enqueue after Stop must report false")
	}
}

// A full queue rejects at enqueue (counted and logged) rather than growing
// without bound.
func TestQueueFullRejectsAtEnqueue(t *testing.T) {
	set := metrics.New("test-queue-full")
	q := &caldav.SendQueue{Workers: 1, MaxPending: 1, Metrics: set}
	defer q.Stop()

	started := make(chan struct{})
	block := make(chan struct{})
	defer close(block)
	if !q.Enqueue(func() error { close(started); <-block; return nil }, "sent", "REQUEST", "a", "b") {
		t.Fatal("first enqueue rejected")
	}
	<-started // the single worker now sits in the attempt; the pending slot is free
	if !q.Enqueue(func() error { return nil }, "sent", "REQUEST", "a", "b") {
		t.Fatal("second enqueue should fill the single pending slot")
	}
	if q.Enqueue(func() error { return nil }, "sent", "REQUEST", "a", "b") {
		t.Fatal("third enqueue should be rejected: queue full")
	}
	if got := outcomeCount(t, set, "queue_full"); got != 1 {
		t.Fatalf("queue_full outcome = %v, want 1", got)
	}
}

// An authentication failure is permanent: it must keep its terminal 5.3 status
// and never reach the queue — retrying the same wrong credential is useless.
func TestAuthFailureIsNotQueued(t *testing.T) {
	set := metrics.New("test-queue-auth")
	sub := &scriptedUserMailer{errs: []error{&textproto.Error{Code: 535, Msg: "bad credentials"}}}
	plugin := sendAsUserPlugin(sub, nil)
	plugin.Metrics = set
	plugin.Queue = fastQueue(set)
	defer plugin.Queue.Stop()

	msg := sendAsUserMsg("mailto:alice@example.org", "mailto:guest@example.com")
	plugin.DeliverExternal(userRequest("principals/alice", "alice", "pw"), msg)

	if want := "5.3;Submission authentication failed"; msg.ScheduleStatus != want {
		t.Fatalf("status = %q, want %q", msg.ScheduleStatus, want)
	}
	if got := outcomeCount(t, set, "queued"); got != 0 {
		t.Fatalf("queued outcome = %v, want 0", got)
	}
	if got := sub.callCount(); got != 1 {
		t.Fatalf("SendAs calls = %d, want 1", got)
	}
}
