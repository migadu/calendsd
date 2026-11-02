package push

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/migadu/calendsd/internal/caldav"
	"github.com/migadu/calendsd/internal/dav"
	"github.com/migadu/calendsd/internal/pgdial"
)

// coalesceWindow batches a burst of notifications for one collection into a
// single push (a bulk import fires one pg_notify per object). Short enough to
// stay "instant", long enough to collapse a burst.
const coalesceWindow = 300 * time.Millisecond

// housekeepingInterval is how often expired subscriptions are pruned.
const housekeepingInterval = time.Hour

// reconnectBackoff is the pause before re-establishing the LISTEN connection
// after it drops (Postgres failover, network blip).
const reconnectBackoff = 5 * time.Second

// Delivery bounds (audit 3.3). Sends used to run one after another on the
// listen goroutine with the worker's lifetime context, so a single endpoint
// that never answered stalled every collection's notifications until restart.
const (
	// sendConcurrency caps in-flight pushes across all collections.
	sendConcurrency = 8
	// defaultSendTimeout bounds one push, including the ACL re-check.
	defaultSendTimeout = 15 * time.Second
	// slowSendWarn is the latency above which a send is logged, so a
	// degrading push service is visible before it is a stall.
	slowSendWarn = 5 * time.Second
)

// dispatchResult is what an asynchronous dispatch hands back to the listen
// loop, which owns the retry set.
type dispatchResult struct {
	key   string
	retry bool
}

// Worker delivers Web Push notifications. It holds a dedicated Postgres
// connection LISTENing on the dav_push channel (a pinned connection
// database/sql can't provide), coalesces per collection, re-checks ACL, and
// sends. It also sweeps for changes missed while it was down.
type Worker struct {
	cfg     Config
	store   *Store
	sender  Sender
	server  *dav.Server
	dsn     string
	log     *slog.Logger
	checker caldav.PrivilegeChecker

	sendTimeout time.Duration // zero → defaultSendTimeout
	initOnce    sync.Once
	sem         chan struct{}       // bounds concurrent sends across collections
	results     chan dispatchResult // outcomes for the listen loop's retry set

	// Per-collection dispatch bookkeeping: one dispatch runs per key at a
	// time (two concurrent ones would both read the same last_synctoken and
	// double-deliver); a flush that arrives while one is running marks the
	// key to be dispatched once more when it finishes.
	mu       sync.Mutex
	inflight map[string]bool
	redo     map[string]bool
}

// init lazily prepares the concurrency state so a Worker built as a struct
// literal (tests) behaves like one from NewWorker.
func (w *Worker) init() {
	w.initOnce.Do(func() {
		if w.sendTimeout <= 0 {
			w.sendTimeout = defaultSendTimeout
		}
		w.sem = make(chan struct{}, sendConcurrency)
		w.results = make(chan dispatchResult, 1024)
		w.inflight = map[string]bool{}
		w.redo = map[string]bool{}
	})
}

// NewWorker builds the dispatch worker. dsn is used to open the dedicated
// LISTEN connection; server supplies the tree and ACL checker for the
// dispatch-time read re-check.
func NewWorker(cfg Config, store *Store, sender Sender, server *dav.Server, dsn string, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{cfg: cfg, store: store, sender: sender, server: server, dsn: dsn, log: log}
}

// Run drives the worker until ctx is cancelled. It never returns an error the
// caller must act on — transient failures are logged and retried — so it is
// safe to `go worker.Run(ctx)`. It returns once its connection is closed.
func (w *Worker) Run(ctx context.Context) {
	if checker, err := caldav.RequirePrivilegeChecker(w.server); err != nil {
		w.log.Error("webdav-push: no ACL checker; dispatch disabled", "err", err)
		return
	} else {
		w.checker = checker
	}

	housekeeping := time.NewTicker(housekeepingInterval)
	defer housekeeping.Stop()

	for ctx.Err() == nil {
		if err := w.listen(ctx, housekeeping); err != nil && ctx.Err() == nil {
			w.log.Warn("webdav-push: listen connection dropped; retrying", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(reconnectBackoff):
			}
		}
	}
}

// listen holds one LISTEN connection and dispatches coalesced notifications
// until the connection fails or ctx is cancelled.
func (w *Worker) listen(ctx context.Context, housekeeping *time.Ticker) (err error) {
	// A panic anywhere in the loop drops this connection like any other
	// failure, and Run reconnects, instead of taking the process down.
	defer func() {
		if p := recover(); p != nil {
			w.log.Error("webdav-push: listen loop panic", "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("webdav-push: listen loop panic: %v", p)
		}
	}()
	cfg, err := pgdial.Config(w.dsn)
	if err != nil {
		return err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}
	w.log.Info("webdav-push: listening for changes", "channel", notifyChannel)

	// WaitForNotification blocks, so read it in a goroutine and drive the
	// coalescing timer from the select loop. The reader stops before the
	// deferred Close above, which must not race a read on the connection.
	lctx, cancel := context.WithCancel(ctx)
	readerDone := make(chan struct{})
	defer func() {
		cancel()
		<-readerDone
	}()
	payloads := make(chan string, 256)
	errc := make(chan error, 1)
	go func() {
		defer close(readerDone)
		defer func() {
			if p := recover(); p != nil {
				w.log.Error("webdav-push: notification reader panic", "panic", p, "stack", string(debug.Stack()))
				errc <- fmt.Errorf("webdav-push: notification reader panic: %v", p)
			}
		}()
		for {
			n, err := conn.WaitForNotification(lctx)
			if err != nil {
				errc <- err
				return
			}
			select {
			case payloads <- n.Payload:
			case <-lctx.Done():
				return
			}
		}
	}()

	// Now that changes wake us, catch up on any committed while we were not
	// listening (down, or reconnecting), so none is dropped.
	retry := newRetrySet()
	w.reconcile(ctx, retry)

	pending := map[string]bool{}
	flush := time.NewTimer(time.Hour)
	flush.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			return err
		case key := <-payloads:
			if len(pending) == 0 {
				flush.Reset(coalesceWindow)
			}
			pending[key] = true
		case <-flush.C:
			for key := range pending {
				w.deliver(ctx, key, retry)
			}
			pending = map[string]bool{}
			retry.arm(time.Now())
		case <-retry.timer.C:
			for _, key := range retry.takeDue(time.Now()) {
				w.deliver(ctx, key, retry)
			}
			retry.arm(time.Now())
		case r := <-w.results:
			// A dispatch finished off the loop; the retry set is this
			// goroutine's, so the outcome is recorded here.
			retry.record(r.key, r.retry, time.Now())
			retry.arm(time.Now())
		case <-housekeeping.C:
			if err := w.store.PruneExpired(); err != nil {
				w.log.Warn("webdav-push: pruning expired subscriptions", "err", err)
			}
			// Also the backstop for deliveries that failed or ran out of
			// retries.
			w.reconcile(ctx, retry)
		}
	}
}

// reconcile sweeps every live subscription, dispatching each distinct
// collection so a change committed while the worker was not listening, or a
// delivery that failed, is not lost. dispatch itself skips subscriptions
// already at the current token.
func (w *Worker) reconcile(ctx context.Context, retry *retrySet) {
	subs, err := w.store.AllLive()
	if err != nil {
		w.log.Warn("webdav-push: reconcile failed", "err", err)
		return
	}
	seen := map[string]bool{}
	for _, s := range subs {
		key := s.Kind + ":" + s.CollectionID
		if seen[key] {
			continue
		}
		seen[key] = true
		w.deliver(ctx, key, retry)
	}
	retry.arm(time.Now())
}

// deliver dispatches key off the listen loop; the outcome reaches retry
// through w.results, so a collection whose subscribers are slow never delays
// another collection's notification, and the retry set stays on this
// goroutine. A key already being dispatched is re-dispatched once that run
// finishes.
func (w *Worker) deliver(ctx context.Context, key string, _ *retrySet) {
	w.dispatchAsync(ctx, key)
}

// dispatchAsync runs dispatch for key on its own goroutine, at most one run
// per key at a time, and reports each run's outcome on w.results.
func (w *Worker) dispatchAsync(ctx context.Context, key string) {
	w.init()
	w.mu.Lock()
	if w.inflight[key] {
		w.redo[key] = true
		w.mu.Unlock()
		return
	}
	w.inflight[key] = true
	w.mu.Unlock()
	go func() {
		for {
			retry := w.dispatch(ctx, key)
			select {
			case w.results <- dispatchResult{key: key, retry: retry}:
			case <-ctx.Done():
			}
			w.mu.Lock()
			if w.redo[key] && ctx.Err() == nil {
				delete(w.redo, key)
				w.mu.Unlock()
				continue
			}
			delete(w.redo, key)
			delete(w.inflight, key)
			w.mu.Unlock()
			return
		}
	}()
}

// retryDelays are the pauses before re-dispatching a collection whose push
// service answered 429 or 5xx. Past the last one, the housekeeping reconcile
// is the next attempt.
var retryDelays = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// retrySet tracks collections awaiting a re-dispatch after a temporary push
// service failure, and a timer for the earliest one due. It belongs to the
// listen loop's goroutine.
type retrySet struct {
	attempts map[string]int
	due      map[string]time.Time
	timer    *time.Timer
}

func newRetrySet() *retrySet {
	t := time.NewTimer(time.Hour)
	t.Stop()
	return &retrySet{attempts: map[string]int{}, due: map[string]time.Time{}, timer: t}
}

// record schedules key's next retry when its dispatch asked for one, backing
// off per retryDelays, and forgets it otherwise or once the delays run out.
func (r *retrySet) record(key string, retry bool, now time.Time) {
	n := r.attempts[key]
	if !retry || n >= len(retryDelays) {
		delete(r.attempts, key)
		delete(r.due, key)
		return
	}
	r.attempts[key] = n + 1
	r.due[key] = now.Add(retryDelays[n])
}

// takeDue removes and returns the keys due by now.
func (r *retrySet) takeDue(now time.Time) []string {
	var keys []string
	for key, at := range r.due {
		if !at.After(now) {
			keys = append(keys, key)
			delete(r.due, key)
		}
	}
	return keys
}

// arm sets the timer for the earliest pending retry, if any.
func (r *retrySet) arm(now time.Time) {
	r.timer.Stop()
	var next time.Time
	for _, at := range r.due {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	if !next.IsZero() {
		r.timer.Reset(max(next.Sub(now), 0))
	}
}

// dispatch delivers the current state of one collection ("kind:id") to every
// live subscription that hasn't already seen it. It reports whether a push
// service answered with a temporary failure (429 or 5xx), so the delivery
// should be retried.
func (w *Worker) dispatch(ctx context.Context, key string) (retry bool) {
	// Runs on its own goroutine (dispatchAsync); a panic is logged and the
	// key left to the housekeeping reconcile.
	defer func() {
		if p := recover(); p != nil {
			w.log.Error("webdav-push: dispatch panic", "key", key, "panic", p, "stack", string(debug.Stack()))
			retry = false
		}
	}()
	w.init()
	kind, id, ok := splitKey(key)
	if !ok {
		return false
	}
	token, exists, err := w.store.CurrentToken(kind, id)
	if err != nil {
		w.log.Warn("webdav-push: reading collection token", "key", key, "err", err)
		return false
	}
	if !exists {
		return false // collection was deleted; its subscriptions will expire out
	}
	subs, err := w.store.ByCollection(kind, id)
	if err != nil {
		w.log.Warn("webdav-push: loading subscriptions", "key", key, "err", err)
		return false
	}
	if len(subs) == 0 {
		return false
	}
	topic := w.cfg.Topic(kind, id)
	payload := buildPushMessage(topic, syncTokenPrefix+strconv.FormatInt(token, 10))

	// Sends run concurrently, bounded by sendConcurrency across the worker
	// and by sendTimeout each; the call returns once every send for this
	// collection has settled.
	var wg sync.WaitGroup
	var mu sync.Mutex
sends:
	for _, s := range subs {
		if s.LastSyncToken >= token {
			continue // already delivered
		}
		select {
		case w.sem <- struct{}{}:
		case <-ctx.Done():
			break sends
		}
		wg.Add(1)
		go func(s Subscription) {
			defer wg.Done()
			defer func() { <-w.sem }()
			if w.send(ctx, s, topic, token, payload) {
				mu.Lock()
				retry = true
				mu.Unlock()
			}
		}(s)
	}
	wg.Wait()
	return retry
}

// send delivers one notification under its own deadline and settles the
// subscription's state from the push service's answer. It reports whether
// the push service answered with a temporary failure (429 or 5xx).
func (w *Worker) send(ctx context.Context, s Subscription, topic string, token int64, payload []byte) (retry bool) {
	// Runs on its own goroutine (dispatch); a panic counts as a failed send.
	defer func() {
		if p := recover(); p != nil {
			w.log.Error("webdav-push: send panic", "endpoint", s.PushResource, "panic", p, "stack", string(debug.Stack()))
			retry = false
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, w.sendTimeout)
	defer cancel()
	// Re-check that the subscriber can still read the collection; a
	// principal that lost access must not keep being told it changed.
	pctx := dav.ContextWithPrincipal(ctx, s.PrincipalURI)
	if err := w.checker.CheckPrivileges(pctx, s.CollectionPath, "{DAV:}read"); err != nil {
		return false
	}
	started := time.Now()
	status, err := w.sender.Send(ctx, s, topic, payload)
	if took := time.Since(started); took > slowSendWarn {
		w.log.Warn("webdav-push: slow push service", "endpoint", s.PushResource, "took", took)
	}
	if err != nil {
		w.log.Warn("webdav-push: send failed", "endpoint", s.PushResource, "err", err)
		return false
	}
	switch {
	case status == http.StatusNotFound || status == http.StatusGone:
		// The push service says this endpoint is gone; drop it.
		if err := w.store.DeleteByID(s.ID); err != nil {
			w.log.Warn("webdav-push: pruning dead subscription", "err", err)
		}
	case status >= 200 && status < 300:
		if err := w.store.SetLastToken(s.ID, token); err != nil {
			w.log.Warn("webdav-push: recording delivery", "err", err)
		}
	case status == http.StatusTooManyRequests || status >= 500:
		w.log.Warn("webdav-push: push service unavailable; will retry", "status", status, "endpoint", s.PushResource)
		return true
	default:
		w.log.Warn("webdav-push: unexpected push status", "status", status, "endpoint", s.PushResource)
	}
	return false
}

// splitKey parses a "kind:id" notification payload.
func splitKey(key string) (kind, id string, ok bool) {
	k, i, found := strings.Cut(key, ":")
	if !found || (k != "calendar" && k != "addressbook") || i == "" {
		return "", "", false
	}
	return k, i, true
}
