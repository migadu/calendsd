package caldav

import (
	"errors"
	"log/slog"
	"net"
	"net/textproto"
	"sync"
	"time"

	"calserver/internal/metrics"
)

// SendQueue retries iMIP sends that failed with a transient error, so a
// stalled or briefly unavailable SMTP hop does not lose scheduling mail. The
// first attempt stays synchronous inside the triggering CalDAV request (its
// outcome feeds SCHEDULE-STATUS); only a retryable failure lands here, with
// the stored status left at 1.0 (pending) — the final outcome is visible in
// the log and the delivery metric, not in the stored object.
//
// The queue is in-memory on purpose: a queued attempt closure carries the
// user's submission credential, which must never be persisted or logged, so
// it lives only in RAM for as long as its message is queued. A restart drops
// whatever is still pending; each pending message was already logged when its
// retry was scheduled, so the trace survives in the journal.
//
// The zero value is usable; the worker pool starts on first Enqueue.
type SendQueue struct {
	// Delays is the backoff schedule after the immediate first retry fails:
	// queue attempt n+1 runs Delays[n] after failure n. When the schedule is
	// exhausted the message is dropped (outcome retry_exhausted). Nil uses
	// defaultRetryDelays; tests shorten it.
	Delays []time.Duration
	// Workers bounds how many attempts run concurrently. Attempts are
	// synchronous SMTP sends that can block for the mailer's full timeout, and
	// the condition that fills the queue (a stalled hop) is exactly the one
	// that makes every attempt slow — a single worker would serialize a burst
	// into attempts × timeout. Zero uses defaultQueueWorkers.
	Workers int
	// MaxPending caps the messages waiting for an attempt; Enqueue rejects
	// beyond it (the caller then records its terminal failure) so a long
	// outage cannot pin unbounded credentials and message bodies in RAM.
	// Zero uses defaultMaxPending.
	MaxPending int
	// Metrics records terminal outcomes (the success outcome or
	// retry_exhausted) and queue_full rejections; enqueueing itself is counted
	// by the caller. Nil records nothing.
	Metrics *metrics.Set
	// Logger records every queue attempt: a delivery at Info, a scheduled
	// retry or a dropped message at Warn. Nil uses slog.Default().
	Logger *slog.Logger

	mu      sync.Mutex
	items   []*queuedSend
	wake    chan struct{}
	stop    chan struct{}
	started bool
	stopped bool
}

// defaultRetryDelays spans roughly an hour: long enough to ride out
// greylisting, a resolver hiccup, or a short outage, short enough that a
// cancellation does not arrive absurdly late.
var defaultRetryDelays = []time.Duration{
	30 * time.Second, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute,
}

const (
	defaultQueueWorkers = 4
	defaultMaxPending   = 256
)

type queuedSend struct {
	// attempt re-runs the submission. It captures everything the send needs —
	// including the user's credential — so the queue itself never sees or
	// stores the secret separately. Once popped from items the queuedSend is
	// owned by exactly one worker, which is what makes the unlocked
	// tries/notBefore updates in attemptNow safe.
	attempt func() error
	// outcome is the metric/log label recorded when the send succeeds
	// (sent_as_user, sent, or resource_relay).
	outcome                   string
	method, sender, recipient string
	tries                     int
	notBefore                 time.Time
}

// NewSendQueue returns a queue whose worker pool starts on first enqueue.
func NewSendQueue(m *metrics.Set, logger *slog.Logger) *SendQueue {
	return &SendQueue{Metrics: m, Logger: logger}
}

func (q *SendQueue) logger() *slog.Logger {
	if q.Logger != nil {
		return q.Logger
	}
	return slog.Default()
}

func (q *SendQueue) delays() []time.Duration {
	if q.Delays != nil {
		return q.Delays
	}
	return defaultRetryDelays
}

func (q *SendQueue) workers() int {
	if q.Workers > 0 {
		return q.Workers
	}
	return defaultQueueWorkers
}

func (q *SendQueue) maxPending() int {
	if q.MaxPending > 0 {
		return q.MaxPending
	}
	return defaultMaxPending
}

// Enqueue schedules attempt for an immediate retry (the synchronous attempt
// has just failed, but that very failure often clears the path — a negative
// DNS cache warmed by the timed-out connection, a freed concurrency slot — so
// trying again right away frequently succeeds). method/sender/recipient label
// the log lines; outcome is the success label. It reports false when the
// message was NOT accepted (queue full or stopped) so the caller can record
// the terminal failure instead. Starts the workers on first use.
func (q *SendQueue) Enqueue(attempt func() error, outcome string, method, sender, recipient string) bool {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return false
	}
	if len(q.items) >= q.maxPending() {
		depth := len(q.items)
		q.mu.Unlock()
		q.Metrics.IMipDelivery("queue_full")
		q.logger().Warn("imip retry queue full; message not queued",
			"method", method, "sender", sender, "recipient", recipient, "pending", depth)
		return false
	}
	if !q.started {
		q.started = true
		// One wake slot per worker: a dropped signal then implies a full
		// buffer, i.e. enough pending wakeups to get every worker scanning.
		q.wake = make(chan struct{}, q.workers())
		q.stop = make(chan struct{})
		for range q.workers() {
			go q.run()
		}
	}
	q.items = append(q.items, &queuedSend{
		attempt: attempt, outcome: outcome,
		method: method, sender: sender, recipient: recipient,
		notBefore: time.Now(),
	})
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// Stop terminates the workers after any in-flight attempts; pending messages
// are abandoned (each was logged when its retry was scheduled) and later
// Enqueue calls report false. Idempotent. Only tests call this — in the
// server the queue lives for the life of the process.
func (q *SendQueue) Stop() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return
	}
	q.stopped = true
	if q.started {
		close(q.stop)
	}
}

func (q *SendQueue) run() {
	for {
		item, wait := q.next()
		if item == nil {
			select {
			case <-q.wake:
			case <-time.After(wait):
			case <-q.stop:
				return
			}
			continue
		}
		select {
		case <-q.stop:
			return
		default:
		}
		q.attemptNow(item)
	}
}

// next pops a due item, or reports how long to sleep until one is due.
func (q *SendQueue) next() (*queuedSend, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil, time.Hour
	}
	best := 0
	for i, it := range q.items {
		if it.notBefore.Before(q.items[best].notBefore) {
			best = i
		}
	}
	it := q.items[best]
	if d := time.Until(it.notBefore); d > 0 {
		return nil, d
	}
	q.items = append(q.items[:best], q.items[best+1:]...)
	return it, 0
}

func (q *SendQueue) attemptNow(item *queuedSend) {
	err := item.attempt()
	attrs := []any{
		"outcome", item.outcome, "method", item.method,
		"sender", item.sender, "recipient", item.recipient, "tries", item.tries + 1,
	}
	if err == nil {
		q.Metrics.IMipDelivery(item.outcome)
		q.logger().Info("imip delivered (retry)", attrs...)
		return
	}
	attrs = append(attrs, "err", err)
	// Attempts so far: the synchronous one, prior queue attempts, and this one.
	if retryableSend(err, item.tries+2) && item.tries < len(q.delays()) {
		delay := q.delays()[item.tries]
		item.tries++
		item.notBefore = time.Now().Add(delay)
		q.logger().Warn("imip retry scheduled", append(attrs, "next_attempt_in", delay)...)
		q.mu.Lock()
		q.items = append(q.items, item)
		q.mu.Unlock()
		return
	}
	q.Metrics.IMipDelivery("retry_exhausted")
	q.logger().Warn("imip giving up", attrs...)
}

// retryableSend reports whether a send that failed with err is worth retrying;
// attempts counts the failures so far, including the one that produced err.
// A 4xx SMTP rejection (greylisting, sending limits, transient policy) and any
// network-level error (dial, timeout, reset) are transient no matter how often
// they recur within the backoff schedule. A 5xx SMTP code is a permanent
// rejection — and an auth failure in particular never gets better by repeating
// the same credential. An error we cannot classify (a refused plaintext
// connection, a malformed address, a broken config) gets one retry in case it
// was a blip, not the whole schedule.
func retryableSend(err error, attempts int) bool {
	var tperr *textproto.Error
	if errors.As(err, &tperr) {
		return tperr.Code >= 400 && tperr.Code < 500
	}
	var nerr net.Error
	if errors.As(err, &nerr) {
		return true
	}
	return attempts <= 1
}
