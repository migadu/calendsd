// Package jobs is a small durable, Postgres-backed work queue (JOB-QUEUE-PLAN.md).
// It exists so background work — today only iMIP scheduling mail — survives a
// process restart, which the in-memory caldav.SendQueue could not.
//
// Shape: one general `jobs` table (Oban-shaped, so a future Elixir/Oban move is
// a column mapping), a claim-then-release worker loop, and periodic reclaim /
// credential-TTL / prune sweeps. A handler's return value decides the outcome:
// nil completes, Discard(err) fails terminally now, any other error reschedules
// with backoff until MaxAttempts.
//
// STANDING INVARIANT (D8): a job's Args is inspectable JSONB and MUST NOT carry
// a secret. The one secret the queue handles — a per-user submission password
// for metered send-as-user retries (D12) — travels only through WithCredential,
// is AEAD-encrypted into dedicated columns before it touches disk, is decrypted
// into Job.Credential only for the handler, and is zeroed right after. The next
// worker author must keep secrets out of Args.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"calserver/internal/pgdial"
)

// notifyChannel is the LISTEN/NOTIFY channel an Enqueue pings so an idle worker
// (possibly on another node) wakes at once instead of waiting for the tick.
const notifyChannel = "jobs"

// Defaults chosen to match the in-memory SendQueue's feel; all overridable.
var defaultBackoff = []time.Duration{
	30 * time.Second, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute,
}

const (
	defaultWorkers       = 4
	defaultReclaimAfter  = 5 * time.Minute  // must exceed the mailer timeout (D9)
	defaultCredentialTTL = time.Hour        // grace after give-up/abandonment (D12)
	defaultRetention     = 24 * time.Hour   // keep settled rows this long for ops
	defaultSweep         = time.Minute      // reclaim/purge/prune + future-due catch-up
	defaultIdle          = 30 * time.Second // max nap when no job is due and no wake arrives
)

// Job is one claimed unit of work handed to a Handler.
type Job struct {
	ID          int64
	Queue       string
	Worker      string
	Args        json.RawMessage
	Attempt     int // this attempt's number (1 on the first try)
	MaxAttempts int
	// Credential is the decrypted submission password for a per-user send, or
	// nil for a relay-identity job. It is valid only for the duration of the
	// handler call; the queue zeroes it as soon as the handler returns.
	Credential []byte
}

// Handler runs a job. Return nil to complete it, Discard(err) to fail it
// terminally now (a permanent error — e.g. a 5xx, D11), or any other error to
// reschedule it with backoff until MaxAttempts is reached. The handler owns any
// terminal side effect (the iMIP handler writes SCHEDULE-STATUS back).
type Handler func(ctx context.Context, j Job) error

// discardErr marks an error as terminal so the queue discards instead of
// retrying. Use Discard to construct it.
type discardErr struct{ err error }

func (d *discardErr) Error() string { return d.err.Error() }
func (d *discardErr) Unwrap() error { return d.err }

// Discard wraps err to tell the queue this failure is permanent: mark the job
// discarded now rather than retrying to exhaustion (D11).
func Discard(err error) error { return &discardErr{err: err} }

// IsDiscard reports whether err was produced by Discard — a terminal failure the
// queue will not retry. Handlers and their tests use it to tell a permanent
// rejection from a transient one.
func IsDiscard(err error) bool {
	var d *discardErr
	return errors.As(err, &d)
}

// Config tunes a Queue. The zero value is usable (all fields default).
type Config struct {
	// DSN opens the dedicated LISTEN connection (pgx); empty disables
	// LISTEN/NOTIFY and the queue relies on the sweep tick alone (fine for
	// single-process tests).
	DSN string
	// Keyring encrypts/decrypts per-user credentials (D12). Nil means no key is
	// configured: Enqueue with WithCredential is refused and the per-user
	// durable path is off (D5b applies upstream).
	Keyring *Keyring
	// Queues the worker loops drain; empty means just "default".
	Queues       []string
	Workers      int             // claim loops per queue
	Backoff      []time.Duration // delay after failure n (n from 0); last entry repeats
	ReclaimAfter time.Duration   // 'executing' older than this is reclaimed (> mailer timeout)
	// CredentialTTL is the grace period a credential lingers after the job stops
	// needing it — after it gave up (discarded, for a manual resend) or went
	// overdue/abandoned. The active retry window needs no coverage here: a
	// retrying job keeps its credential automatically (its lifetime tracks the
	// backoff-lengthening schedule — D12), so this can be short.
	CredentialTTL time.Duration
	Retention     time.Duration // settled rows are pruned once older than this
	SweepInterval time.Duration // reclaim/purge/prune + future-due catch-up cadence
	Logger        *slog.Logger
}

// Queue is a durable work queue over one Postgres database. Register handlers,
// Start it with the server lifecycle, then Enqueue from request paths.
type Queue struct {
	store    *store
	cfg      Config
	registry map[string]Handler
	log      *slog.Logger
	wake     chan struct{}
}

// New builds a queue over db. Register handlers before Start.
func New(db *sql.DB, cfg Config) *Queue {
	if cfg.Workers <= 0 {
		cfg.Workers = defaultWorkers
	}
	if cfg.Backoff == nil {
		cfg.Backoff = defaultBackoff
	}
	if cfg.ReclaimAfter <= 0 {
		cfg.ReclaimAfter = defaultReclaimAfter
	}
	if cfg.CredentialTTL <= 0 {
		cfg.CredentialTTL = defaultCredentialTTL
	}
	if cfg.Retention <= 0 {
		cfg.Retention = defaultRetention
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = defaultSweep
	}
	if len(cfg.Queues) == 0 {
		cfg.Queues = []string{"default"}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Queue{
		store:    &store{db: db},
		cfg:      cfg,
		registry: map[string]Handler{},
		log:      log,
		wake:     make(chan struct{}, cfg.Workers*len(cfg.Queues)),
	}
}

// Register wires a handler for a worker name. Call before Start; not safe to
// call concurrently with a running queue.
func (q *Queue) Register(worker string, h Handler) { q.registry[worker] = h }

// EncryptsCredentials reports whether a keyring is configured, i.e. whether
// Enqueue can accept a WithCredential job (the per-user durable path — D12).
// Callers fall back to a synchronous send / D5b when it returns false.
func (q *Queue) EncryptsCredentials() bool { return q.cfg.Keyring != nil }

// Opt configures one Enqueue.
type Opt func(*enqueueOpts)

type enqueueOpts struct {
	queue       string
	maxAttempts int
	scheduledAt time.Time
	credential  []byte
}

// WithCredential attaches a submission password to a per-user job; the queue
// encrypts it before insert (D12). Requires a Keyring — Enqueue errors without
// one, so callers must fall back to a synchronous send / D5b when no key is set.
func WithCredential(pw []byte) Opt { return func(o *enqueueOpts) { o.credential = pw } }

// WithQueue routes the job to a named queue (default "default").
func WithQueue(name string) Opt { return func(o *enqueueOpts) { o.queue = name } }

// WithMaxAttempts overrides the retry ceiling for this job.
func WithMaxAttempts(n int) Opt { return func(o *enqueueOpts) { o.maxAttempts = n } }

// WithDelay makes the job first eligible after d (backoff/scheduling).
func WithDelay(d time.Duration) Opt {
	return func(o *enqueueOpts) { o.scheduledAt = time.Now().Add(d) }
}

// Enqueue persists a job for worker with the given args (marshalled to JSONB;
// it MUST NOT contain a secret — D8). A per-user job passes WithCredential; the
// password is AEAD-encrypted into dedicated columns, never into args. Enqueue
// coalesces per (uid, recipient, method): a newer revision supersedes a pending
// job, an older/equal re-issue is a no-op (D10).
func (q *Queue) Enqueue(ctx context.Context, worker string, args any, opts ...Opt) error {
	o := enqueueOpts{queue: "default", maxAttempts: 6}
	for _, fn := range opts {
		fn(&o)
	}
	if o.scheduledAt.IsZero() {
		o.scheduledAt = time.Now()
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("jobs: marshal args: %w", err)
	}
	var cred *encCredential
	if o.credential != nil {
		ct, nonce, keyID, err := q.cfg.Keyring.Encrypt(o.credential)
		if err != nil {
			return fmt.Errorf("jobs: encrypt credential: %w", err)
		}
		cred = &encCredential{ciphertext: ct, nonce: nonce, keyID: keyID}
	}
	if err := q.store.enqueue(ctx, o.queue, worker, raw, o.maxAttempts, o.scheduledAt, cred); err != nil {
		return fmt.Errorf("jobs: enqueue: %w", err)
	}
	q.signalWake()
	// Best-effort cross-node wake; a failure just means peers rely on their tick.
	if _, err := q.store.db.ExecContext(ctx, `SELECT pg_notify($1, '')`, notifyChannel); err != nil {
		q.log.Debug("jobs: pg_notify failed", "err", err)
	}
	return nil
}

// EnqueueLatest persists a relay-identity job that coalesces latest-wins per
// (worker, key): while a job for the same key is still pending ('available'), a
// higher generation replaces its args and resets its retry state, and an equal
// or lower generation is a no-op. The queue stamps args.coalesce_key and
// args.generation, so args must marshal to a JSON object. generation must be
// monotonic per key (share_notify uses the sharee's instance id).
//
// Coalescing only covers pending jobs: one already executing may still send,
// and a retry or reclaim of it yields to a newer pending generation (see
// store.reschedule and store.reclaimOrphans) rather than violating the index.
// WithCredential is refused — these jobs never carry a secret.
func (q *Queue) EnqueueLatest(ctx context.Context, worker, key string, generation int64, args any, opts ...Opt) error {
	o := enqueueOpts{queue: "default", maxAttempts: 6}
	for _, fn := range opts {
		fn(&o)
	}
	if o.credential != nil {
		return errors.New("jobs: EnqueueLatest does not take a credential")
	}
	if key == "" {
		return errors.New("jobs: EnqueueLatest needs a coalesce key")
	}
	if o.scheduledAt.IsZero() {
		o.scheduledAt = time.Now()
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("jobs: marshal args: %w", err)
	}
	if len(raw) == 0 || raw[0] != '{' {
		return errors.New("jobs: EnqueueLatest args must be a JSON object")
	}
	if err := q.store.enqueueLatest(ctx, o.queue, worker, key, generation, raw, o.maxAttempts, o.scheduledAt); err != nil {
		return fmt.Errorf("jobs: enqueue: %w", err)
	}
	q.signalWake()
	if _, err := q.store.db.ExecContext(ctx, `SELECT pg_notify($1, '')`, notifyChannel); err != nil {
		q.log.Debug("jobs: pg_notify failed", "err", err)
	}
	return nil
}

// Start runs the claim loops, the LISTEN wake, and the sweep tick until ctx is
// cancelled. Safe to `go q.Start(ctx)`; it never returns an error the caller
// must act on. Register all handlers first.
func (q *Queue) Start(ctx context.Context) {
	for _, queue := range q.cfg.Queues {
		for i := 0; i < q.cfg.Workers; i++ {
			go q.workerLoop(ctx, queue)
		}
	}
	go q.sweepLoop(ctx)
	if q.cfg.DSN != "" {
		go q.listenLoop(ctx)
	}
}

// workerLoop claims and processes jobs for one queue until ctx is cancelled,
// napping on the wake channel (or a bounded idle timeout) when nothing is due.
func (q *Queue) workerLoop(ctx context.Context, queue string) {
	for {
		if ctx.Err() != nil {
			return
		}
		c, err := q.store.claim(ctx, queue)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			q.log.Warn("jobs: claim failed", "queue", queue, "err", err)
			if q.sleep(ctx, time.Second) {
				return
			}
			continue
		}
		if c == nil {
			if q.sleep(ctx, defaultIdle) {
				return
			}
			continue
		}
		q.process(ctx, c)
	}
}

// process decrypts any credential, runs the handler, zeroes the credential, and
// settles the outcome. A panic in a handler is recovered and treated as a
// transient error so one bad job can't take down a worker.
func (q *Queue) process(ctx context.Context, c *claimed) {
	h := q.registry[c.worker]
	if h == nil {
		q.log.Error("jobs: no handler registered; discarding", "worker", c.worker, "id", c.id)
		q.settleDiscard(c, "no handler for worker "+c.worker)
		return
	}

	j := Job{ID: c.id, Queue: c.queue, Worker: c.worker, Args: c.args, Attempt: c.attempt, MaxAttempts: c.maxAttempts}
	if c.cred != nil {
		pt, err := q.cfg.Keyring.Decrypt(c.cred.ciphertext, c.cred.nonce, c.cred.keyID)
		if err != nil {
			// Fail-safe (D12): a rotated-out or tampered credential is never
			// worked around via an unmetered relay — discard and let D5b surface.
			q.log.Warn("jobs: credential undecryptable; discarding", "id", c.id, "key_id", c.cred.keyID, "err", err)
			q.settleDiscard(c, "credential undecryptable")
			return
		}
		j.Credential = pt
		defer zero(pt)
	}

	err := runHandler(ctx, h, j)

	switch {
	case err == nil:
		q.settle(c, q.store.complete)
	case IsDiscard(err):
		q.settleDiscard(c, err.Error())
	case c.attempt >= c.maxAttempts:
		q.log.Warn("jobs: giving up (attempts exhausted)", "id", c.id, "worker", c.worker, "attempts", c.attempt, "err", err)
		q.settleDiscard(c, err.Error())
	default:
		delay := q.backoff(c.attempt)
		q.log.Warn("jobs: rescheduling after transient failure", "id", c.id, "worker", c.worker, "attempt", c.attempt, "next_in", delay, "err", err)
		q.settle(c, func(sctx context.Context, c *claimed) error {
			return q.store.reschedule(sctx, c, time.Now().Add(delay), err.Error())
		})
	}
}

// runHandler invokes h, converting a panic into a (retryable) error.
func runHandler(ctx context.Context, h Handler, j Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("jobs: handler panic: %v", r)
		}
	}()
	return h(ctx, j)
}

// backoff returns the delay before retry after failure number attempt (1-based).
func (q *Queue) backoff(attempt int) time.Duration {
	b := q.cfg.Backoff
	i := attempt - 1
	if i >= len(b) {
		i = len(b) - 1
	}
	if i < 0 {
		i = 0
	}
	return b[i]
}

// settle runs a terminal/reschedule write on a detached, time-bounded context
// so a job that finished during shutdown still records its outcome (rather than
// being left 'executing' for the reclaim sweep). The write applies only while
// the row is still this claim's: a job reclaimed (and perhaps claimed again),
// cancelled or re-queued while its handler ran is left alone.
func (q *Queue) settle(c *claimed, write func(context.Context, *claimed) error) {
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch err := write(sctx, c); {
	case errors.Is(err, errStaleClaim):
		q.log.Warn("jobs: settle skipped; job changed while executing", "id", c.id, "attempt", c.attempt)
	case err != nil:
		q.log.Warn("jobs: settle failed (row stays executing; reclaim will recover)", "id", c.id, "err", err)
	}
}

func (q *Queue) settleDiscard(c *claimed, lastErr string) {
	q.settle(c, func(ctx context.Context, c *claimed) error { return q.store.discard(ctx, c, lastErr) })
}

// sweepLoop periodically reclaims orphaned jobs, purges aged credentials, prunes
// settled rows, and wakes workers for jobs whose scheduled_at has since passed.
func (q *Queue) sweepLoop(ctx context.Context) {
	t := time.NewTicker(q.cfg.SweepInterval)
	defer t.Stop()
	q.sweep(ctx) // once at startup
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			q.sweep(ctx)
		}
	}
}

func (q *Queue) sweep(ctx context.Context) {
	now := time.Now()
	if n, err := q.store.reclaimOrphans(ctx, now.Add(-q.cfg.ReclaimAfter)); err != nil {
		q.log.Warn("jobs: reclaim sweep failed", "err", err)
	} else if n > 0 {
		q.log.Info("jobs: reclaimed orphaned jobs", "count", n)
	}
	if n, err := q.store.purgeCredentials(ctx, now.Add(-q.cfg.CredentialTTL)); err != nil {
		q.log.Warn("jobs: credential purge failed", "err", err)
	} else if n > 0 {
		q.log.Info("jobs: purged aged credentials", "count", n)
	}
	if _, err := q.store.pruneTerminal(ctx, now.Add(-q.cfg.Retention)); err != nil {
		q.log.Warn("jobs: prune failed", "err", err)
	}
	q.signalWake() // future-scheduled rows may now be due; reclaim may have freed rows
}

// listenLoop holds a dedicated pgx connection LISTENing for NOTIFY jobs and
// wakes the workers on each notification, reconnecting on drop. Mirrors
// internal/push's worker.listen.
func (q *Queue) listenLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if err := q.listen(ctx); err != nil && ctx.Err() == nil {
			q.log.Warn("jobs: listen connection dropped; retrying", "err", err)
			if q.sleep(ctx, 5*time.Second) {
				return
			}
		}
	}
}

func (q *Queue) listen(ctx context.Context) error {
	cfg, err := pgdial.Config(q.cfg.DSN)
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
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		q.signalWake()
	}
}

// signalWake nudges every idle worker to poll (best-effort; a full buffer means
// enough wakeups are already pending).
func (q *Queue) signalWake() {
	for i := 0; i < cap(q.wake); i++ {
		select {
		case q.wake <- struct{}{}:
		default:
			return
		}
	}
}

// sleep waits for a wake, the timeout, or cancellation; it reports whether ctx
// was cancelled (the caller should return).
func (q *Queue) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-q.wake:
		return false
	case <-t.C:
		return false
	}
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
