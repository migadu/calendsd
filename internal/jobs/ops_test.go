package jobs

// Postgres-backed tests for the operator query/mutation layer (Ops), used by
// `calctl jobs`. They assert the state transitions and — critically — that the
// credential is never exposed, only its presence.

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

// enqueuePerUser inserts an imip_send job carrying an encrypted credential and
// returns its id.
func enqueuePerUser(t *testing.T, q *Queue, uid, recipient, secret string) int64 {
	t.Helper()
	args := map[string]any{
		"uid": uid, "recipient": recipient, "method": "REQUEST",
		"username": "alice", "envelope_from": "alice@x.test",
		"sequence": "0", "dtstamp": "20260101T000000Z", "message_b64": "Zm9v",
	}
	if err := q.Enqueue(context.Background(), "imip_send", args, WithCredential([]byte(secret))); err != nil {
		t.Fatal(err)
	}
	return lastJobID(t, q.store.db)
}

func TestOpsListAndRedaction(t *testing.T) {
	db := openScratchDB(t, "calserver_ops_list")
	q := newTestQueue(t, db)
	ops := &Ops{DB: db}

	id := enqueuePerUser(t, q, "u1", "mailto:a@x.test", "s3cr3t-pw")
	// A relay job too (no credential).
	if err := q.Enqueue(context.Background(), "imip_send", map[string]any{
		"uid": "u2", "recipient": "mailto:b@x.test", "method": "REQUEST",
		"sequence": "0", "dtstamp": "20260101T000000Z", "message_b64": "Zm9v",
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := ops.List(ListFilter{Worker: "imip_send"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("List returned %d rows, want 2", len(rows))
	}

	j, err := ops.Get(id)
	if err != nil || j == nil {
		t.Fatalf("Get(%d) = %v, %v", id, j, err)
	}
	if !j.HasCred || j.KeyID == nil || *j.KeyID != 1 {
		t.Errorf("per-user job: HasCred=%v KeyID=%v, want true / 1", j.HasCred, j.KeyID)
	}
	if j.Username != "alice" || j.Recipient != "mailto:a@x.test" {
		t.Errorf("Get lifted args wrong: username=%q recipient=%q", j.Username, j.Recipient)
	}
	// The secret must not appear anywhere in the operator view — not in args, not
	// in any field. Marshal the whole row and grep.
	blob, _ := json.Marshal(j)
	if bytes.Contains(blob, []byte("s3cr3t-pw")) {
		t.Fatal("Get exposed the plaintext credential")
	}
	if bytes.Contains(j.Args, []byte("s3cr3t-pw")) {
		t.Fatal("credential leaked into args (D8 violation)")
	}
}

func TestOpsListFailedFilter(t *testing.T) {
	db := openScratchDB(t, "calserver_ops_failed")
	q := newTestQueue(t, db)
	ops := &Ops{DB: db}

	avail := enqueuePerUser(t, q, "u1", "mailto:a@x.test", "pw") // available, attempt 0 → not "failed"
	disc := enqueuePerUser(t, q, "u2", "mailto:b@x.test", "pw")
	if _, err := db.Exec(`UPDATE jobs SET state='discarded' WHERE id=$1`, disc); err != nil {
		t.Fatal(err)
	}

	rows, err := ops.List(ListFilter{Failed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != disc {
		t.Fatalf("--failed returned %+v, want just the discarded job %d", rows, disc)
	}
	if avail == 0 {
		t.Fatal("setup")
	}
}

func TestOpsRetry(t *testing.T) {
	db := openScratchDB(t, "calserver_ops_retry")
	q := newTestQueue(t, db)
	ops := &Ops{DB: db}

	id := enqueuePerUser(t, q, "u1", "mailto:a@x.test", "pw")
	// Simulate a given-up job: discarded, several attempts.
	if _, err := db.Exec(`UPDATE jobs SET state='discarded', attempt=6, last_error='gave up' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := ops.Retry(id); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	r := getJob(t, db, id)
	if r.state != "available" || r.attempt != 0 || r.lastErr.Valid {
		t.Errorf("after retry: state=%q attempt=%d err=%v, want available/0/NULL", r.state, r.attempt, r.lastErr)
	}
	if !r.credSet {
		t.Error("retry must keep the credential so the worker can send")
	}
}

func TestOpsRetryRefusesCompleted(t *testing.T) {
	db := openScratchDB(t, "calserver_ops_retry_done")
	q := newTestQueue(t, db)
	ops := &Ops{DB: db}

	id := enqueuePerUser(t, q, "u1", "mailto:a@x.test", "pw")
	if _, err := db.Exec(`UPDATE jobs SET state='completed', completed_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := ops.Retry(id); err != ErrJobCompleted {
		t.Fatalf("Retry on completed = %v, want ErrJobCompleted", err)
	}
	if err := ops.Retry(999999); err != ErrNoJob {
		t.Fatalf("Retry on missing = %v, want ErrNoJob", err)
	}
}

func TestOpsRelayOverride(t *testing.T) {
	db := openScratchDB(t, "calserver_ops_relay")
	q := newTestQueue(t, db)
	ops := &Ops{DB: db}

	id := enqueuePerUser(t, q, "u1", "mailto:a@x.test", "pw")
	if _, err := db.Exec(`UPDATE jobs SET state='discarded', attempt=6 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := ops.RelayOverride(id); err != nil {
		t.Fatalf("RelayOverride: %v", err)
	}
	r := getJob(t, db, id)
	if r.state != "available" || r.attempt != 0 {
		t.Errorf("after relay override: state=%q attempt=%d, want available/0", r.state, r.attempt)
	}
	if r.credSet {
		t.Error("RelayOverride must drop the credential (the worker then relays, unmetered)")
	}
}

func TestOpsCancelDropsCredential(t *testing.T) {
	db := openScratchDB(t, "calserver_ops_cancel")
	q := newTestQueue(t, db)
	ops := &Ops{DB: db}

	id := enqueuePerUser(t, q, "u1", "mailto:a@x.test", "pw")
	if err := ops.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	r := getJob(t, db, id)
	if r.state != "discarded" {
		t.Errorf("state = %q, want discarded", r.state)
	}
	if r.credSet {
		t.Error("Cancel must NULL the retained credential (drop the secret now)")
	}
}

func TestOpsReschedule(t *testing.T) {
	db := openScratchDB(t, "calserver_ops_resched")
	q := newTestQueue(t, db)
	ops := &Ops{DB: db}

	id := enqueuePerUser(t, q, "u1", "mailto:a@x.test", "pw")
	when := time.Now().Add(90 * time.Minute).Truncate(time.Second)
	if err := ops.Reschedule(id, when); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}
	var got time.Time
	if err := db.QueryRow(`SELECT scheduled_at FROM jobs WHERE id=$1`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Truncate(time.Second).Equal(when) {
		t.Errorf("scheduled_at = %s, want %s", got, when)
	}

	// A non-available job can't be rescheduled.
	if _, err := db.Exec(`UPDATE jobs SET state='completed' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := ops.Reschedule(id, when); err == nil {
		t.Error("Reschedule of a completed job should error")
	}
}

func TestOpsDelete(t *testing.T) {
	db := openScratchDB(t, "calserver_ops_delete")
	q := newTestQueue(t, db)
	ops := &Ops{DB: db}

	id := enqueuePerUser(t, q, "u1", "mailto:a@x.test", "pw")
	if err := ops.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if j, _ := ops.Get(id); j != nil {
		t.Error("row still present after Delete")
	}
	if err := ops.Delete(id); err != ErrNoJob {
		t.Fatalf("Delete on missing = %v, want ErrNoJob", err)
	}
}

// TestOpsRequeueRefusesExecuting: re-queueing a job a worker is running would
// let a second worker send it again.
func TestOpsRequeueRefusesExecuting(t *testing.T) {
	db := openScratchDB(t, "calserver_f_ops_requeue_exec")
	q := newTestQueue(t, db)
	ops := &Ops{DB: db}

	id := enqueuePerUser(t, q, "u1", "mailto:a@x.test", "pw")
	if _, err := q.store.claim(context.Background(), "default"); err != nil {
		t.Fatal(err)
	}
	if err := ops.Retry(id); err != ErrJobExecuting {
		t.Errorf("Retry on executing = %v, want ErrJobExecuting", err)
	}
	if err := ops.RelayOverride(id); err != ErrJobExecuting {
		t.Errorf("RelayOverride on executing = %v, want ErrJobExecuting", err)
	}
	if r := getJob(t, db, id); r.state != "executing" || r.attempt != 1 || !r.credSet {
		t.Errorf("job changed: state=%q attempt=%d credSet=%v, want executing/1/true", r.state, r.attempt, r.credSet)
	}
}
