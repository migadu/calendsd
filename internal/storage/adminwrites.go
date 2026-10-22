// AdminWrites is the Phase C carve-out (ADMIN-PLAN.md §1, §3.8): the only
// raw mutation SQL in the admin app, for rows no backend can write —
// users/principals (no backend write path exists, no sync state) and
// orphaned rows (their parent collection is gone, so no backend can reach
// them). Everything else the admin panel writes goes through the regular
// backend interfaces.
package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoPrincipal is returned (wrapped) when a delete targets a principal that
// no longer exists. The retention purge treats it as "already gone → done"
// via errors.Is, rather than string-matching the message.
var ErrNoPrincipal = errors.New("no principal")

// AdminWrites holds the §1 carve-out mutations. Keep this type small and
// this file the only place the admin app mutates tables directly.
type AdminWrites struct {
	DB *sql.DB
}

// CreateUser inserts the users row and the principal triple (the principal
// plus its calendar-proxy-read/-write sub-principals, matching sabre's
// seed convention so delegation works). digestA1 is the precomputed
// md5(user:realm:pass); the caller owns the realm.
func (w *AdminWrites) CreateUser(ctx context.Context, username, digestA1, email, displayName string) error {
	if username == "" || strings.ContainsAny(username, "/\n") {
		return fmt.Errorf("invalid username %q", username)
	}
	return withTx(ctx, w.DB, func(tx *sql.Tx) error {
		return createUserRows(ctx, tx, username, digestA1, email, displayName)
	})
}

// createUserRows writes the users row and the principal triple inside a caller-
// owned transaction. Factored out of CreateUser because the contacts import
// creates a principal as one step of a larger per-principal transaction
// (ContactImport.ImportPrincipal) and must not open a nested one — and because
// two hand-written copies of this INSERT pair would eventually disagree about
// the proxy sub-principals, which fails silently as broken delegation.
//
// It inserts blind: callers that may meet an existing principal check first.
func createUserRows(ctx context.Context, tx *sql.Tx, username, digestA1, email, displayName string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, digesta1) VALUES ($1, $2)`,
		username, digestA1); err != nil {
		return fmt.Errorf("creating users row (already exists?): %w", err)
	}
	principal := "principals/" + username
	if _, err := tx.ExecContext(ctx, `
INSERT INTO principals (uri, email, displayname) VALUES
	($1, nullif($2, ''), nullif($3, '')),
	($1 || '/calendar-proxy-read', NULL, NULL),
	($1 || '/calendar-proxy-write', NULL, NULL)`,
		principal, email, displayName); err != nil {
		return fmt.Errorf("creating principal rows (already exist?): %w", err)
	}
	return nil
}

// BlockedDigestA1 returns a random 32-hex string shaped like a digesta1 hash
// but corresponding to no password, for accounts whose password lives entirely
// in an external identity source (rcptd). users.digesta1 is NOT NULL and unused
// on that path, so storing an unguessable value means Digest auth simply never
// matches — where a real md5(user:realm:pass) would be a latent credential if
// rcptd were ever switched off.
func BlockedDigestA1() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("storage: reading CSPRNG for blocked digesta1: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// UpdatePrincipal updates a principal's email and/or displayname. A nil
// field is left untouched; an empty string clears the column to NULL,
// matching CreateUser's nullif convention. Same carve-out rationale as
// CreateUser: the principals columns carry no sync state and no backend
// exposes a write path for them.
func (w *AdminWrites) UpdatePrincipal(ctx context.Context, uri string, email, displayName *string) error {
	var sets []string
	args := []any{uri}
	if email != nil {
		args = append(args, *email)
		sets = append(sets, fmt.Sprintf("email = nullif($%d, '')", len(args)))
	}
	if displayName != nil {
		args = append(args, *displayName)
		sets = append(sets, fmt.Sprintf("displayname = nullif($%d, '')", len(args)))
	}
	if len(sets) == 0 {
		return fmt.Errorf("nothing to update")
	}
	res, err := w.DB.ExecContext(ctx,
		`UPDATE principals SET `+strings.Join(sets, ", ")+` WHERE uri = $1`, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no principal %q", uri)
	}
	return nil
}

// DeleteUser is the reverse of CreateUser: it removes the users row (absent
// for group principals, which is fine) and the principal triple. The
// principal must hold no calendar instances or address books — owned ones
// have sync state and shared-in ones are invites, so both must be deleted
// or unshared through DAV first; this deletes only rows no backend can
// write. Group-membership rows and scheduling inbox objects referencing the
// principal go with it — exactly the rows the doctor would otherwise flag
// as dangling/orphaned.
func (w *AdminWrites) DeleteUser(ctx context.Context, username string) error {
	if username == "" || strings.ContainsAny(username, "/\n") {
		return fmt.Errorf("invalid username %q", username)
	}
	principal := "principals/" + username
	return withTx(ctx, w.DB, func(tx *sql.Tx) error {
		var calendars, books int
		if err := tx.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM calendarinstances WHERE principaluri = $1),
			(SELECT count(*) FROM addressbooks WHERE principaluri = $1)`,
			principal).Scan(&calendars, &books); err != nil {
			return err
		}
		if calendars > 0 || books > 0 {
			return fmt.Errorf("%s still holds %d calendar instance(s) and %d address book(s); delete or unshare them through DAV first, or use DeleteUserCascade",
				principal, calendars, books)
		}
		return deletePrincipalRows(ctx, tx, principal, username)
	})
}

// DeleteUserCascade is DeleteUser without the guard: it first removes every
// collection the principal owns — calendars via deleteCalendarRows (the same
// cascade CalDAV.DeleteCalendar's shared-owner branch uses, which also drops
// any sharees' instance rows) and address books via deleteAddressBookRows
// (shared with CardDAV.DeleteAddressBook) — plus any calendars shared *into*
// this principal (its own instance rows), then deletes the principal triple
// and users row. All in one transaction, so a failure leaves the account and
// its data intact. Property-storage rows are left for the doctor's
// orphaned-property sweep, same as DeleteUser.
func (w *AdminWrites) DeleteUserCascade(ctx context.Context, username string) error {
	if username == "" || strings.ContainsAny(username, "/\n") {
		return fmt.Errorf("invalid username %q", username)
	}
	return withTx(ctx, w.DB, func(tx *sql.Tx) error {
		return deleteUserCascadeRows(ctx, tx, username)
	})
}

// deleteUserCascadeRows is DeleteUserCascade inside a caller-owned
// transaction, so PurgeExpired can claim the tombstone and cascade in one.
func deleteUserCascadeRows(ctx context.Context, tx *sql.Tx, username string) error {
	principal := "principals/" + username
	// Owned calendars (access=1): capture ids first, since the deletes below
	// remove the very instance rows that mark ownership.
	ownedCals, err := scanIDs(ctx, tx,
		`SELECT calendarid FROM calendarinstances WHERE principaluri = $1 AND access = 1`, principal)
	if err != nil {
		return err
	}
	for _, id := range ownedCals {
		if err := deleteCalendarRows(ctx, tx, id); err != nil {
			return err
		}
	}
	// Any remaining instance rows are shares into this principal; drop them.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM calendarinstances WHERE principaluri = $1`, principal); err != nil {
		return err
	}

	ownedBooks, err := scanIDs(ctx, tx,
		`SELECT id FROM addressbooks WHERE principaluri = $1`, principal)
	if err != nil {
		return err
	}
	for _, id := range ownedBooks {
		if err := deleteAddressBookRows(ctx, tx, id); err != nil {
			return err
		}
	}

	// Webmail's address suggestions. Not reachable through any collection
	// above and not protected by a foreign key, so the purge has to name it:
	// the rows hold correspondents' names and addresses, which must not
	// outlive the deleted account. The contact_import ledger is deliberately
	// NOT purged here — it is evidence about a migration run and is meant to
	// survive the principal (see scripts/schema/pgsql.calserver.sql).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM contact_frecency WHERE principal = $1`, principal); err != nil {
		return err
	}

	return deletePrincipalRows(ctx, tx, principal, username)
}

// deletePrincipalRows removes the rows shared by both delete paths: the
// principal's group-membership, scheduling-inbox, working-hours
// (calendar_availability), share-invite notification and free-busy
// visibility rows, its principal
// triple (the principal plus the two proxy sub-principals), and its users
// row (absent for group principals, which is fine). It errors if no
// principal row matched, so deleting an unknown account is reported.
func deletePrincipalRows(ctx context.Context, tx *sql.Tx, principal, username string) error {
	proxyRead := principal + "/calendar-proxy-read"
	proxyWrite := principal + "/calendar-proxy-write"
	if _, err := tx.ExecContext(ctx, `
DELETE FROM groupmembers
 WHERE principal_id IN (SELECT id FROM principals WHERE uri IN ($1, $2, $3))
    OR member_id    IN (SELECT id FROM principals WHERE uri IN ($1, $2, $3))`,
		principal, proxyRead, proxyWrite); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM schedulingobjects WHERE principaluri = $1`, principal); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM calendar_availability WHERE principal = $1`, principal); err != nil {
		return err
	}
	// Their share-invite inbox: no foreign key, and a later account with the
	// same name must not inherit the invites.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM notifications WHERE principaluri = $1`, principal); err != nil {
		return err
	}
	// Their own free-busy setting, and their place on everyone else's
	// allowed list (a later account with the same name must not inherit
	// those grants).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM free_busy_visibility WHERE principal = $1`, principal); err != nil {
		return err
	}
	// Grants name the principal by URI or by its address.
	if _, err := tx.ExecContext(ctx, `
UPDATE free_busy_visibility v
   SET allowed = array_remove(array_remove(v.allowed, $1::text), a.addr), updated_at = now()
  FROM (SELECT coalesce('mailto:' || lower((SELECT email FROM principals WHERE uri = $1)), '') AS addr) a
 WHERE $1::text = ANY (v.allowed) OR a.addr = ANY (v.allowed)`, principal); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM principals WHERE uri IN ($1, $2, $3)`, principal, proxyRead, proxyWrite)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no principal %q: %w", principal, ErrNoPrincipal)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE username = $1`, username); err != nil {
		return err
	}
	return nil
}

// DeletedPrincipal is one pending-deletion tombstone.
type DeletedPrincipal struct {
	Principal  string
	Username   string
	Reason     string
	DeletedAt  time.Time
	PurgeAfter time.Time
}

// ScheduleDelete soft-deletes an account: it verifies the principal exists,
// then records a deleted_principals tombstone with purge_after = now() +
// retainDays days. Re-scheduling an already-tombstoned account updates its
// window and reason (idempotent). It touches no calendar/card/principal data —
// the auth gate disables login immediately, and the purge job removes the data
// once the window elapses. retainDays must be positive.
func (w *AdminWrites) ScheduleDelete(ctx context.Context, username string, retainDays int, reason string) error {
	if username == "" || strings.ContainsAny(username, "/\n") {
		return fmt.Errorf("invalid username %q", username)
	}
	if retainDays <= 0 {
		return fmt.Errorf("retention days must be positive, got %d", retainDays)
	}
	principal := "principals/" + username
	var exists bool
	if err := w.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM principals WHERE uri = $1)`, principal).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("no principal %q: %w", principal, ErrNoPrincipal)
	}
	// purge_after is materialized now so a later config change never moves it.
	_, err := w.DB.ExecContext(ctx, `
INSERT INTO deleted_principals (principal, username, purge_after, reason)
VALUES ($1, $2, now() + make_interval(days => $3), nullif($4, ''))
ON CONFLICT (principal) DO UPDATE
   SET purge_after = excluded.purge_after,
       reason      = excluded.reason,
       deleted_at  = now()`,
		principal, username, retainDays, reason)
	return err
}

// CancelDelete removes a pending-deletion tombstone (restore). It errors if the
// account was not scheduled for deletion.
func (w *AdminWrites) CancelDelete(ctx context.Context, username string) error {
	if username == "" || strings.ContainsAny(username, "/\n") {
		return fmt.Errorf("invalid username %q", username)
	}
	res, err := w.DB.ExecContext(ctx,
		`DELETE FROM deleted_principals WHERE principal = $1`, "principals/"+username)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%q is not scheduled for deletion", username)
	}
	return nil
}

// Deletions lists all pending-deletion tombstones, soonest purge first.
func (w *AdminWrites) Deletions(ctx context.Context) ([]DeletedPrincipal, error) {
	return scanDeletions(w.DB.QueryContext(ctx,
		`SELECT principal, username, coalesce(reason, ''), deleted_at, purge_after
		   FROM deleted_principals ORDER BY purge_after`))
}

// ExpiredDeletions lists tombstones whose retention window has elapsed
// (purge_after <= now()), soonest first — what a purge sweep would act on.
func (w *AdminWrites) ExpiredDeletions(ctx context.Context) ([]DeletedPrincipal, error) {
	return scanDeletions(w.DB.QueryContext(ctx,
		`SELECT principal, username, coalesce(reason, ''), deleted_at, purge_after
		   FROM deleted_principals WHERE purge_after <= now() ORDER BY purge_after`))
}

// PurgeExpired runs the retention sweep: it permanently removes every account
// whose window has elapsed and returns what it purged. For each expired
// tombstone it CLAIMS the row first (delete … WHERE purge_after <= now()) and
// only cascades if the claim won — so a concurrent CancelDelete (restore) that
// removed the tombstone means the account is left intact. The claim and the
// cascade share one transaction: a cascade that fails rolls the claim back
// too, so the account keeps its tombstone (login stays disabled) and the next
// sweep retries it. A cascade that finds the principal already gone
// (ErrNoPrincipal) still counts as purged, and commits the claim.
func (w *AdminWrites) PurgeExpired(ctx context.Context) ([]DeletedPrincipal, error) {
	expired, err := w.ExpiredDeletions(ctx)
	if err != nil {
		return nil, err
	}
	var purged []DeletedPrincipal
	for _, d := range expired {
		claimed := false
		err := withTx(ctx, w.DB, func(tx *sql.Tx) error {
			// Claim: decide ownership by the delete, not the earlier read. If a
			// restore already removed the tombstone, RowsAffected is 0 and we
			// skip. The delete also locks the tombstone against a concurrent
			// restore until the cascade commits.
			res, err := tx.ExecContext(ctx,
				`DELETE FROM deleted_principals WHERE principal = $1 AND purge_after <= now()`,
				d.Principal)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n == 0 {
				return nil // lost the race to a restore; leave the account intact
			}
			claimed = true
			if err := deleteUserCascadeRows(ctx, tx, d.Username); err != nil && !errors.Is(err, ErrNoPrincipal) {
				return err
			}
			return nil
		})
		if err != nil {
			return purged, fmt.Errorf("purging %s: %w", d.Username, err)
		}
		if claimed {
			purged = append(purged, d)
		}
	}
	return purged, nil
}

func scanDeletions(rows *sql.Rows, queryErr error) ([]DeletedPrincipal, error) {
	if queryErr != nil {
		return nil, queryErr
	}
	defer rows.Close()
	var out []DeletedPrincipal
	for rows.Next() {
		var d DeletedPrincipal
		if err := rows.Scan(&d.Principal, &d.Username, &d.Reason, &d.DeletedAt, &d.PurgeAfter); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// scanIDs runs a single-column query and collects the string ids, used by
// DeleteUserCascade to capture owned collection ids before deleting the rows
// that identify them.
func scanIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetPassword replaces a user's digesta1 hash.
func (w *AdminWrites) SetPassword(ctx context.Context, username, digestA1 string) error {
	res, err := w.DB.ExecContext(ctx,
		`UPDATE users SET digesta1 = $2 WHERE username = $1`, username, digestA1)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no users row for %q", username)
	}
	return nil
}

// deletableOrphanSQL maps doctor checks to the DELETE mirroring their
// anti-join. Only rows unreachable through any backend are deletable here.
var deletableOrphanSQL = map[string]string{
	"orphaned-instance": `DELETE FROM calendarinstances ci
	  WHERE NOT EXISTS (SELECT 1 FROM calendars c WHERE c.id = ci.calendarid)`,
	"orphaned-object": `DELETE FROM calendarobjects o
	  WHERE NOT EXISTS (SELECT 1 FROM calendars c WHERE c.id = o.calendarid)`,
	"orphaned-card": `DELETE FROM cards cd
	  WHERE NOT EXISTS (SELECT 1 FROM addressbooks ab WHERE ab.id = cd.addressbookid)`,
	"dangling-groupmember": `DELETE FROM groupmembers gm
	  WHERE NOT EXISTS (SELECT 1 FROM principals p WHERE p.id = gm.principal_id)
	     OR NOT EXISTS (SELECT 1 FROM principals p WHERE p.id = gm.member_id)`,
	"orphaned-scheduling-object": `DELETE FROM schedulingobjects s
	  WHERE s.principaluri IS NOT NULL
	    AND NOT EXISTS (SELECT 1 FROM principals p WHERE p.uri = s.principaluri)`,
	"orphaned-availability": `DELETE FROM calendar_availability ca
	  WHERE NOT EXISTS (SELECT 1 FROM principals p WHERE p.uri = ca.principal)`,
	"orphaned-visibility": `DELETE FROM free_busy_visibility v
	  WHERE NOT EXISTS (SELECT 1 FROM principals p WHERE p.uri = v.principal)`,
}

// DeletableOrphanChecks lists the doctor checks DeleteOrphans accepts.
func DeletableOrphanChecks() []string {
	return []string{"orphaned-instance", "orphaned-object", "orphaned-card",
		"dangling-groupmember", "orphaned-scheduling-object", "orphaned-availability",
		"orphaned-visibility", "orphaned-property"}
}

// DeleteOrphans removes the rows a doctor check flagged, returning how many
// went. The deletes mirror the doctor's queries exactly, so a doctor run
// right after reports the check clean.
func (w *AdminWrites) DeleteOrphans(ctx context.Context, check string) (int64, error) {
	if check == "orphaned-property" {
		// Classified in Go (same walk the doctor uses), deleted by id in one
		// statement, inside the transaction the classification read in, so
		// the delete is all or nothing.
		var total int64
		err := withTx(ctx, w.DB, func(tx *sql.Tx) error {
			orphans, err := orphanedPropertyRows(ctx, tx)
			if err != nil || len(orphans) == 0 {
				return err
			}
			ids := make([]int64, len(orphans))
			for i, o := range orphans {
				ids[i] = o.id
			}
			res, err := tx.ExecContext(ctx, `DELETE FROM propertystorage WHERE id = ANY($1::bigint[])`, ids)
			if err != nil {
				return err
			}
			total, err = res.RowsAffected()
			return err
		})
		if err != nil {
			return 0, err
		}
		return total, nil
	}
	query, ok := deletableOrphanSQL[check]
	if !ok {
		return 0, fmt.Errorf("check %q is not deletable", check)
	}
	res, err := w.DB.ExecContext(ctx, query)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
