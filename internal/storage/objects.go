// Calendar object CRUD over the sabre calendarobjects table.

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"calendsd/internal/caldav"
	"calendsd/internal/caldav/ical"
)

// objectColumns surfaces a legacy row's NULL etag as the md5 of its data, the
// value a write would have stored, so no client ever sees an empty etag.
const objectColumns = `id, uri, lastmodified, coalesce(etag, md5(calendardata)), calendardata, size, componenttype`

// rowScanner is what *sql.Row and *sql.Rows have in common.
type rowScanner interface {
	Scan(dest ...any) error
}

func (b *CalDAV) CalendarObjects(ctx context.Context, id caldav.CalendarID) ([]caldav.ObjectInfo, error) {
	rows, err := b.DB.QueryContext(ctx,
		`SELECT `+objectColumns+` FROM calendarobjects WHERE calendarid = $1`, id.Calendar)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []caldav.ObjectInfo
	for rows.Next() {
		obj, err := scanObject(rows, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *obj)
	}
	return out, rows.Err()
}

// CalendarQuery prefilters candidate objects in SQL by component type and the
// denormalized firstoccurence/lastoccurence bounds, transcribing sabre's PDO
// calendarQuery. The occurrence filters are NULL-safe so objects without
// computed bounds are never wrongly excluded — the caller's validator refines.
func (b *CalDAV) CalendarQuery(ctx context.Context, id caldav.CalendarID, componentType string, start, end time.Time) ([]string, error) {
	defer b.Metrics.DBOpTimer("calendar_query")()
	q := `SELECT uri FROM calendarobjects WHERE calendarid = $1`
	args := []any{id.Calendar}
	if componentType != "" {
		args = append(args, componentType)
		q += fmt.Sprintf(" AND componenttype = $%d", len(args))
	}
	// The occurrence columns are int4 but a client's time-range can lie past
	// 2038; binding the bounds as bigint compares them instead of failing the
	// query with an int4 overflow.
	if !end.IsZero() {
		args = append(args, end.Unix())
		q += fmt.Sprintf(" AND (firstoccurence IS NULL OR firstoccurence < $%d::bigint)", len(args))
	}
	if !start.IsZero() {
		args = append(args, start.Unix())
		q += fmt.Sprintf(" AND (lastoccurence IS NULL OR lastoccurence > $%d::bigint)", len(args))
	}
	rows, err := b.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var uri string
		if err := rows.Scan(&uri); err != nil {
			return nil, err
		}
		out = append(out, uri)
	}
	return out, rows.Err()
}

func (b *CalDAV) CalendarObject(ctx context.Context, id caldav.CalendarID, objectURI string) (*caldav.ObjectInfo, error) {
	obj, err := scanObject(b.DB.QueryRowContext(ctx,
		`SELECT `+objectColumns+` FROM calendarobjects WHERE calendarid = $1 AND uri = $2`,
		id.Calendar, objectURI), id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return obj, err
}

func scanObject(row rowScanner, id caldav.CalendarID) (*caldav.ObjectInfo, error) {
	var (
		rowID, lastmodified, size sql.NullInt64
		uri, componenttype        sql.NullString
		etag                      sql.NullString
		data                      []byte
	)
	if err := row.Scan(&rowID, &uri, &lastmodified, &etag, &data, &size, &componenttype); err != nil {
		return nil, err
	}
	return &caldav.ObjectInfo{
		URI:          uri.String,
		CalendarID:   id,
		CalendarData: data,
		ETag:         `"` + etag.String + `"`,
		Size:         size.Int64,
		Component:    componenttype.String,
	}, nil
}

// CreateCalendarObject mirrors sabre's createCalendarObject + addChange,
// wrapped in one transaction (deliberately safer than sabre's unwrapped
// statements — see PLAN.md §5).
func (b *CalDAV) CreateCalendarObject(ctx context.Context, id caldav.CalendarID, objectURI string, data []byte) (string, error) {
	defer b.Metrics.DBOpTimer("create_object")()
	d, err := ical.Denormalize(data)
	if err != nil {
		return "", err
	}
	err = withTx(ctx, b.DB, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO calendarobjects
       (calendarid, uri, calendardata, lastmodified, etag, size, componenttype, firstoccurence, lastoccurence, uid)
VALUES ($1, $2, $3, extract(epoch from now())::int, $4, $5, $6, $7, $8, $9)`,
			id.Calendar, objectURI, data, d.ETag, d.Size, d.ComponentType,
			d.FirstOccurrence, d.LastOccurrence, d.UID); err != nil {
			return err
		}
		return recordChange(ctx, tx, calendarLog, id.Calendar, objectURI, 1)
	})
	if err != nil {
		return "", err
	}
	return `"` + d.ETag + `"`, nil
}

func (b *CalDAV) UpdateCalendarObject(ctx context.Context, id caldav.CalendarID, objectURI string, data []byte) (string, error) {
	defer b.Metrics.DBOpTimer("update_object")()
	d, err := ical.Denormalize(data)
	if err != nil {
		return "", err
	}
	err = withTx(ctx, b.DB, func(tx *sql.Tx) error {
		// A vanished object is ErrNotFound, with no change row: logging a
		// modify for a uri that no longer exists would tell sync clients to
		// fetch something that 404s.
		if err := affectedOne(tx.ExecContext(ctx, `
UPDATE calendarobjects
   SET calendardata = $1, lastmodified = extract(epoch from now())::int,
       etag = $2, size = $3, componenttype = $4,
       firstoccurence = $5, lastoccurence = $6, uid = $7
 WHERE calendarid = $8 AND uri = $9`,
			data, d.ETag, d.Size, d.ComponentType, d.FirstOccurrence, d.LastOccurrence,
			d.UID, id.Calendar, objectURI)); err != nil {
			return err
		}
		return recordChange(ctx, tx, calendarLog, id.Calendar, objectURI, 2)
	})
	if err != nil {
		return "", err
	}
	return `"` + d.ETag + `"`, nil
}

func (b *CalDAV) DeleteCalendarObject(ctx context.Context, id caldav.CalendarID, objectURI string) error {
	return withTx(ctx, b.DB, func(tx *sql.Tx) error {
		if err := affectedOne(tx.ExecContext(ctx,
			`DELETE FROM calendarobjects WHERE calendarid = $1 AND uri = $2`,
			id.Calendar, objectURI)); err != nil {
			return err
		}
		return recordChange(ctx, tx, calendarLog, id.Calendar, objectURI, 3)
	})
}
