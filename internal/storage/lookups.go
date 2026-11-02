// Indexed lookups the CalDAV layer uses instead of listing every principal
// or loading every object of a calendar.

package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/caldav"
)

var (
	_ caldav.AddressResolver = (*Principals)(nil)
	_ acl.DomainLister       = (*Principals)(nil)
	_ acl.Pager              = (*Principals)(nil)
	_ caldav.UIDLookup       = (*CalDAV)(nil)
	_ caldav.MetaLister      = (*CalDAV)(nil)
	_ caldav.BatchFetcher    = (*CalDAV)(nil)
)

// PrincipalByAddress returns the principal directly under prefix whose email
// matches a mailto: calendar user address (case-insensitively), or "" when
// none does. Implements caldav.AddressResolver; served by the
// principals_lower_email index.
func (p *Principals) PrincipalByAddress(ctx context.Context, prefix, address string) (string, error) {
	if len(address) < len("mailto:") || !strings.EqualFold(address[:len("mailto:")], "mailto:") {
		return "", nil
	}
	email := address[len("mailto:"):]
	if email == "" {
		return "", nil
	}
	var uri string
	err := p.DB.QueryRowContext(ctx, `
SELECT uri FROM principals
 WHERE lower(email) = lower($1)
   AND uri LIKE $2 AND strpos(substr(uri, $3), '/') = 0
 ORDER BY id LIMIT 1`,
		email, escapeLike(prefix)+"/%", len(prefix)+2).Scan(&uri)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return uri, err
}

// CalendarObjectByUID returns the object carrying uid in the calendar, or nil
// when there is none. Implements caldav.UIDLookup; served by the
// calendarobjects_calendarid_uid index.
func (b *CalDAV) CalendarObjectByUID(ctx context.Context, id caldav.CalendarID, uid string) (*caldav.ObjectInfo, error) {
	defer b.Metrics.DBOpTimer("calendar_object_by_uid")()
	rows, err := b.DB.QueryContext(ctx,
		`SELECT `+objectColumns+` FROM calendarobjects WHERE calendarid = $1 AND uid = $2 ORDER BY id LIMIT 1`,
		id.Calendar, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanObject(rows, id)
}
