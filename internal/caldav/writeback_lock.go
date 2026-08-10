package caldav

import (
	"context"
	"database/sql"
	"hash/fnv"
	"log/slog"
)

// AdvisoryLocker serializes the per-object SCHEDULE-STATUS write-back across
// processes, so durable iMIP workers on different calserver nodes patching the
// same calendar object don't clobber each other. It layers a Postgres session
// advisory lock (cluster-wide) on top of the in-process striped mutexes
// (same-node): the advisory lock is the mutex, held only for the brief
// read-modify-write, while the actual read/write still go through the DAV tree
// on pooled connections. A single-node deployment can leave it unset (the
// in-process locks alone suffice).
//
// It is intentionally best-effort: if the advisory lock can't be taken (a DB
// blip), the write-back proceeds under the in-process lock only — a stale status
// is a cosmetic loss, and the send itself already happened.
type AdvisoryLocker struct {
	DB    *sql.DB
	local objectLocks
	Log   *slog.Logger
}

func (a *AdvisoryLocker) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// Lock takes the same-node stripe, then the cross-node advisory lock, and
// returns an unlock that releases both.
func (a *AdvisoryLocker) Lock(path string) func() {
	localUnlock := a.local.Lock(path)
	if a.DB == nil {
		return localUnlock
	}
	ctx := context.Background()
	conn, err := a.DB.Conn(ctx)
	if err != nil {
		a.logger().Warn("imip write-back: advisory-lock connection unavailable; single-node lock only", "err", err)
		return localUnlock
	}
	key := advisoryKey(path)
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		conn.Close()
		a.logger().Warn("imip write-back: pg_advisory_lock failed; single-node lock only", "err", err)
		return localUnlock
	}
	return func() {
		// Release the advisory lock before returning the connection to the pool,
		// or the session-level lock would linger on a reused connection.
		if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
			a.logger().Warn("imip write-back: pg_advisory_unlock failed", "err", err)
		}
		conn.Close()
		localUnlock()
	}
}

// advisoryKey maps an object path to a stable 64-bit advisory-lock key. The
// bigint advisory-lock space is separate from the (int,int) space, so this
// namespace is calserver-private as long as no other code uses the bigint form.
func advisoryKey(path string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("caldav-writeback:"))
	_, _ = h.Write([]byte(path))
	return int64(h.Sum64())
}
