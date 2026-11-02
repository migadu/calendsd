// Scheduling inbox objects over the sabre schedulingobjects table.

package storage

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/caldav"
)

// SchedulingObjects reads a principal's schedule inbox from the shared
// schedulingobjects table. Port of PDO getSchedulingObjects.
func (b *CalDAV) SchedulingObjects(ctx context.Context, principalURI string) ([]caldav.SchedulingObjectInfo, error) {
	defer b.Metrics.DBOpTimer("scheduling_objects")()
	rows, err := b.DB.QueryContext(ctx,
		`SELECT uri, calendardata FROM schedulingobjects WHERE principaluri = $1`, principalURI)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []caldav.SchedulingObjectInfo
	for rows.Next() {
		var uri string
		var data []byte
		if err := rows.Scan(&uri, &data); err != nil {
			return nil, err
		}
		out = append(out, caldav.SchedulingObjectInfo{
			URI: uri, PrincipalURI: principalURI, CalendarData: data,
		})
	}
	return out, rows.Err()
}

func (b *CalDAV) SchedulingObject(ctx context.Context, principalURI, objectURI string) (*caldav.SchedulingObjectInfo, error) {
	var data []byte
	err := b.DB.QueryRowContext(ctx,
		`SELECT calendardata FROM schedulingobjects WHERE principaluri = $1 AND uri = $2`,
		principalURI, objectURI).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &caldav.SchedulingObjectInfo{URI: objectURI, PrincipalURI: principalURI, CalendarData: data}, nil
}

// CreateSchedulingObject transcribes PDO createSchedulingObject: md5 etag,
// size, epoch lastmodified.
func (b *CalDAV) CreateSchedulingObject(ctx context.Context, principalURI, objectURI string, data []byte) error {
	sum := md5.Sum(data)
	_, err := b.DB.ExecContext(ctx, `
INSERT INTO schedulingobjects (principaluri, calendardata, uri, lastmodified, etag, size)
VALUES ($1, $2, $3, extract(epoch from now())::int, $4, $5)`,
		principalURI, data, objectURI, hex.EncodeToString(sum[:]), len(data))
	return err
}

func (b *CalDAV) DeleteSchedulingObject(ctx context.Context, principalURI, objectURI string) error {
	_, err := b.DB.ExecContext(ctx,
		`DELETE FROM schedulingobjects WHERE principaluri = $1 AND uri = $2`,
		principalURI, objectURI)
	return err
}

var _ caldav.AvailabilityBackend = (*CalDAV)(nil)

// Availability reads a principal's working hours from calendar_availability
// (docs/design/VAVAILABILITY-PLAN.md §3.1); nil when none are set.
func (b *CalDAV) Availability(ctx context.Context, principalURI string) ([]byte, error) {
	defer b.Metrics.DBOpTimer("availability")()
	var data []byte
	err := b.DB.QueryRowContext(ctx,
		`SELECT data FROM calendar_availability WHERE principal = $1`, principalURI).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return data, err
}

func (b *CalDAV) SetAvailability(ctx context.Context, principalURI string, data []byte) error {
	_, err := b.DB.ExecContext(ctx, `
INSERT INTO calendar_availability (principal, data) VALUES ($1, $2)
ON CONFLICT (principal) DO UPDATE SET data = $2, updated_at = now()`,
		principalURI, string(data))
	return err
}

func (b *CalDAV) DeleteAvailability(ctx context.Context, principalURI string) error {
	_, err := b.DB.ExecContext(ctx, `DELETE FROM calendar_availability WHERE principal = $1`, principalURI)
	return err
}

var _ caldav.FreeBusyVisibilityBackend = (*CalDAV)(nil)

// FreeBusyVisibility reads who may see a principal's free-busy
// (docs/design/VAVAILABILITY-PLAN.md §10); the zero value (their mail domain) when no
// row exists. It backs the ACL plugin's FreeBusyPolicy, so it runs on every
// non-owner read-free-busy check.
func (b *CalDAV) FreeBusyVisibility(ctx context.Context, principalURI string) (acl.FreeBusyVisibility, error) {
	defer b.Metrics.DBOpTimer("free_busy_visibility")()
	var audience string
	var allowed []string
	err := b.DB.QueryRowContext(ctx,
		`SELECT audience, allowed FROM free_busy_visibility WHERE principal = $1`,
		principalURI).Scan(&audience, textArray(&allowed))
	if err == sql.ErrNoRows {
		return acl.FreeBusyVisibility{}, nil
	}
	if err != nil {
		return acl.FreeBusyVisibility{}, err
	}
	return acl.FreeBusyVisibility{Hidden: audience == "nobody", Allowed: allowed}, nil
}

func (b *CalDAV) SetFreeBusyVisibility(ctx context.Context, principalURI string, v acl.FreeBusyVisibility) error {
	audience := "domain"
	if v.Hidden {
		audience = "nobody"
	}
	// Non-nil, so an empty list is '{}' rather than NULL in the NOT NULL column.
	allowed := append([]string{}, v.Allowed...)
	_, err := b.DB.ExecContext(ctx, `
INSERT INTO free_busy_visibility (principal, audience, allowed)
VALUES ($1, $2, $3::text[])
ON CONFLICT (principal) DO UPDATE SET audience = $2, allowed = EXCLUDED.allowed, updated_at = now()`,
		principalURI, audience, allowed)
	return err
}

func (b *CalDAV) DeleteFreeBusyVisibility(ctx context.Context, principalURI string) error {
	_, err := b.DB.ExecContext(ctx, `DELETE FROM free_busy_visibility WHERE principal = $1`, principalURI)
	return err
}

// textArray scans a TEXT[] column into dst with pgx's own array codec;
// database/sql hands arrays over as their text form, which this decodes.
// A Map is cheap to make (the type registry behind it is shared) and not
// safe for concurrent use, so each scan gets its own.
func textArray(dst *[]string) sql.Scanner {
	return pgtype.NewMap().SQLScanner(dst)
}
