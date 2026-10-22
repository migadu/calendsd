package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Ops is the operator-facing view of the jobs table, for `calctl jobs`. It
// exposes and mutates queue **state** only — it never returns or accepts the
// credential (`enc_credential`), only whether one is present. So the ops tooling
// needs database access but not the encryption key, and no secret reaches a
// terminal or shell history. The worker remains the only component that decrypts
// and sends.
type Ops struct{ DB *sql.DB }

// JobRow is one job as an operator sees it: identity, state, retry accounting,
// the coalesce/routing fields lifted from args, and credential *presence* — never
// the secret itself. Args and Username are populated only by Get (the detail view).
type JobRow struct {
	ID          int64           `json:"id"`
	Queue       string          `json:"queue"`
	Worker      string          `json:"worker"`
	State       string          `json:"state"`
	Recipient   string          `json:"recipient,omitempty"`
	Method      string          `json:"method,omitempty"`
	UID         string          `json:"uid,omitempty"`
	Username    string          `json:"username,omitempty"` // Get only; empty for relay jobs
	Attempt     int             `json:"attempt"`
	MaxAttempts int             `json:"maxAttempts"`
	ScheduledAt time.Time       `json:"scheduledAt"`
	InsertedAt  time.Time       `json:"insertedAt"`
	CompletedAt *time.Time      `json:"completedAt,omitempty"`
	LastError   string          `json:"lastError,omitempty"`
	HasCred     bool            `json:"hasCredential"`
	KeyID       *int16          `json:"keyId,omitempty"`
	Args        json.RawMessage `json:"args,omitempty"` // Get only
}

// ListFilter narrows List. Empty string / false fields don't filter.
type ListFilter struct {
	State     string
	Worker    string
	Queue     string
	Recipient string // substring, case-insensitive
	Failed    bool   // discarded, or available with a prior attempt (the operator view)
	Limit     int    // 0 → 100
}

// List returns matching jobs, newest first. It never selects enc_credential.
func (o *Ops) List(f ListFilter) ([]JobRow, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := o.DB.Query(`
SELECT id, queue, worker, state,
       coalesce(args->>'recipient',''), coalesce(args->>'method',''), coalesce(args->>'uid',''),
       attempt, max_attempts, scheduled_at, inserted_at, completed_at,
       coalesce(last_error,''), enc_credential IS NOT NULL, key_id
  FROM jobs
 WHERE ($1 = '' OR state = $1)
   AND ($2 = '' OR worker = $2)
   AND ($3 = '' OR queue = $3)
   AND ($4 = '' OR args->>'recipient' ILIKE '%' || $4 || '%')
   AND (NOT $5::bool OR state = 'discarded' OR (state = 'available' AND attempt > 0))
 ORDER BY id DESC
 LIMIT $6`,
		f.State, f.Worker, f.Queue, f.Recipient, f.Failed, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRow
	for rows.Next() {
		var (
			r     JobRow
			done  sql.NullTime
			keyID sql.NullInt16
		)
		if err := rows.Scan(&r.ID, &r.Queue, &r.Worker, &r.State,
			&r.Recipient, &r.Method, &r.UID, &r.Attempt, &r.MaxAttempts,
			&r.ScheduledAt, &r.InsertedAt, &done, &r.LastError, &r.HasCred, &keyID); err != nil {
			return nil, err
		}
		if done.Valid {
			r.CompletedAt = &done.Time
		}
		if keyID.Valid {
			r.KeyID = &keyID.Int16
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns one job in full (including its inspectable args, which never hold a
// secret — D8), or nil if there is no such id. enc_credential is reported only as
// HasCred/KeyID, never returned.
func (o *Ops) Get(id int64) (*JobRow, error) {
	var (
		r     JobRow
		raw   []byte
		done  sql.NullTime
		keyID sql.NullInt16
	)
	err := o.DB.QueryRow(`
SELECT id, queue, worker, state, args,
       coalesce(args->>'recipient',''), coalesce(args->>'method',''), coalesce(args->>'uid',''),
       coalesce(args->>'username',''), attempt, max_attempts, scheduled_at, inserted_at,
       completed_at, coalesce(last_error,''), enc_credential IS NOT NULL, key_id
  FROM jobs WHERE id = $1`, id).Scan(
		&r.ID, &r.Queue, &r.Worker, &r.State, &raw,
		&r.Recipient, &r.Method, &r.UID, &r.Username, &r.Attempt, &r.MaxAttempts,
		&r.ScheduledAt, &r.InsertedAt, &done, &r.LastError, &r.HasCred, &keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Args = raw
	if done.Valid {
		r.CompletedAt = &done.Time
	}
	if keyID.Valid {
		r.KeyID = &keyID.Int16
	}
	return &r, nil
}

// ErrJobCompleted is returned by mutations that refuse to touch a job that has
// already been delivered — resending it would duplicate the send; re-enqueue via
// the app instead.
var ErrJobCompleted = errors.New("job already completed")

// ErrNoJob is returned when the id does not exist.
var ErrNoJob = errors.New("no such job")

// ErrJobExecuting is returned by re-queues of a job a worker is running now:
// a second copy would send twice. Wait for it to settle (or for the reclaim
// sweep, if its worker died).
var ErrJobExecuting = errors.New("job is executing; wait for it to settle")

// Retry resets a job to run immediately: state='available', due now, attempt
// count cleared. It refuses a completed job (ErrJobCompleted) and a running one
// (ErrJobExecuting). For a per-user
// job whose credential was purged, the worker will fail-safe discard on the next
// run — callers that know the semantics (calctl imip resend) check HasCred first.
func (o *Ops) Retry(id int64) error {
	return o.requeue(id, `
UPDATE jobs SET state='available', scheduled_at=now(), attempt=0, attempted_at=NULL, last_error=NULL
 WHERE id=$1`)
}

// ErrNewerPending is returned when re-queueing a latest-wins job (one enqueued
// with Queue.EnqueueLatest) whose key already has a pending job of the same or
// a newer generation: that job supersedes this one, and only one may be
// pending (jobs_latest_ix).
var ErrNewerPending = errors.New("a newer job for the same key is already pending and supersedes this one")

// requeue runs a re-queue UPDATE ($1 = id) in a transaction that first locks
// the row, refuses a completed or executing job (ErrJobCompleted /
// ErrJobExecuting), and reconciles a latest-wins job with its key's pending
// sibling the way the queue's own retry does (reconcileSibling): a sibling of
// the same or a newer generation wins (ErrNewerPending); an older one is
// retired as superseded.
func (o *Ops) requeue(id int64, update string) error {
	tx, err := o.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var worker, state string
	var key sql.NullString
	var gen sql.NullInt64
	err = tx.QueryRow(`
SELECT worker, state, args->>'coalesce_key', (args->>'generation')::bigint FROM jobs WHERE id = $1 FOR UPDATE`,
		id).Scan(&worker, &state, &key, &gen)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoJob
	}
	if err != nil {
		return err
	}
	switch state {
	case "completed":
		return ErrJobCompleted
	case "executing":
		return ErrJobExecuting
	}
	if key.Valid {
		superseded, err := reconcileSibling(context.Background(), tx, worker, key.String, id, gen.Int64,
			"superseded by a newer generation re-queued via calctl")
		if err != nil {
			return err
		}
		if superseded {
			return ErrNewerPending
		}
	}
	if _, err := tx.Exec(update, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RelayOverride drops the encrypted credential and re-queues the job so the
// worker delivers it via the **relay** (server) identity — the deliberate,
// unmetered `calctl imip resend --relay` fallback for a per-user send whose
// credential is gone. Refuses a completed or executing job. It never decrypts anything; it
// only clears the credential columns and resets state.
func (o *Ops) RelayOverride(id int64) error {
	return o.requeue(id, `
UPDATE jobs SET state='available', scheduled_at=now(), attempt=0, attempted_at=NULL, last_error=NULL,
       enc_credential=NULL, enc_nonce=NULL, key_id=NULL
 WHERE id=$1`)
}

// Cancel stops delivery: marks the job discarded and NULLs any retained
// credential (drop the secret now). Refuses a completed job.
func (o *Ops) Cancel(id int64) error {
	res, err := o.DB.Exec(`
UPDATE jobs SET state='discarded', completed_at=now(),
       enc_credential=NULL, enc_nonce=NULL, key_id=NULL, last_error='cancelled via calctl'
 WHERE id=$1 AND state <> 'completed'`, id)
	return o.mutated(id, res, err)
}

// Reschedule moves an available job's next-run time (delay it, or bring it
// forward). Only meaningful for an available job.
func (o *Ops) Reschedule(id int64, at time.Time) error {
	res, err := o.DB.Exec(`UPDATE jobs SET scheduled_at=$2 WHERE id=$1 AND state='available'`, id, at)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Distinguish "no such job" from "not available".
		j, gerr := o.Get(id)
		if gerr != nil {
			return gerr
		}
		if j == nil {
			return ErrNoJob
		}
		return fmt.Errorf("job is %s, not available; only an available job can be rescheduled", j.State)
	}
	return nil
}

// Delete hard-deletes the row (also erasing any retained credential); for pruning
// or a mistaken enqueue.
func (o *Ops) Delete(id int64) error {
	res, err := o.DB.Exec(`DELETE FROM jobs WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoJob
	}
	return nil
}

// mutated maps a guarded (state <> 'completed') UPDATE's result to ErrNoJob /
// ErrJobCompleted / nil.
func (o *Ops) mutated(id int64, res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	j, gerr := o.Get(id)
	if gerr != nil {
		return gerr
	}
	if j == nil {
		return ErrNoJob
	}
	return ErrJobCompleted
}
