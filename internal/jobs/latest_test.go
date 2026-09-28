package jobs

// EnqueueLatest (docs/share-invite-email.md): latest-wins coalescing per
// (worker, key), and retry/reclaim yielding to the newer generation instead of
// violating jobs_latest_ix.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

const latestWorker = "share_notify"

func enqueueLatest(t *testing.T, q *Queue, key string, gen int64, marker string) {
	t.Helper()
	if err := q.EnqueueLatest(context.Background(), latestWorker, key, gen,
		map[string]any{"marker": marker}); err != nil {
		t.Fatalf("EnqueueLatest(%s, %d): %v", key, gen, err)
	}
}

func argsOf(t *testing.T, r jobRow) (gen int64, marker string) {
	t.Helper()
	var a struct {
		Generation int64  `json:"generation"`
		Marker     string `json:"marker"`
		Key        string `json:"coalesce_key"`
	}
	if err := json.Unmarshal(r.args, &a); err != nil {
		t.Fatal(err)
	}
	return a.Generation, a.Marker
}

// jobsFor lists (id, state, generation) for a key, oldest first.
func jobsFor(t *testing.T, q *Queue, key string) (ids []int64, states []string, gens []int64) {
	t.Helper()
	rows, err := q.store.db.Query(`
SELECT id, state, (args->>'generation')::bigint FROM jobs
 WHERE worker = $1 AND args->>'coalesce_key' = $2 ORDER BY id`, latestWorker, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, gen int64
		var state string
		if err := rows.Scan(&id, &state, &gen); err != nil {
			t.Fatal(err)
		}
		ids, states, gens = append(ids, id), append(states, state), append(gens, gen)
	}
	return
}

// claimOnly moves the one due job to 'executing' without running it.
func claimOnly(t *testing.T, q *Queue) *claimed {
	t.Helper()
	c, err := q.store.claim(context.Background(), "default")
	if err != nil || c == nil {
		t.Fatalf("claim = %v, %v", c, err)
	}
	return c
}

func TestEnqueueLatestSupersede(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_latest")
	q := newTestQueue(t, db)

	enqueueLatest(t, q, "42|principals/bob", 5, "first")
	enqueueLatest(t, q, "42|principals/bob", 7, "newer") // replaces
	enqueueLatest(t, q, "42|principals/bob", 6, "older") // a delayed enqueue: no-op
	enqueueLatest(t, q, "42|principals/bob", 7, "equal") // no-op
	if n := countJobs(t, db); n != 1 {
		t.Fatalf("jobs = %d, want 1 pending for the key", n)
	}
	if gen, marker := argsOf(t, getJob(t, db, lastJobID(t, db))); gen != 7 || marker != "newer" {
		t.Fatalf("pending = gen %d %q, want gen 7 \"newer\"", gen, marker)
	}

	// Another key, and an iMIP job, never coalesce with it.
	enqueueLatest(t, q, "42|principals/carol", 3, "carol")
	if err := q.Enqueue(context.Background(), "imip_send",
		imipArgs("u1", "mailto:bob@x.test", "REQUEST", "0", "20260101T000000Z")); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), "imip_send",
		imipArgs("u1", "mailto:bob@x.test", "REQUEST", "1", "20260101T000000Z")); err != nil {
		t.Fatal(err)
	}
	if n := countJobs(t, db); n != 3 {
		t.Fatalf("jobs = %d, want 3 (bob, carol, one coalesced iMIP)", n)
	}

	// Coalescing covers pending jobs only: once bob's is executing, a newer
	// generation queues behind it rather than rewriting the running job.
	claimOnlyWorker(t, q, "42|principals/bob")
	enqueueLatest(t, q, "42|principals/bob", 8, "behind")
	_, states, gens := jobsFor(t, q, "42|principals/bob")
	if len(states) != 2 || states[0] != "executing" || gens[0] != 7 || states[1] != "available" || gens[1] != 8 {
		t.Fatalf("states=%v gens=%v, want [executing(7) available(8)]", states, gens)
	}
}

func TestEnqueueLatestRefusals(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_latest_refuse")
	q := newTestQueue(t, db)
	ctx := context.Background()
	if err := q.EnqueueLatest(ctx, latestWorker, "k", 1, map[string]any{}, WithCredential([]byte("pw"))); err == nil {
		t.Error("credential accepted")
	}
	if err := q.EnqueueLatest(ctx, latestWorker, "", 1, map[string]any{}); err == nil {
		t.Error("empty key accepted")
	}
	if err := q.EnqueueLatest(ctx, latestWorker, "k", 1, []int{1}); err == nil {
		t.Error("non-object args accepted")
	}
	if n := countJobs(t, db); n != 0 {
		t.Fatalf("jobs = %d after refusals", n)
	}
}

// TestLatestRetryYieldsToNewerPending: a job that fails while a sibling for
// its key was enqueued must not collide with it when rescheduled — the higher
// generation survives either way round.
func TestLatestRetryYieldsToNewerPending(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_latest_retry")
	q := newTestQueue(t, db)
	q.Register(latestWorker, func(context.Context, Job) error { return errors.New("smtp blip") })

	// Executing gen 5 fails after gen 7 was enqueued: gen 5 is superseded.
	enqueueLatest(t, q, "a", 5, "old")
	c := claimOnly(t, q)
	enqueueLatest(t, q, "a", 7, "new")
	q.process(context.Background(), c)
	_, states, gens := jobsFor(t, q, "a")
	if len(states) != 2 || states[0] != "discarded" || states[1] != "available" || gens[1] != 7 {
		t.Fatalf("after retry: states=%v gens=%v, want [discarded available(7)]", states, gens)
	}

	// Executing gen 9 fails after a delayed gen 8 enqueue: gen 8 is superseded
	// and gen 9 goes back to pending.
	enqueueLatest(t, q, "b", 9, "new")
	c = claimOnlyWorker(t, q, "b")
	enqueueLatest(t, q, "b", 8, "delayed")
	q.process(context.Background(), c)
	_, states, gens = jobsFor(t, q, "b")
	if len(states) != 2 || states[0] != "available" || gens[0] != 9 || states[1] != "discarded" {
		t.Fatalf("after retry: states=%v gens=%v, want [available(9) discarded(8)]", states, gens)
	}
}

// claimOnlyWorker claims the pending job for key, leaving others pending.
func claimOnlyWorker(t *testing.T, q *Queue, key string) *claimed {
	t.Helper()
	// Push every other available job into the future so claim picks key's.
	if _, err := q.store.db.Exec(`
UPDATE jobs SET scheduled_at = now() + interval '1 hour'
 WHERE state = 'available' AND COALESCE(args->>'coalesce_key', '') <> $1`, key); err != nil {
		t.Fatal(err)
	}
	c := claimOnly(t, q)
	if _, err := q.store.db.Exec(`
UPDATE jobs SET scheduled_at = now() WHERE state = 'available'`); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestLatestReclaimYieldsToNewest: reclaiming orphaned latest-wins jobs keeps
// only the newest generation per key pending — against a pending sibling in
// either order, and between two orphans — and still reclaims other jobs.
func TestLatestReclaimYieldsToNewest(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_latest_reclaim")
	q := newTestQueue(t, db)
	ctx := context.Background()
	orphan := func(key string) {
		t.Helper()
		c := claimOnlyWorker(t, q, key)
		if _, err := db.Exec(`UPDATE jobs SET attempted_at = now() - interval '1 hour' WHERE id = $1`, c.id); err != nil {
			t.Fatal(err)
		}
	}

	// a: orphan gen 5, pending gen 7 → the orphan is superseded.
	enqueueLatest(t, q, "a", 5, "")
	orphan("a")
	enqueueLatest(t, q, "a", 7, "")
	// b: orphan gen 9, pending (delayed) gen 8 → the pending one yields.
	enqueueLatest(t, q, "b", 9, "")
	orphan("b")
	enqueueLatest(t, q, "b", 8, "")
	// c: two orphans, gen 3 and gen 4 → only gen 4 returns.
	enqueueLatest(t, q, "c", 3, "")
	orphan("c")
	enqueueLatest(t, q, "c", 4, "")
	orphan("c")
	// An ordinary iMIP orphan alongside.
	if err := q.Enqueue(ctx, "imip_send", imipArgs("u9", "mailto:z@x.test", "REQUEST", "0", "20260101T000000Z")); err != nil {
		t.Fatal(err)
	}
	orphan("")

	if _, err := q.store.reclaimOrphans(ctx, time.Now().Add(-defaultReclaimAfter)); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	want := map[string][2][]any{
		"a": {{"discarded", "available"}, {int64(5), int64(7)}},
		"b": {{"available", "discarded"}, {int64(9), int64(8)}},
		"c": {{"discarded", "available"}, {int64(3), int64(4)}},
	}
	for key, w := range want {
		_, states, gens := jobsFor(t, q, key)
		for i := range states {
			if states[i] != w[0][i] || gens[i] != w[1][i] {
				t.Errorf("key %s: states=%v gens=%v, want %v %v", key, states, gens, w[0], w[1])
				break
			}
		}
	}
	var imipState string
	if err := db.QueryRow(`SELECT state FROM jobs WHERE worker = 'imip_send'`).Scan(&imipState); err != nil {
		t.Fatal(err)
	}
	if imipState != "available" {
		t.Errorf("iMIP orphan = %q, want reclaimed to available", imipState)
	}
}

// TestOpsRequeueRespectsLatest: calctl's retry/relay re-queue of a
// latest-wins job yields to a pending job of its key with the same or a newer
// generation, and retires an older one — instead of hitting jobs_latest_ix.
func TestOpsRequeueRespectsLatest(t *testing.T) {
	db := openScratchDB(t, "calserver_jobs_latest_ops")
	q := newTestQueue(t, db)
	ops := &Ops{DB: db}
	discard := func(key string) int64 {
		t.Helper()
		ids, _, _ := jobsFor(t, q, key)
		id := ids[len(ids)-1]
		if _, err := db.Exec(`UPDATE jobs SET state = 'discarded' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	// An old generation, given up; a newer one pending: retry is refused.
	enqueueLatest(t, q, "a", 5, "old")
	old := discard("a")
	enqueueLatest(t, q, "a", 7, "new")
	if err := ops.Retry(old); !errors.Is(err, ErrNewerPending) {
		t.Fatalf("Retry of a superseded job = %v, want ErrNewerPending", err)
	}
	if err := ops.RelayOverride(old); !errors.Is(err, ErrNewerPending) {
		t.Fatalf("RelayOverride of a superseded job = %v, want ErrNewerPending", err)
	}

	// A newer generation, given up; an older one pending: the retry wins.
	enqueueLatest(t, q, "b", 9, "new")
	newer := discard("b")
	enqueueLatest(t, q, "b", 8, "delayed")
	if err := ops.Retry(newer); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	_, states, gens := jobsFor(t, q, "b")
	if len(states) != 2 || states[0] != "available" || gens[0] != 9 || states[1] != "discarded" {
		t.Fatalf("after retry: states=%v gens=%v, want [available(9) discarded(8)]", states, gens)
	}

	// Errors for missing and completed jobs are unchanged.
	if err := ops.Retry(999999); !errors.Is(err, ErrNoJob) {
		t.Errorf("Retry(missing) = %v", err)
	}
}
