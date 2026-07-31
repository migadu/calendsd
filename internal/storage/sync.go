// Sync/change tracking over the sabre calendarchanges table.

package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"calserver/internal/caldav"
	"calserver/internal/dav"
)

// ChangesForCalendar transcribes sabre's PDO getChangesForCalendar: the
// deduplicated change log since a token (last operation per uri wins), or
// the full object list for an initial sync.
func (b *CalDAV) ChangesForCalendar(id caldav.CalendarID, syncToken string, syncLevel, limit int) (*dav.ChangeSet, error) {
	defer b.Metrics.DBOpTimer("changes_for_calendar")()
	if syncToken == "" {
		var currentToken sql.NullInt64
		err := b.DB.QueryRow(`SELECT synctoken FROM calendars WHERE id = $1`, id.Calendar).Scan(&currentToken)
		// A missing calendar (no row / null token) means "no sync info";
		// any other error is a real failure that must surface, not be
		// masked as an empty change set.
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !currentToken.Valid {
			return nil, nil
		}
		out := &dav.ChangeSet{SyncToken: strconv.FormatInt(currentToken.Int64, 10)}
		rows, err := b.DB.Query(`SELECT uri FROM calendarobjects WHERE calendarid = $1`, id.Calendar)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var uri string
			if err := rows.Scan(&uri); err != nil {
				return nil, err
			}
			out.Added = append(out.Added, uri)
		}
		return out, rows.Err()
	}

	since, err := strconv.ParseInt(syncToken, 10, 64)
	if err != nil {
		return nil, nil // invalid token
	}
	query := `SELECT uri, operation, synctoken FROM calendarchanges
	           WHERE synctoken >= $1 AND calendarid = $2 ORDER BY synctoken`
	if limit > 0 {
		// One extra row detects truncation, like sabre.
		query += fmt.Sprintf(" LIMIT %d", limit+1)
	}
	rows, err := b.DB.Query(query, since, id.Calendar)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type change struct {
		operation int
		synctoken int64
	}
	// Last change per uri wins, in first-seen order (PHP assoc semantics).
	changes := map[string]change{}
	var order []string
	for rows.Next() {
		var uri string
		var c change
		if err := rows.Scan(&uri, &c.operation, &c.synctoken); err != nil {
			return nil, err
		}
		if _, seen := changes[uri]; !seen {
			order = append(order, uri)
		}
		changes[uri] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := &dav.ChangeSet{SyncToken: syncToken}
	var currentToken int64
	for count, uri := range order {
		if limit > 0 && count >= limit {
			out.Truncated = true
			break
		}
		c := changes[uri]
		// The client-facing token is one past the last synced change.
		if c.synctoken+1 > currentToken {
			currentToken = c.synctoken + 1
			out.SyncToken = strconv.FormatInt(currentToken, 10)
		}
		switch c.operation {
		case 1:
			out.Added = append(out.Added, uri)
		case 2:
			out.Modified = append(out.Modified, uri)
		case 3:
			out.Deleted = append(out.Deleted, uri)
		}
	}
	return out, nil
}

// addChange transcribes sabre's addChange (record at the current synctoken,
// then increment) but locks the calendars row first: two concurrent writers
// must not stamp two changes with the same token, or sync clients past that
// token skip one forever. Sabre's unwrapped statements race here; the lock
// serializes all Go-side writers and any sabre writer that reaches its
// UPDATE while we hold the row.
func addChange(tx *sql.Tx, calendarID, objectURI string, operation int) error {
	var token int64
	if err := tx.QueryRow(
		`SELECT synctoken FROM calendars WHERE id = $1 FOR UPDATE`, calendarID).Scan(&token); err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO calendarchanges (uri, synctoken, calendarid, operation)
VALUES ($1, $2, $3, $4)`,
		objectURI, token, calendarID, operation); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE calendars SET synctoken = $1 WHERE id = $2`, token+1, calendarID)
	return err
}
