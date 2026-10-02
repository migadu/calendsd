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
// safe to `go worker.Run(ctx)` and forget.
func (w *Worker) Run(ctx context.Context) {
	if checker, err := caldav.RequirePrivilegeChecker(w.server); err != nil {
		w.log.Error("webdav-push: no ACL checker; dispatch disabled", "err", err)
		return
	} else {
		w.checker = checker
	}

	// Catch up on anything that changed while we were down before we start
	// listening, so a restart doesn't drop a notification.
	w.reconcile(ctx)

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
	// coalescing timer from the select loop.
	payloads := make(chan string, 256)
	errc := make(chan error, 1)
	go func() {
		for {
			n, err := conn.WaitForNotification(ctx)
			if err != nil {
				errc <- err
				return
			}
			payloads <- n.Payload
		}
	}()

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
				w.dispatch(ctx, key)
			}
			pending = map[string]bool{}
		case <-housekeeping.C:
			if err := w.store.PruneExpired(); err != nil {
				w.log.Warn("webdav-push: pruning expired subscriptions", "err", err)
			}
		}
	}
}

// reconcile sweeps every live subscription once at startup, dispatching each
// distinct collection so a change committed while the worker was down is not
// lost. dispatch itself skips subscriptions already at the current token.
func (w *Worker) reconcile(ctx context.Context) {
	subs, err := w.store.AllLive()
	if err != nil {
		w.log.Warn("webdav-push: startup reconcile failed", "err", err)
		return
	}
	seen := map[string]bool{}
	for _, s := range subs {
		key := s.Kind + ":" + s.CollectionID
		if seen[key] {
			continue
		}
		seen[key] = true
		w.dispatch(ctx, key)
	}
}

// dispatch delivers the current state of one collection ("kind:id") to every
// live subscription that hasn't already seen it.
func (w *Worker) dispatch(ctx context.Context, key string) {
	kind, id, ok := splitKey(key)
	if !ok {
		return
	}
	token, exists, err := w.store.CurrentToken(kind, id)
	if err != nil {
		w.log.Warn("webdav-push: reading collection token", "key", key, "err", err)
		return
	}
	if !exists {
		return // collection was deleted; its subscriptions will expire out
	}
	subs, err := w.store.ByCollection(kind, id)
	if err != nil {
		w.log.Warn("webdav-push: loading subscriptions", "key", key, "err", err)
		return
	}
	if len(subs) == 0 {
		return
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
		default:
			w.log.Warn("webdav-push: unexpected push status", "status", status, "endpoint", s.PushResource)
		}
	}
}

// splitKey parses a "kind:id" notification payload.
func splitKey(key string) (kind, id string, ok bool) {
	k, i, found := strings.Cut(key, ":")
	if !found || (k != "calendar" && k != "addressbook") || i == "" {
		return "", "", false
	}
	return k, i, true
}
