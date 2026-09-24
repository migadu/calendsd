// Bulk-load plumbing: Postgres COPY through pgx, inside an ordinary
// database/sql transaction.
package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// copyInto bulk-loads rows into table through Postgres's COPY protocol, on the
// connection conn is holding — so the load is part of whatever transaction is
// open on that connection and rolls back with it.
//
// The pool is database/sql over pgx's stdlib driver (see Open), which exposes
// no COPY of its own, so the only route to pgx.Conn.CopyFrom is Conn.Raw and a
// type assertion. That is safe here, and not obviously so, which is why this
// lives in one place with a test (TestCopyIntoInsideTx) pinning the two
// behaviours it rests on:
//
//   - Conn.Raw hands out the same driver connection a *sql.Tx opened from that
//     *sql.Conn is using, so pgx sends the COPY inside the open transaction
//     (pgx transactions are a BEGIN on the connection, not a separate object);
//   - the *sql.Tx stays usable afterwards.
//
// The caller must therefore pass the *sql.Conn its transaction came from, and
// must not use the connection concurrently — one goroutine per principal, which
// is how ContactImport is shaped.
func copyInto(ctx context.Context, conn *sql.Conn, table string, columns []string, rows [][]any) (int64, error) {
	var copied int64
	err := conn.Raw(func(driverConn any) error {
		pgxConn, ok := driverConn.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("storage: COPY needs the pgx stdlib driver, got %T", driverConn)
		}
		var err error
		copied, err = pgxConn.Conn().CopyFrom(
			ctx, pgx.Identifier{table}, columns, pgx.CopyFromRows(rows))
		return err
	})
	if err != nil {
		return copied, fmt.Errorf("COPY into %s: %w", table, err)
	}
	if copied != int64(len(rows)) {
		return copied, fmt.Errorf("COPY into %s: %d of %d rows", table, copied, len(rows))
	}
	return copied, nil
}
