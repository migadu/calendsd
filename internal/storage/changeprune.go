package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// pruneBatch bounds one prune transaction: it deletes at most this many sync
// tokens' worth of rows from one collection, so it never holds a long lock or
// builds a huge transaction on a collection with years of history.
const pruneBatch = 10000

// PruneChangeLogs deletes calendar and address-book change rows created
// before cutoff, and raises each pruned collection's synctoken_floor so a
// sync REPORT with a token below it answers valid-sync-token instead of an
// incomplete delta. It returns how many rows it removed.
//
// It prunes by sync token, not by row: a collection's rows below a token F
// go together, so rows that share a token are never split. F is the lowest
// token among the collection's rows that are newer than cutoff (or its
// current token when none are), so a row newer than cutoff is never removed,
// whatever order concurrent writers committed in.
func PruneChangeLogs(ctx context.Context, db *sql.DB, cutoff time.Time) (int64, error) {
	var total int64
	for _, log := range []changeLog{calendarLog, addressBookLog} {
		for {
			ids, err := pruneCandidates(ctx, db, log, cutoff)
			if err != nil {
				return total, err
			}
			var removed int64
			for _, id := range ids {
				n, err := pruneCollection(ctx, db, log, id, cutoff)
				if err != nil {
					return total, err
				}
				removed += n
			}
			total += removed
			// No candidates left, or none this server could prune: another
			// server holds their prune locks. Asking again would return the
			// same ones.
			if removed == 0 {
				break
			}
		}
	}
	return total, nil
}

// pruneCandidates lists collections whose oldest change row is older than
// cutoff: one probe of the (collection, synctoken) index per collection. A
// collection whose oldest row is at or above its current token is left out:
// no prune can remove that row, and listing it again on every pass could
// fill the LIMIT and starve the collections that can move.
func pruneCandidates(ctx context.Context, db *sql.DB, log changeLog, cutoff time.Time) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
SELECT c.id FROM `+log.collections+` c
 CROSS JOIN LATERAL (SELECT created, synctoken FROM `+log.changes+` ch
                      WHERE ch.`+log.idColumn+` = c.id ORDER BY synctoken LIMIT 1) oldest
 WHERE oldest.created < $1 AND oldest.synctoken < c.synctoken
 LIMIT 100`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// pruneCollection removes one batch of a collection's old rows and raises its
// floor, in one transaction. The floor update comes last, so the collection's
// row lock, which every writer to it needs, is held only until the commit;
// that is also the order a collection delete takes the two (change rows, then
// the collection row), so the two cannot deadlock.
//
// Another server pruning the same collection could deadlock with this one on
// the overlapping DELETEs, so a collection whose prune lock is taken is
// skipped: the other server is doing it.
func pruneCollection(ctx context.Context, db *sql.DB, log changeLog, id int64, cutoff time.Time) (int64, error) {
	var n int64
	err := withTx(ctx, db, func(tx *sql.Tx) error {
		var locked bool
		if err := tx.QueryRowContext(ctx,
			`SELECT pg_try_advisory_xact_lock(hashtext($1), $2)`, log.changes, id).Scan(&locked); err != nil || !locked {
			return err
		}
		var floor int64
		if err := tx.QueryRowContext(ctx, `
SELECT least(
         coalesce((SELECT min(synctoken) FROM `+log.changes+` WHERE `+log.idColumn+` = $1 AND created >= $2),
                  (SELECT synctoken FROM `+log.collections+` WHERE id = $1)),
         (SELECT min(synctoken) FROM `+log.changes+` WHERE `+log.idColumn+` = $1) + $3)`,
			id, cutoff, pruneBatch).Scan(&floor); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM `+log.changes+` WHERE `+log.idColumn+` = $1 AND synctoken < $2`, id, floor)
		if err != nil {
			return err
		}
		if n, err = res.RowsAffected(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE `+log.collections+`
   SET synctoken_floor = greatest(synctoken_floor, $2) WHERE id = $1`, id, floor)
		return err
	})
	return n, err
}

// RunChangeLogPruner prunes change rows older than retention once at start
// and then every interval, until ctx is done. Several servers sharing one
// database may all run it: a prune is idempotent and the floor only rises.
func RunChangeLogPruner(ctx context.Context, db *sql.DB, retention, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		start := time.Now()
		n, err := pruneChangeLogsRecovered(ctx, db, start.Add(-retention))
		switch {
		case err != nil && ctx.Err() == nil:
			slog.Warn("change-log prune failed", "err", err, "removed", n)
		case n > 0:
			slog.Info("pruned change log", "removed", n, "took", time.Since(start).Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pruneChangeLogsRecovered is PruneChangeLogs with a panic turned into an
// error, so one bad pass is logged and retried next interval rather than
// taking the server down from its background goroutine.
func pruneChangeLogsRecovered(ctx context.Context, db *sql.DB, cutoff time.Time) (n int64, err error) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("change-log prune panic", "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("change-log prune panic: %v", p)
		}
	}()
	return PruneChangeLogs(ctx, db, cutoff)
}
