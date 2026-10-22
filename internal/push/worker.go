package push

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"calserver/internal/caldav"
	"calserver/internal/dav"
	"calserver/internal/pgdial"
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
func (w *Worker) listen(ctx context.Context, housekeeping *time.Ticker) error {
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

// deliver dispatches key and records in retry whether to try it again.
func (w *Worker) deliver(ctx context.Context, key string, retry *retrySet) {
	retry.record(key, w.dispatch(ctx, key), time.Now())
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

	for _, s := range subs {
		if s.LastSyncToken >= token {
			continue // already delivered
		}
		// Re-check that the subscriber can still read the collection; a
		// principal that lost access must not keep being told it changed.
		pctx := dav.ContextWithPrincipal(ctx, s.PrincipalURI)
		if err := w.checker.CheckPrivileges(pctx, s.CollectionPath, "{DAV:}read"); err != nil {
			continue
		}
		status, err := w.sender.Send(ctx, s, topic, payload)
		if err != nil {
			w.log.Warn("webdav-push: send failed", "endpoint", s.PushResource, "err", err)
			continue
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
			retry = true
		default:
			w.log.Warn("webdav-push: unexpected push status", "status", status, "endpoint", s.PushResource)
		}
	}
	return retry
}

// splitKey parses a "kind:id" notification payload.
func splitKey(key string) (kind, id string, ok bool) {
	k, i, found := strings.Cut(key, ":")
	if !found || (k != "calendar" && k != "addressbook") || i == "" {
		return "", "", false
	}
	return k, i, true
}
