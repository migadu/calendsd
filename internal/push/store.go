package push

import (
	"database/sql"
	"errors"
	"time"
)

// DefaultMaxSubscriptionsPerPrincipal bounds how many live subscriptions one
// principal may hold across all collections. A client registers one per
// collection per device; fifty covers a power user with several devices, while
// an unbounded table is a trivial storage and dispatch-fan-out DoS for any
// authenticated user (audit 3.3).
const DefaultMaxSubscriptionsPerPrincipal = 50

// ErrTooManySubscriptions is returned by Upsert when a new subscription would
// exceed the principal's cap. The plugin maps it to 403.
var ErrTooManySubscriptions = errors.New("too many push subscriptions for this principal")

// ErrEndpointOwnedByOther is returned by Upsert when the (collection,
// endpoint) row belongs to a different principal: a co-reader who learned
// the endpoint URL must not be able to take over the subscription and its
// keys (audit 6.13). The plugin maps it to 409.
var ErrEndpointOwnedByOther = errors.New("this push endpoint is registered to another principal")

// Store is the push_subscriptions persistence layer. It talks to the same
// Postgres the DAV backends use, over the shared database/sql pool.
type Store struct {
	DB *sql.DB
	// MaxPerPrincipal caps live subscriptions per principal; zero means
	// DefaultMaxSubscriptionsPerPrincipal.
	MaxPerPrincipal int
}

func (s *Store) maxPerPrincipal() int {
	if s.MaxPerPrincipal > 0 {
		return s.MaxPerPrincipal
	}
	return DefaultMaxSubscriptionsPerPrincipal
}

// Upsert inserts a subscription or, if one already exists for the same
// (collection, endpoint), refreshes its keys/path/principal/expiry in place —
// the idempotent re-register the spec requires. On a fresh insert
// last_synctoken is seeded to seedToken (the collection's current synctoken)
// so the startup catch-up sweep does not fire a spurious push for a
// brand-new subscription; on conflict last_synctoken is left untouched.
// It returns the row's token (freshly minted on insert, preserved on update).
//
// The per-principal cap is enforced in the same transaction: live rows for the
// principal are counted, excluding the row this call would refresh, so a
// re-register never trips the cap while a genuinely new endpoint beyond it
// returns ErrTooManySubscriptions.
func (s *Store) Upsert(sub Subscription, seedToken int64, expires time.Time) (string, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// Serialise registrations per principal for the rest of the transaction,
	// so two concurrent POSTs at the cap cannot both count below it.
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext($1))`, "push:"+sub.PrincipalURI); err != nil {
		return "", err
	}
	var live int
	if err := tx.QueryRow(`
SELECT count(*) FROM push_subscriptions
 WHERE principal_uri = $1 AND expires > now()
   AND NOT (collection_kind = $2 AND collection_id = $3 AND push_resource = $4)`,
		sub.PrincipalURI, sub.Kind, sub.CollectionID, sub.PushResource).Scan(&live); err != nil {
		return "", err
	}
	if live >= s.maxPerPrincipal() {
		return "", ErrTooManySubscriptions
	}

	var token string
	err = tx.QueryRow(`
INSERT INTO push_subscriptions
  (token, collection_kind, collection_id, collection_path, principal_uri,
   push_resource, p256dh, auth_secret, last_synctoken, expires)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (collection_kind, collection_id, push_resource)
DO UPDATE SET
  collection_path = EXCLUDED.collection_path,
  p256dh          = EXCLUDED.p256dh,
  auth_secret     = EXCLUDED.auth_secret,
  expires         = EXCLUDED.expires
WHERE push_subscriptions.principal_uri = EXCLUDED.principal_uri
RETURNING token`,
		sub.Token, sub.Kind, sub.CollectionID, sub.CollectionPath, sub.PrincipalURI,
		sub.PushResource, sub.P256dh, sub.AuthSecret, seedToken, expires,
	).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		// The conflict row belongs to someone else: the WHERE skipped the
		// update, so nothing was returned (audit 6.13).
		return "", ErrEndpointOwnedByOther
	}
	if err != nil {
		return "", err
	}
	return token, tx.Commit()
}

// DeleteByToken removes the subscription resource, scoped to the principal so
// one user cannot unsubscribe another's endpoint. It returns whether a row was
// deleted (false → the caller answers 404).
func (s *Store) DeleteByToken(token, principal string) (bool, error) {
	res, err := s.DB.Exec(
		`DELETE FROM push_subscriptions WHERE token = $1 AND principal_uri = $2`,
		token, principal)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteByID removes a subscription by primary key (used to prune a dead
// endpoint the push service reported gone).
func (s *Store) DeleteByID(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM push_subscriptions WHERE id = $1`, id)
	return err
}

// SetLastToken records the highest synctoken delivered to a subscription, so a
// later notification for an already-delivered token is a no-op.
func (s *Store) SetLastToken(id, token int64) error {
	_, err := s.DB.Exec(
		`UPDATE push_subscriptions SET last_synctoken = $1 WHERE id = $2`, token, id)
	return err
}

// ByCollection lists the live (unexpired) subscriptions for one collection.
func (s *Store) ByCollection(kind, id string) ([]Subscription, error) {
	rows, err := s.DB.Query(`
SELECT id, token, collection_kind, collection_id, collection_path, principal_uri,
       push_resource, p256dh, auth_secret, last_synctoken
  FROM push_subscriptions
 WHERE collection_kind = $1 AND collection_id = $2 AND expires > now()`, kind, id)
	if err != nil {
		return nil, err
	}
	return scanSubs(rows)
}

// AllLive lists every unexpired subscription, for the startup catch-up sweep.
func (s *Store) AllLive() ([]Subscription, error) {
	rows, err := s.DB.Query(`
SELECT id, token, collection_kind, collection_id, collection_path, principal_uri,
       push_resource, p256dh, auth_secret, last_synctoken
  FROM push_subscriptions
 WHERE expires > now()`)
	if err != nil {
		return nil, err
	}
	return scanSubs(rows)
}

// CurrentToken returns the collection's current synctoken. ok is false when
// the collection no longer exists (deleted out from under a subscription).
func (s *Store) CurrentToken(kind, id string) (token int64, ok bool, err error) {
	table := "calendars"
	if kind == "addressbook" {
		table = "addressbooks"
	}
	var t sql.NullInt64
	// id is text but the column is integer; pgx sends it as an untyped
	// parameter Postgres infers as integer, exactly like storage.addChange's
	// "WHERE id = $1" with a string calendarID.
	err = s.DB.QueryRow(`SELECT synctoken FROM `+table+` WHERE id = $1`, id).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return t.Int64, t.Valid, nil
}

// PruneExpired deletes subscriptions past their expiry (worker housekeeping).
func (s *Store) PruneExpired() error {
	_, err := s.DB.Exec(`DELETE FROM push_subscriptions WHERE expires < now()`)
	return err
}

func scanSubs(rows *sql.Rows) ([]Subscription, error) {
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var s Subscription
		if err := rows.Scan(&s.ID, &s.Token, &s.Kind, &s.CollectionID, &s.CollectionPath,
			&s.PrincipalURI, &s.PushResource, &s.P256dh, &s.AuthSecret, &s.LastSyncToken); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
