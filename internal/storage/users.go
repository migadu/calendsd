// Auth backend over the shared users table.

package storage

import (
	"context"
	"database/sql"
	"errors"
)

// Users reads the users table for authentication: digesta1 =
// md5(user:realm:pass) with the realm baked in (docs/design/PLAN.md §5).
type Users struct {
	DB *sql.DB
}

// HashA1 returns the stored digesta1 hash for a user, or "" when there is no
// such user. A database failure is an error, not an unknown user, so callers
// can tell an outage from bad credentials.
func (u *Users) HashA1(ctx context.Context, realm, username string) (string, error) {
	var hash sql.NullString
	err := u.DB.QueryRowContext(ctx,
		`SELECT digesta1 FROM users WHERE username = $1`, username).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return hash.String, err
}

// IsDeleted reports whether a principal has a pending-deletion tombstone
// (soft-deleted, not yet purged). It is a single primary-key lookup, cheap
// enough to call on every authenticated request from the auth gate.
func (u *Users) IsDeleted(ctx context.Context, principal string) (bool, error) {
	var exists bool
	err := u.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM deleted_principals WHERE principal = $1)`,
		principal).Scan(&exists)
	return exists, err
}
