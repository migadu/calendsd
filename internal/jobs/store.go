package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
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
WHERE (COALESCE((EXCLUDED.args->>'sequence')::int, 0), COALESCE(EXCLUDED.args->>'dtstamp', ''))
    > (COALESCE((jobs.args->>'sequence')::int, 0), COALESCE(jobs.args->>'dtstamp', ''))`,
		queue, worker, []byte(args), maxAttempts, scheduledAt, ct, nonce, keyID)
	return err
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

// complete marks a successful job terminal and NULLs its credential — a
// delivered send never needs it again (D12 lifecycle).
func (s *store) complete(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE jobs SET state = 'completed', completed_at = now(), last_error = NULL,
    enc_credential = NULL, enc_nonce = NULL, key_id = NULL
WHERE id = $1`, id)
	return err
}

// reschedule returns a transiently-failed job to the queue, due after backoff,
// keeping its credential for the next attempt.
func (s *store) reschedule(ctx context.Context, id int64, runAt time.Time, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE jobs SET state = 'available', scheduled_at = $2, last_error = $3
WHERE id = $1`, id, runAt, lastErr)
	return err
}

// discard marks a job terminally failed but KEEPS enc_credential: a given-up
// per-user send can still be resent manually via calctl until the TTL sweep
// clears the credential (D5b/D12). completed_at doubles as the settled-at time
// for pruning.
func (s *store) discard(ctx context.Context, id int64, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE jobs SET state = 'discarded', completed_at = now(), last_error = $2
WHERE id = $1`, id, lastErr)
	return err
}

// reclaimOrphans resets 'executing' rows whose claim is older than the cutoff —
// a worker that died mid-send — back to 'available', or to 'discarded' when
// they are out of attempts. Without this the durability goal half-fails for the
// jobs a crash was meant to save (D9). Returns the number reclaimed.
func (s *store) reclaimOrphans(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
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
	return res.RowsAffected()
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
func (s *store) purgeCredentials(ctx context.Context, grace time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE jobs SET enc_credential = NULL, enc_nonce = NULL, key_id = NULL
WHERE enc_credential IS NOT NULL AND (
    (state = 'discarded' AND completed_at < $1) OR
    (state = 'available' AND scheduled_at < $1)
)`, grace)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
