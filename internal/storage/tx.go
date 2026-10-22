// Transaction plumbing shared by every backend in this package.
package storage

import (
	"context"
	"database/sql"

	"calendsd/internal/dav"
)

// ErrNotFound is returned when an update or delete matched no row — the
// object went away between the caller's lookup and the write. It is a
// *dav.Error, so the DAV layer answers 404.
var ErrNotFound error = dav.ErrNotFound("the resource no longer exists")

// affectedOne turns an Exec result into ErrNotFound when it matched no row,
// so a write against a vanished row records no change.
func affectedOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// withTx runs fn inside a transaction: commit when fn returns nil, roll back
// when it errors. Every multi-statement write in this package goes through
// here so no path can forget the rollback. The BEGIN, including any
// connection the pool has to dial for it, honours ctx, and database/sql rolls
// the transaction back when ctx ends, so the statements fn runs stop with it.
func withTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
