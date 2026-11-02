package jobs

// Postgres-backed tests for the durable queue. They build a scratch database
// from the sabre + calendsd schema (the jobs table lives in the calendsd
// extension schema) and drive the claim → process → settle state machine
// directly, so outcomes are deterministic without racing the worker goroutines.
// Skips automatically when the dev Postgres isn't reachable, like the storage
// and push packages' Postgres tests.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx database/sql driver

	"github.com/migadu/calendsd/internal/storage/storagetest"
)

func testDSN(dbName string) string {
	return "postgres:///" + dbName + "?host=" + os.Getenv("PGHOST")
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func openScratchDB(t *testing.T, dbName string) *sql.DB {
	return storagetest.NewDB(t, dbName)
}

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	kr, err := NewKeyring(Key{ID: 1, Key: bytes.Repeat([]byte("k"), 32)})
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

// newTestQueue builds a queue whose backoff is zero so a rescheduled job is
// immediately due again — the tests drive claim/process by hand.
func newTestQueue(t *testing.T, db *sql.DB) *Queue {
	t.Helper()
	return New(db, Config{
		Keyring: testKeyring(t),
		Backoff: []time.Duration{0},
	})
}

// imipArgs is a minimal args payload carrying the coalesce keys.
func imipArgs(uid, recipient, method, seq, dtstamp string) map[string]any {
	return map[string]any{
		"uid": uid, "recipient": recipient, "method": method,
		"sequence": seq, "dtstamp": dtstamp, "message_b64": "Zm9v",
	}
}

type jobRow struct {
	state   string
	attempt int
	maxAtt  int
	credSet bool
	args    json.RawMessage
	lastErr sql.NullString
}

func getJob(t *testing.T, db *sql.DB, id int64) jobRow {
	t.Helper()
	var r jobRow
	err := db.QueryRow(`SELECT state, attempt, max_attempts, enc_credential IS NOT NULL, args, last_error
		FROM jobs WHERE id = $1`, id).Scan(&r.state, &r.attempt, &r.maxAtt, &r.credSet, &r.args, &r.lastErr)
	if err != nil {
		t.Fatalf("getJob(%d): %v", id, err)
	}
	return r
}

func lastJobID(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM jobs ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("lastJobID: %v", err)
	}
	return id
}

func countJobs(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// claimOne claims and processes the next due job, returning its id (0 if none).
func (q *Queue) claimOne(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	c, err := q.store.claim(context.Background(), "default")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if c == nil {
		return 0
	}
	q.process(context.Background(), c)
	return c.id
}

// TestCompleteClearsCredential: a relay job runs once and completes; a per-user
// job's credential is decrypted for the handler and NULLed on success (D12).
func TestCompleteClearsCredential(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_complete")
	q := newTestQueue(t, db)

	var gotCred []byte
	q.Register("imip_send", func(_ context.Context, j Job) error {
		gotCred = append([]byte(nil), j.Credential...) // copy before the queue zeroes it
		return nil
	})

	if err := q.Enqueue(context.Background(), "imip_send",
		imipArgs("u1", "mailto:a@x.test", "REQUEST", "0", "20260101T000000Z"),
		WithCredential([]byte("s3cret"))); err != nil {
		t.Fatal(err)
	}
	id := lastJobID(t, db)

	if got := q.claimOne(t, db); got != id {
		t.Fatalf("claimed %d, want %d", got, id)
	}
	if !bytes.Equal(gotCred, []byte("s3cret")) {
		t.Errorf("handler saw credential %q, want %q", gotCred, "s3cret")
	}
	r := getJob(t, db, id)
	if r.state != "completed" {
		t.Errorf("state = %q, want completed", r.state)
	}
	if r.credSet {
		t.Error("enc_credential should be NULL after completion")
	}
}

// TestRescheduleThenComplete: a transient failure reschedules (credential kept),
// the retry succeeds. Exercises retry-to-success and the attempt counter.
func TestRescheduleThenComplete(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_retry")
	q := newTestQueue(t, db)

	calls := 0
	q.Register("imip_send", func(_ context.Context, j Job) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded // transient
		}
		return nil
	})

	if err := q.Enqueue(context.Background(), "imip_send",
		imipArgs("u2", "mailto:b@x.test", "REQUEST", "0", "20260101T000000Z"),
		WithCredential([]byte("pw"))); err != nil {
		t.Fatal(err)
	}
	id := lastJobID(t, db)

	q.claimOne(t, db) // attempt 1 → transient → rescheduled (backoff 0, due now)
	if r := getJob(t, db, id); r.state != "available" || r.attempt != 1 || !r.credSet {
		t.Fatalf("after fail: state=%q attempt=%d credSet=%v, want available/1/true", r.state, r.attempt, r.credSet)
	}
	q.claimOne(t, db) // attempt 2 → success
	if r := getJob(t, db, id); r.state != "completed" || r.attempt != 2 || r.credSet {
		t.Fatalf("after success: state=%q attempt=%d credSet=%v, want completed/2/false", r.state, r.attempt, r.credSet)
	}
	if calls != 2 {
		t.Errorf("handler called %d times, want 2", calls)
	}
}

// TestDiscardRetainsCredential: a permanent failure (Discard) is terminal now
// and KEEPS the credential so a manual `calctl imip resend` can reuse it (D5b).
func TestDiscardRetainsCredential(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_discard")
	q := newTestQueue(t, db)

	q.Register("imip_send", func(_ context.Context, j Job) error {
		return Discard(context.Canceled) // permanent
	})
	if err := q.Enqueue(context.Background(), "imip_send",
		imipArgs("u3", "mailto:c@x.test", "REQUEST", "0", "20260101T000000Z"),
		WithCredential([]byte("pw"))); err != nil {
		t.Fatal(err)
	}
	id := lastJobID(t, db)
	q.claimOne(t, db)
	r := getJob(t, db, id)
	if r.state != "discarded" {
		t.Errorf("state = %q, want discarded", r.state)
	}
	if !r.credSet {
		t.Error("enc_credential should be retained on discard (D5b manual resend)")
	}
}

// TestReclaimOrphan: a job stuck 'executing' past reclaim_after (a worker that
// died mid-send) is returned to 'available' by the sweep (D9).
func TestReclaimOrphan(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_reclaim")
	q := newTestQueue(t, db)
	q.Register("imip_send", func(context.Context, Job) error { return nil })

	if err := q.Enqueue(context.Background(), "imip_send",
		imipArgs("u4", "mailto:d@x.test", "REQUEST", "0", "20260101T000000Z")); err != nil {
		t.Fatal(err)
	}
	id := lastJobID(t, db)
	// Claim to move it to 'executing', then simulate a crash by backdating the
	// claim and NOT settling.
	if _, err := q.store.claim(context.Background(), "default"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE jobs SET attempted_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	n, err := q.store.reclaimOrphans(context.Background(), time.Now().Add(-defaultReclaimAfter))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d, want 1", n)
	}
	if r := getJob(t, db, id); r.state != "available" {
		t.Errorf("state = %q, want available after reclaim", r.state)
	}
}

// TestSweepReclaimsBeforePurge: the startup/periodic sweep reclaims orphaned
// executing jobs *before* it purges credentials, so a job that was executing
// at a crash and is older than both windows comes back available with its
// credential intact instead of being purged and then discarded by its handler
// (audit 3.5).
func TestSweepReclaimsBeforePurge(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_sweeporder")
	q := newTestQueue(t, db)
	ctx := context.Background()
	if err := q.Enqueue(ctx, "imip_send",
		imipArgs("u5", "mailto:e@x.test", "REQUEST", "0", "20260101T000000Z"),
		WithCredential([]byte("pw"))); err != nil {
		t.Fatal(err)
	}
	id := lastJobID(t, db)
	if _, err := q.store.claim(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	// Crashed mid-execution, long enough ago for both the reclaim window and
	// the credential TTL to have elapsed.
	if _, err := db.Exec(`UPDATE jobs SET attempted_at = now() - interval '3 hours',
	                                      scheduled_at = now() - interval '3 hours' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	q.sweep(ctx)
	r := getJob(t, db, id)
	if r.state != "available" {
		t.Errorf("state = %q, want available after sweep", r.state)
	}
	if !r.credSet {
		t.Error("a reclaimed job must keep its credential (reclaim resets scheduled_at before the purge looks)")
	}
}

// TestPurgeCredentialsTracksSchedule: a credential is dropped only once the job
// stops needing it — kept while a retry is still scheduled (a future
// scheduled_at, even if the row was inserted long ago), dropped after give-up
// (discarded past grace) or when an available job is overdue past grace. Proves
// the purge tracks the (backoff-lengthening) schedule, not a fixed age from
// insert (D12).
func TestPurgeCredentialsTracksSchedule(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_purge")
	q := newTestQueue(t, db)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	enq := func(uid string) int64 {
		if err := q.Enqueue(ctx, "imip_send",
			imipArgs(uid, "mailto:"+uid+"@x.test", "REQUEST", "0", "20260101T000000Z"),
			WithCredential([]byte("pw"))); err != nil {
			t.Fatal(err)
		}
		return lastJobID(t, db)
	}
	retrying := enq("retry")  // available, next attempt still in the future
	overdue := enq("overdue") // available, scheduled_at long past (abandoned)
	gaveUp := enq("gaveup")   // discarded a while ago

	// Age every insert far past the grace, to prove the purge does NOT key off it.
	exec(`UPDATE jobs SET inserted_at = now() - interval '10 hours'`)
	exec(`UPDATE jobs SET scheduled_at = now() + interval '20 minutes' WHERE id=$1`, retrying)
	exec(`UPDATE jobs SET scheduled_at = now() - interval '2 hours'   WHERE id=$1`, overdue)
	exec(`UPDATE jobs SET state='discarded', completed_at = now() - interval '2 hours' WHERE id=$1`, gaveUp)

	discarded, available, err := q.store.purgeCredentials(ctx, time.Now().Add(-time.Hour)) // grace = 1h
	if err != nil {
		t.Fatal(err)
	}
	if discarded != 1 || available != 1 {
		t.Fatalf("purged discarded=%d available=%d credentials, want 1 + 1 (gave-up + overdue)", discarded, available)
	}
	if !getJob(t, db, retrying).credSet {
		t.Error("a job with a future retry must keep its credential")
	}
	if getJob(t, db, overdue).credSet {
		t.Error("an overdue/abandoned available job should have its credential purged")
	}
	if getJob(t, db, gaveUp).credSet {
		t.Error("a given-up job past the grace should have its credential purged")
	}
}

// TestCoalesceSupersede: a newer revision (higher SEQUENCE) replaces the pending
// job's body; an older/equal re-issue is a no-op — one row throughout (D10).
func TestCoalesceSupersede(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_coalesce")
	q := newTestQueue(t, db)
	ctx := context.Background()

	enq := func(seq string) {
		if err := q.Enqueue(ctx, "imip_send",
			imipArgs("uc", "mailto:e@x.test", "REQUEST", seq, "20260101T00000"+seq+"Z")); err != nil {
			t.Fatal(err)
		}
	}
	enq("1")
	enq("2") // newer → supersedes
	enq("1") // older → no-op

	if n := countJobs(t, db); n != 1 {
		t.Fatalf("job count = %d, want 1 (coalesced)", n)
	}
	id := lastJobID(t, db)
	var seq string
	if err := db.QueryRow(`SELECT args->>'sequence' FROM jobs WHERE id = $1`, id).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != "2" {
		t.Errorf("pending job SEQUENCE = %q, want 2 (newest revision wins)", seq)
	}
}

// TestUndecryptableCredentialDiscards: a credential whose key is not in the ring
// (rotated out / tampered) fail-safes to discard, never an unmetered send (D12).
func TestUndecryptableCredentialDiscards(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_badkey")
	q := newTestQueue(t, db)
	sent := false
	q.Register("imip_send", func(context.Context, Job) error { sent = true; return nil })

	if err := q.Enqueue(context.Background(), "imip_send",
		imipArgs("u5", "mailto:f@x.test", "REQUEST", "0", "20260101T000000Z"),
		WithCredential([]byte("pw"))); err != nil {
		t.Fatal(err)
	}
	id := lastJobID(t, db)
	// Corrupt the stored key_id so no ring entry matches.
	if _, err := db.Exec(`UPDATE jobs SET key_id = 99 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	q.claimOne(t, db)
	if sent {
		t.Error("handler ran despite an undecryptable credential")
	}
	if r := getJob(t, db, id); r.state != "discarded" {
		t.Errorf("state = %q, want discarded (fail-safe)", r.state)
	}
}

// TestEnqueueCredentialWithoutKeyring: without a key configured, a per-user
// enqueue is refused (the caller must fall back to the RAM tier / D5b).
func TestEnqueueCredentialWithoutKeyring(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_nokey")
	q := New(db, Config{}) // no keyring
	err := q.Enqueue(context.Background(), "imip_send",
		imipArgs("u6", "mailto:g@x.test", "REQUEST", "0", "20260101T000000Z"),
		WithCredential([]byte("pw")))
	if err == nil {
		t.Fatal("expected error enqueuing a credentialed job without a keyring")
	}
	if n := countJobs(t, db); n != 0 {
		t.Errorf("job count = %d, want 0 (enqueue must not persist)", n)
	}
}

// TestEmptyBackoffUsesDefault: an empty (non-nil) Backoff, e.g. from a config
// list with no entries, falls back to the default instead of indexing an
// empty slice on the first retry.
func TestEmptyBackoffUsesDefault(t *testing.T) {
	q := New(nil, Config{Backoff: []time.Duration{}})
	if got := q.backoff(1); got != defaultBackoff[0] {
		t.Errorf("backoff(1) = %v, want %v", got, defaultBackoff[0])
	}
}

// TestWaitSettlesInFlightJob: after the queue's ctx is cancelled, Wait returns
// only once a running handler has returned and its outcome is recorded, so a
// shutdown does not leave the row 'executing' for the reclaim sweep.
func TestWaitSettlesInFlightJob(t *testing.T) {
	db := openScratchDB(t, "calserver_f_jobs_wait")
	q := newTestQueue(t, db)
	started := make(chan struct{})
	q.Register("imip_send", func(ctx context.Context, j Job) error {
		close(started)
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond) // a send unwinding after cancel
		return ctx.Err()
	})
	if err := q.Enqueue(context.Background(), "imip_send",
		imipArgs("u8", "mailto:w@x.test", "REQUEST", "0", "20260101T000000Z")); err != nil {
		t.Fatal(err)
	}
	id := lastJobID(t, db)

	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("handler never started")
	}
	cancel()
	q.Wait()
	if r := getJob(t, db, id); r.state != "available" || r.attempt != 1 {
		t.Errorf("after Wait: state=%q attempt=%d, want available/1 (rescheduled)", r.state, r.attempt)
	}
}

// TestSettleIgnoresStaleClaim: a handler that outlives its claim — the job was
// reclaimed and claimed again, or cancelled — must not settle the row the
// newer claim (or the operator) now owns.
func TestSettleIgnoresStaleClaim(t *testing.T) {
	db := openScratchDB(t, "calserver_f_jobs_stale_settle")
	q := newTestQueue(t, db)
	ctx := context.Background()
	q.Register("imip_send", func(context.Context, Job) error { return nil })

	if err := q.Enqueue(ctx, "imip_send",
		imipArgs("u7", "mailto:h@x.test", "REQUEST", "0", "20260101T000000Z")); err != nil {
		t.Fatal(err)
	}
	id := lastJobID(t, db)
	first, err := q.store.claim(ctx, "default")
	if err != nil || first == nil {
		t.Fatalf("claim: %v, %v", first, err)
	}
	// Reclaim it and claim it again: attempt 2 now owns the row.
	if _, err := db.Exec(`UPDATE jobs SET attempted_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := q.store.reclaimOrphans(ctx, time.Now().Add(-defaultReclaimAfter)); err != nil {
		t.Fatal(err)
	}
	second, err := q.store.claim(ctx, "default")
	if err != nil || second == nil || second.attempt != 2 {
		t.Fatalf("second claim: %+v, %v", second, err)
	}

	// The first worker finishes, fails or gives up: none of it may touch
	// attempt 2's row.
	for name, write := range map[string]func() error{
		"complete":   func() error { return q.store.complete(ctx, first) },
		"reschedule": func() error { return q.store.reschedule(ctx, first, time.Now(), "late") },
		"discard":    func() error { return q.store.discard(ctx, first, "late") },
	} {
		if err := write(); !errors.Is(err, errStaleClaim) {
			t.Errorf("%s with a stale claim = %v, want errStaleClaim", name, err)
		}
		if r := getJob(t, db, id); r.state != "executing" || r.attempt != 2 {
			t.Fatalf("after stale %s: state=%q attempt=%d, want executing/2", name, r.state, r.attempt)
		}
	}

	// A job cancelled while executing stays cancelled when its handler ends.
	if err := (&Ops{DB: db}).Cancel(id); err != nil {
		t.Fatal(err)
	}
	q.process(ctx, second)
	if r := getJob(t, db, id); r.state != "discarded" || r.lastErr.String != "cancelled via calctl" {
		t.Errorf("after cancel + completion: state=%q err=%q, want discarded/cancelled", r.state, r.lastErr.String)
	}
}
