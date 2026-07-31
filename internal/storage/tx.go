// Transaction plumbing shared by every backend in this package.
package storage

import "database/sql"

// withTx runs fn inside a transaction: commit when fn returns nil, roll back
// when it errors. Every multi-statement write in this package goes through
// here so no path can forget the rollback.
func withTx(db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
