// AdminWrites is the Phase C carve-out (ADMIN-PLAN.md §1, §3.8): the only
// raw mutation SQL in the admin app, for rows no backend can write —
// users/principals (no backend write path exists, no sync state) and
// orphaned rows (their parent collection is gone, so no backend can reach
// them). Everything else the admin panel writes goes through the regular
// backend interfaces.
package storage

import (
	"database/sql"
	"fmt"
	"strings"
)

// AdminWrites holds the §1 carve-out mutations. Keep this type small and
// this file the only place the admin app mutates tables directly.
type AdminWrites struct {
	DB *sql.DB
}

// CreateUser inserts the users row and the principal triple (the principal
// plus its calendar-proxy-read/-write sub-principals, matching sabre's
// seed convention so delegation works). digestA1 is the precomputed
// md5(user:realm:pass); the caller owns the realm.
func (w *AdminWrites) CreateUser(username, digestA1, email, displayName string) error {
	if username == "" || strings.ContainsAny(username, "/\n") {
		return fmt.Errorf("invalid username %q", username)
	}
	tx, err := w.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO users (username, digesta1) VALUES ($1, $2)`,
		username, digestA1); err != nil {
		return fmt.Errorf("creating users row (already exists?): %w", err)
	}
	principal := "principals/" + username
	if _, err := tx.Exec(`
INSERT INTO principals (uri, email, displayname) VALUES
	($1, nullif($2, ''), nullif($3, '')),
	($1 || '/calendar-proxy-read', NULL, NULL),
	($1 || '/calendar-proxy-write', NULL, NULL)`,
		principal, email, displayName); err != nil {
		return fmt.Errorf("creating principal rows (already exist?): %w", err)
	}
	return tx.Commit()
}

// SetPassword replaces a user's digesta1 hash.
func (w *AdminWrites) SetPassword(username, digestA1 string) error {
	res, err := w.DB.Exec(
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
}

// DeletableOrphanChecks lists the doctor checks DeleteOrphans accepts.
func DeletableOrphanChecks() []string {
	return []string{"orphaned-instance", "orphaned-object", "orphaned-card",
		"dangling-groupmember", "orphaned-scheduling-object", "orphaned-property"}
}

// DeleteOrphans removes the rows a doctor check flagged, returning how many
// went. The deletes mirror the doctor's queries exactly, so a doctor run
// right after reports the check clean.
func (w *AdminWrites) DeleteOrphans(check string) (int64, error) {
	if check == "orphaned-property" {
		// Classified in Go (same walk the doctor uses), deleted by id.
		orphans, err := (&AdminQueries{DB: w.DB}).orphanedPropertyRows()
		if err != nil {
			return 0, err
		}
		var total int64
		for _, o := range orphans {
			res, err := w.DB.Exec(`DELETE FROM propertystorage WHERE id = $1`, o.id)
			if err != nil {
				return total, err
			}
			n, _ := res.RowsAffected()
			total += n
		}
		return total, nil
	}
	query, ok := deletableOrphanSQL[check]
	if !ok {
		return 0, fmt.Errorf("check %q is not deletable", check)
	}
	res, err := w.DB.Exec(query)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
