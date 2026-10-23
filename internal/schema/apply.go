package schema

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// migrationLock is the advisory lock key every Apply transaction
// takes, so two runs against one database apply each migration once: the
// second waits, then finds it recorded.
const migrationLock = 0x63616c656e6473 // "calends"

// Apply runs the embedded migrations the database has not had
// yet, in order, and returns the ones it ran. Each runs in its own
// transaction together with its schema_migrations row, so a failure leaves
// every earlier migration applied and the failed one not at all.
//
// A migration file is sent without arguments, so pgx sends it as one
// simple-protocol query and multi-statement files work.
//
// A transaction holds every lock it takes until the end, and CREATE INDEX IF
// NOT EXISTS takes a SHARE lock on its table even when the index exists. A
// statement queued behind another session's lock would keep writes to the
// tables already visited blocked while it waits, so each lock wait is capped
// at lockTimeout and the run fails instead (zero means no cap). The wait for
// another Apply is bounded only by ctx.
func Apply(ctx context.Context, db *sql.DB, lockTimeout time.Duration) ([]Migration, error) {
	var applied []Migration
	for _, m := range Migrations() {
		ran, err := applyMigration(ctx, db, m, lockTimeout)
		if err != nil {
			return applied, fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
		}
		if ran {
			applied = append(applied, m)
		}
	}
	return applied, nil
}

func applyMigration(ctx context.Context, db *sql.DB, m Migration, lockTimeout time.Duration) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    integer PRIMARY KEY,
    name       text NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`); err != nil {
		return false, err
	}
	var done bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, m.Version).Scan(&done); err != nil {
		return false, err
	}
	if done {
		return false, nil
	}
	// SET LOCAL ends with the transaction, so the pooled connection keeps no trace.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", lockTimeout.Milliseconds())); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.Version, m.Name); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Version returns the highest migration recorded in the database, 0
// when the runner has never run there.
func Version(ctx context.Context, db *sql.DB) (int, error) {
	// Two queries: a statement naming a missing table fails at parse time,
	// whichever branch of a CASE it is in.
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		return 0, err
	}
	var v int
	err := db.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}
