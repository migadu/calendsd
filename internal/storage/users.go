// Auth backend over the shared users table.

package storage

import (
	"database/sql"
)

// Users reads the shared users table for authentication. Port of
// DAV\Auth\Backend\PDO: digesta1 = md5(user:realm:pass) with the realm baked
// in (PLAN.md §5).
type Users struct {
	DB *sql.DB
}

// HashA1 returns the stored digesta1 hash for a user.
func (u *Users) HashA1(realm, username string) (string, bool) {
	var hash string
	err := u.DB.QueryRow(
		`SELECT digesta1 FROM users WHERE username = $1`, username).Scan(&hash)
	return hash, err == nil
}

// IsDeleted reports whether a principal has a pending-deletion tombstone
// (soft-deleted, not yet purged). It is a single primary-key lookup, cheap
// enough to call on every authenticated request from the auth gate.
func (u *Users) IsDeleted(principal string) (bool, error) {
	var exists bool
	err := u.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM deleted_principals WHERE principal = $1)`,
		principal).Scan(&exists)
	return exists, err
}
