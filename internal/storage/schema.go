package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"calserver/scripts/schema"
)

// ApplySchema runs the embedded pgsql.calserver.sql against db. Without
// arguments pgx sends it as one simple-protocol query, so the file runs as a
// single implicit transaction: it applies completely or not at all. Every
// statement in it is conditional, so running it against a database that is
// already up to date changes nothing.
//
// One transaction also holds every lock it takes until the end, and CREATE
// INDEX IF NOT EXISTS takes a SHARE lock on its table even when the index
// exists. A statement queued behind another session's lock would keep writes
// to the tables already visited blocked while it waits, so each lock wait is
// capped at lockTimeout and the run fails instead (zero means no cap).
func ApplySchema(ctx context.Context, db *sql.DB, lockTimeout time.Duration) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Session level, so it is reset before the connection goes back to the pool.
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET lock_timeout = %d", lockTimeout.Milliseconds())); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), "RESET lock_timeout")
	_, err = conn.ExecContext(ctx, schema.Calserver)
	return err
}
