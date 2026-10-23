package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// store is the jobs-table persistence layer. It owns every SQL statement the
// queue runs (like internal/push owns its own queries) over the shared
// database/sql pool. Each method is a single autocommit statement — the claim
// commits before the handler runs, so no row lock is ever held across an SMTP
// send (D9).
type store struct{ db *sql.DB }

// encCredential bundles the three at-rest credential columns so a nil means
// "relay job, no credential" without three parallel nullable params.
type encCredential struct {
	ciphertext []byte
	nonce      []byte
	keyID      int16
}

// enqueue inserts a job, or — when a pending job already exists for the same
// (uid, recipient, method) — supersedes it iff the incoming revision is newer
// by (SEQUENCE, DTSTAMP), else no-ops (D10). A missing SEQUENCE sorts as 0 and
// a missing DTSTAMP as the empty string, so a re-issued PUT with identical args
// is an older-or-equal no-op. Rows whose args lack those keys never collide
// (all-NULL index tuple), so a future non-iMIP worker just always inserts.
func (s *store) enqueue(ctx context.Context, queue, worker string, args json.RawMessage, maxAttempts int, scheduledAt time.Time, cred *encCredential) error {
	var ct, nonce []byte
	var keyID any
	if cred != nil {
		ct, nonce, keyID = cred.ciphertext, cred.nonce, cred.keyID
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO jobs (queue, worker, args, state, max_attempts, scheduled_at,
                  enc_credential, enc_nonce, key_id)
VALUES ($1, $2, $3, 'available', $4, $5, $6, $7, $8)
ON CONFLICT ((args->>'uid'), (args->>'recipient'), (args->>'method')) WHERE state = 'available'
DO UPDATE SET
    args           = EXCLUDED.args,
    enc_credential = EXCLUDED.enc_credential,
    enc_nonce      = EXCLUDED.enc_nonce,
    key_id         = EXCLUDED.key_id,
    max_attempts   = EXCLUDED.max_attempts,
    attempt        = 0,
    scheduled_at   = EXCLUDED.scheduled_at,
    -- Restart the credential TTL clock: the supersede wrote a fresh credential,
    -- so its purge window runs from now, not from the original insert.
    inserted_at    = now(),
    last_error     = NULL
WHERE (COALESCE((EXCLUDED.args->>'sequence')::bigint, 0), COALESCE(EXCLUDED.args->>'dtstamp', ''))
    > (COALESCE((jobs.args->>'sequence')::bigint, 0), COALESCE(jobs.args->>'dtstamp', ''))`,
		queue, worker, []byte(args), maxAttempts, scheduledAt, ct, nonce, keyID)
	return err
}

// enqueueLatest inserts a latest-wins job (Queue.EnqueueLatest), stamping the
// coalesce key and generation into args. A pending job for the same (worker,
// key) is replaced only by a strictly newer generation; the replacement resets
// retry accounting exactly as the iMIP supersede does. Arbitrated by
// jobs_latest_ix; these rows never carry the iMIP keys, so jobs_coalesce_ix is
// unaffected.
func (s *store) enqueueLatest(ctx context.Context, queue, worker, key string, generation int64, args json.RawMessage, maxAttempts int, scheduledAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO jobs (queue, worker, args, state, max_attempts, scheduled_at)
VALUES ($1, $2, $3::jsonb || jsonb_build_object('coalesce_key', $4::text, 'generation', $5::bigint),
        'available', $6, $7)
ON CONFLICT (worker, (args->>'coalesce_key')) WHERE state = 'available' AND args ? 'coalesce_key'
DO UPDATE SET
    queue        = EXCLUDED.queue,
    args         = EXCLUDED.args,
    max_attempts = EXCLUDED.max_attempts,
    attempt      = 0,
    scheduled_at = EXCLUDED.scheduled_at,
    inserted_at  = now(),
    last_error   = NULL
WHERE (EXCLUDED.args->>'generation')::bigint > (jobs.args->>'generation')::bigint`,
		queue, worker, []byte(args), key, generation, maxAttempts, scheduledAt)
	return err
}

// isUniqueViolation reports a Postgres unique_violation (23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// claimed is a row the claim loop handed to a worker: identity, payload, retry
// accounting, and the (still-encrypted) credential columns.
type claimed struct {
	id          int64
	queue       string
	worker      string
	args        json.RawMessage
	attempt     int
	maxAttempts int
	cred        *encCredential
}

// claim atomically flips the oldest due, available job in queue to 'executing'
// (incrementing attempt, stamping attempted_at) and returns it, or nil when
// none is due. FOR UPDATE SKIP LOCKED distributes across workers and nodes; the
// UPDATE commits immediately, so the row lock is held for microseconds (D9).
func (s *store) claim(ctx context.Context, queue string) (*claimed, error) {
	row := s.db.QueryRowContext(ctx, `
UPDATE jobs SET state = 'executing', attempt = attempt + 1, attempted_at = now()
WHERE id = (
    SELECT id FROM jobs
    WHERE queue = $1 AND state = 'available' AND scheduled_at <= now()
    ORDER BY scheduled_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING id, queue, worker, args, attempt, max_attempts, enc_credential, enc_nonce, key_id`,
		queue)
	var (
		c        claimed
		rawArgs  []byte
		ct, nnce []byte
		keyID    sql.NullInt16
	)
	if err := row.Scan(&c.id, &c.queue, &c.worker, &rawArgs, &c.attempt, &c.maxAttempts, &ct, &nnce, &keyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	c.args = rawArgs
	if ct != nil {
		c.cred = &encCredential{ciphertext: ct, nonce: nnce, keyID: keyID.Int16}
	}
	return &c, nil
}

// errStaleClaim reports a settle that found the row no longer executing the
// claimed attempt — reclaimed (and perhaps claimed again), cancelled or
// re-queued while the handler ran. Nothing was written.
var errStaleClaim = errors.New("jobs: claim is stale")

// settled maps a guarded settle UPDATE that matched no row to errStaleClaim.
func settled(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errStaleClaim
	}
	return nil
}

// complete marks a successful job terminal and NULLs its credential — a
// delivered send never needs it again (D12 lifecycle).
func (s *store) complete(ctx context.Context, c *claimed) error {
	return settled(s.db.ExecContext(ctx, `
UPDATE jobs SET state = 'completed', completed_at = now(), last_error = NULL,
    enc_credential = NULL, enc_nonce = NULL, key_id = NULL
WHERE id = $1 AND state = 'executing' AND attempt = $2`, c.id, c.attempt))
}

// reschedule returns a transiently-failed job to the queue, due after backoff,
// keeping its credential for the next attempt.
//
// A latest-wins job (args.coalesce_key) may meet a pending sibling that was
// enqueued while it executed; only one may be 'available' (jobs_latest_ix). The
// higher generation survives — a pending sibling wins a tie, being the fresher
// enqueue — and the other is discarded as superseded. A sibling inserted
// between the check and the update surfaces as a unique violation; the whole
// decision is simply retried.
func (s *store) reschedule(ctx context.Context, c *claimed, runAt time.Time, lastErr string) error {
	var err error
	for try := 0; try < 3; try++ {
		if err = s.rescheduleOnce(ctx, c, runAt, lastErr); !isUniqueViolation(err) {
			return err
		}
	}
	return err
}

func (s *store) rescheduleOnce(ctx context.Context, c *claimed, runAt time.Time, lastErr string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	id := c.id
	var worker string
	var key sql.NullString
	var gen sql.NullInt64
	err = tx.QueryRowContext(ctx, `
SELECT worker, args->>'coalesce_key', (args->>'generation')::bigint FROM jobs
 WHERE id = $1 AND state = 'executing' AND attempt = $2 FOR UPDATE`,
		id, c.attempt).Scan(&worker, &key, &gen)
	if errors.Is(err, sql.ErrNoRows) {
		return errStaleClaim
	}
	if err != nil {
		return err
	}
	if key.Valid {
		superseded, err := reconcileSibling(ctx, tx, worker, key.String, id, gen.Int64,
			"superseded by a newer generation awaiting retry")
		if err != nil {
			return err
		}
		if superseded {
			if _, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'discarded', completed_at = now(), last_error = $2
WHERE id = $1`, id, "superseded by a newer pending job; last error: "+lastErr); err != nil {
				return err
			}
			return tx.Commit()
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs SET state = 'available', scheduled_at = $2, last_error = $3
WHERE id = $1`, id, runAt, lastErr); err != nil {
		return err
	}
	return tx.Commit()
}

// reconcileSibling settles a latest-wins job (id, generation gen) that is about
// to become pending against its key's pending sibling, if any, since only one
// may be pending (jobs_latest_ix). A sibling of the same or a newer
// generation wins — the caller must then not requeue id — and an older one is
// discarded with siblingErr as its last_error. Run in tx with id locked.
func reconcileSibling(ctx context.Context, tx *sql.Tx, worker, key string, id, gen int64, siblingErr string) (superseded bool, err error) {
	var otherID, otherGen int64
	err = tx.QueryRowContext(ctx, `
SELECT id, (args->>'generation')::bigint FROM jobs
 WHERE worker = $1 AND args ? 'coalesce_key' AND args->>'coalesce_key' = $2
   AND state = 'available' AND id <> $3
   FOR UPDATE`, worker, key, id).Scan(&otherID, &otherGen)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	case otherGen >= gen:
		return true, nil
	}
	_, err = tx.ExecContext(ctx, `
UPDATE jobs SET state = 'discarded', completed_at = now(), last_error = $2
WHERE id = $1`, otherID, siblingErr)
	return false, err
}

// discard marks a job terminally failed but KEEPS enc_credential: a given-up
// per-user send can still be resent manually via calctl until the TTL sweep
// clears the credential (D5b/D12). completed_at doubles as the settled-at time
// for pruning.
func (s *store) discard(ctx context.Context, c *claimed, lastErr string) error {
	return settled(s.db.ExecContext(ctx, `
UPDATE jobs SET state = 'discarded', completed_at = now(), last_error = $3
WHERE id = $1 AND state = 'executing' AND attempt = $2`, c.id, c.attempt, lastErr))
}

// reclaimOrphans resets 'executing' rows whose claim is older than the cutoff —
// a worker that died mid-send — back to 'available', or to 'discarded' when
// they are out of attempts. Without this the durability goal half-fails for the
// jobs a crash was meant to save (D9). Returns the number reclaimed.
//
// Latest-wins jobs (args.coalesce_key) are first reduced so the bulk reset
// cannot put two pending rows on one key (jobs_latest_ix): of the orphans that
// would return to 'available' and any pending sibling, only the highest
// generation survives (a pending sibling wins a tie); the rest are discarded as
// superseded.
func (s *store) reclaimOrphans(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// An orphan is outranked by a pending sibling of at least its generation,
	// or by another returning orphan of a higher (generation, id).
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs j
   SET state = 'discarded', completed_at = now(),
       last_error = 'reclaimed: superseded by a newer job for the same key'
 WHERE j.state = 'executing' AND j.attempted_at < $1 AND j.attempt < j.max_attempts
   AND j.args ? 'coalesce_key'
   AND EXISTS (
     SELECT 1 FROM jobs o
      WHERE o.id <> j.id AND o.worker = j.worker AND o.args ? 'coalesce_key'
        AND o.args->>'coalesce_key' = j.args->>'coalesce_key'
        AND (
          (o.state = 'available'
             AND (o.args->>'generation')::bigint >= (j.args->>'generation')::bigint)
          OR (o.state = 'executing' AND o.attempted_at < $1 AND o.attempt < o.max_attempts
             AND ((o.args->>'generation')::bigint, o.id) > ((j.args->>'generation')::bigint, j.id))
        ))`, cutoff); err != nil {
		return 0, err
	}
	// A pending sibling older than a surviving orphan yields to it.
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs o
   SET state = 'discarded', completed_at = now(),
       last_error = 'superseded by a newer generation reclaimed for retry'
 WHERE o.state = 'available' AND o.args ? 'coalesce_key'
   AND EXISTS (
     SELECT 1 FROM jobs j
      WHERE j.state = 'executing' AND j.attempted_at < $1 AND j.attempt < j.max_attempts
        AND j.worker = o.worker AND j.args ? 'coalesce_key'
        AND j.args->>'coalesce_key' = o.args->>'coalesce_key'
        AND (j.args->>'generation')::bigint > (o.args->>'generation')::bigint)`, cutoff); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
UPDATE jobs
   SET state        = CASE WHEN attempt >= max_attempts THEN 'discarded' ELSE 'available' END,
       -- A reclaimed job is due now (retry promptly); a fresh scheduled_at also
       -- keeps its credential from being immediately purge-eligible.
       scheduled_at = CASE WHEN attempt >= max_attempts THEN scheduled_at ELSE now() END,
       last_error   = 'reclaimed: worker vanished mid-send',
       completed_at = CASE WHEN attempt >= max_attempts THEN now() ELSE completed_at END
 WHERE state = 'executing' AND attempted_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// purgeCredentials clears a stored credential once the job no longer needs it —
// a backstop that must never fire on a job still working through its retry
// schedule. Because each transient failure pushes scheduled_at forward by the
// (increasing) backoff, a live job's credential lifetime grows with its schedule
// instead of expiring at a fixed age from insert (D12). A credential is dropped
// only when:
//   - the send GAVE UP (discarded) more than `grace` ago — the manual-resend
//     window closes (D5b); or
//   - an 'available' job is overdue by more than `grace` (scheduled_at long
//     past, never claimed — a stuck/abandoned row, not the normal cadence where
//     scheduled_at sits at or ahead of now).
//
// 'executing' rows are left to reclaimOrphans (which keys off attempted_at and
// resets scheduled_at to now on the reclaim, so a reclaimed job is never
// immediately purge-eligible). Touches only the secret columns, leaving the row
// for ops visibility.
//
// The two counts are reported separately: a purge of an 'available' row means
// a per-user send will now be discarded by its handler instead of delivered
// (audit 3.5), i.e. the queue is not keeping up, which an operator must see.
func (s *store) purgeCredentials(ctx context.Context, grace time.Time) (discarded, available int64, err error) {
	rows, err := s.db.QueryContext(ctx, `
UPDATE jobs SET enc_credential = NULL, enc_nonce = NULL, key_id = NULL
WHERE enc_credential IS NOT NULL AND (
    (state = 'discarded' AND completed_at < $1) OR
    (state = 'available' AND scheduled_at < $1)
)
RETURNING state`, grace)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			return discarded, available, err
		}
		if state == "available" {
			available++
		} else {
			discarded++
		}
	}
	return discarded, available, rows.Err()
}

// pruneTerminal deletes settled rows past the retention window (multi-KB bodies
// live in args, so this is not optional — step 7). Credentials on discarded rows
// are already gone via purgeCredentials by the time retention elapses.
func (s *store) pruneTerminal(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
DELETE FROM jobs WHERE state IN ('completed', 'discarded') AND completed_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
