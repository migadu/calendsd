// Sync/change tracking over the sabre calendarchanges and addressbookchanges
// tables. Calendars and address books keep the same shape of change log, so
// one implementation serves both.

package storage

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/migadu/calendsd/internal/caldav"
	"github.com/migadu/calendsd/internal/dav"
)

// changeLog names the tables behind one kind of synced collection.
type changeLog struct {
	collections string // the table holding synctoken, keyed by id
	changes     string // the change log
	members     string // the member objects, listed by an initial sync
	idColumn    string // the collection id column in changes and members
	notify      string // pg_notify payload prefix for WebDAV-Push
}

var (
	calendarLog    = changeLog{"calendars", "calendarchanges", "calendarobjects", "calendarid", "calendar:"}
	addressBookLog = changeLog{"addressbooks", "addressbookchanges", "cards", "addressbookid", "addressbook:"}
)

// ChangesForCalendar transcribes sabre's PDO getChangesForCalendar: the
// deduplicated change log since a token (last operation per uri wins), or
// the full object list for an initial sync.
func (b *CalDAV) ChangesForCalendar(ctx context.Context, id caldav.CalendarID, syncToken string, syncLevel, limit int) (*dav.ChangeSet, error) {
	defer b.Metrics.DBOpTimer("changes_for_calendar")()
	return changesSince(ctx, b.DB, calendarLog, id.Calendar, syncToken, limit)
}

// ChangesForAddressBook is ChangesForCalendar for an address book.
func (b *CardDAV) ChangesForAddressBook(ctx context.Context, addressBookID, syncToken string, syncLevel, limit int) (*dav.ChangeSet, error) {
	defer b.Metrics.DBOpTimer("changes_for_addressbook")()
	return changesSince(ctx, b.DB, addressBookLog, addressBookID, syncToken, limit)
}

// changesSince answers a sync REPORT for one collection. An empty token is an
// initial sync (every member as added, at the current token); nil, nil means
// an unknown collection, an unparseable token, or one below the pruned floor.
func changesSince(ctx context.Context, db *sql.DB, log changeLog, collectionID, syncToken string, limit int) (*dav.ChangeSet, error) {
	if syncToken == "" {
		var currentToken int64
		err := db.QueryRowContext(ctx, `SELECT synctoken FROM `+log.collections+` WHERE id = $1`, collectionID).Scan(&currentToken)
		// A missing collection means "no sync info"; any other error is a
		// real failure that must surface, not be masked as an empty change
		// set.
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		out := &dav.ChangeSet{SyncToken: strconv.FormatInt(currentToken, 10)}
		rows, err := db.QueryContext(ctx, `SELECT uri FROM `+log.members+` WHERE `+log.idColumn+` = $1`, collectionID)
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
	query := `SELECT uri, operation, synctoken FROM ` + log.changes + `
	           WHERE synctoken >= $1 AND ` + log.idColumn + ` = $2 ORDER BY synctoken`
	if limit > 0 {
		// One extra row detects truncation, like sabre.
		query += " LIMIT " + strconv.Itoa(limit+1)
	}
	rows, err := db.QueryContext(ctx, query, since, collectionID)
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
	// Rows below the floor may have been pruned, so a token below it cannot
	// be answered with a delta. Read after the rows: a prune deletes and
	// raises the floor in one transaction, so one committing between the two
	// reads shows here and the possibly incomplete rows are not used.
	var floor int64
	err = db.QueryRowContext(ctx, `SELECT synctoken_floor FROM `+log.collections+` WHERE id = $1`, collectionID).Scan(&floor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if since < floor {
		return nil, nil // pruned: the REPORT answers valid-sync-token
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

// recordChange transcribes sabre's addChange (record at the current
// synctoken, then increment) as one UPDATE … RETURNING, which takes the
// collection's row lock for the rest of the transaction: two concurrent
// writers must not stamp two changes with the same token, or sync clients
// past that token skip one forever. Sabre's unwrapped statements race here;
// the lock serializes all Go-side writers and any sabre writer that reaches
// its UPDATE while we hold the row.
func recordChange(ctx context.Context, tx *sql.Tx, log changeLog, collectionID, uri string, operation int) error {
	var token int64
	if err := tx.QueryRowContext(ctx, `UPDATE `+log.collections+` SET synctoken = synctoken + 1
 WHERE id = $1 RETURNING synctoken - 1`, collectionID).Scan(&token); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+log.changes+` (uri, synctoken, `+log.idColumn+`, operation)
VALUES ($1, $2, $3, $4)`, uri, token, collectionID, operation); err != nil {
		return err
	}
	// Notify the WebDAV-Push dispatcher (internal/push) that this collection
	// changed. pg_notify delivers on COMMIT only, so a rolled-back write emits
	// nothing; with no LISTENer (push disabled) it is a cheap no-op. The
	// payload is the collection key the worker maps to subscriptions/topic.
	_, err := tx.ExecContext(ctx, `SELECT pg_notify('dav_push', $1)`, log.notify+collectionID)
	return err
}
