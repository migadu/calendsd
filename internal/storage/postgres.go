// Package storage implements the backends against the shared sabre/dav
// Postgres schema (PLAN.md §5). Every query is transcribed from sabre's
// Backend\PDO classes; every value written must be readable by a live sabre
// instance on the same database, and vice versa. The schema is the
// compatibility contract — no DDL lives here.
package storage

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"calserver/internal/caldav"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
	"calserver/internal/metrics"
	"calserver/internal/pgdial"
)

// syncTokenPrefix is sabre's Sync\Plugin::SYNCTOKEN_PREFIX; the ctag/token
// format existing clients have stored.
const syncTokenPrefix = "http://sabre.io/ns/sync/"

// Pool bounds the database/sql connection pool. A zero field takes its
// DefaultPool value, so callers can override only the knobs they care about.
type Pool struct {
	// MaxOpen caps total connections. The database/sql default is unlimited,
	// which lets a load spike open arbitrarily many connections against the
	// Postgres we share with a live sabre instance and starve it — so we
	// always set a bound.
	MaxOpen int
	// MaxIdle caps idle connections kept for reuse (database/sql default 2).
	MaxIdle int
	// ConnMaxLifetime retires a connection after this age, so the pool
	// recycles across Postgres failovers and connection poolers instead of
	// clinging to a dead endpoint. Zero (database/sql default) never retires.
	ConnMaxLifetime time.Duration
}

// DefaultPool is the conservative bound applied when a field is left zero.
// Sized to leave headroom under a shared Postgres's max_connections for the
// live sabre instance, the admin panel, and calctl.
var DefaultPool = Pool{MaxOpen: 20, MaxIdle: 5, ConnMaxLifetime: 5 * time.Minute}

// Open connects to the shared Postgres database with DefaultPool bounds —
// fine for tests and short-lived CLIs. Long-running servers use OpenPool to
// tune the pool from config.
func Open(dsn string) (*sql.DB, error) {
	return OpenPool(dsn, Pool{})
}

// OpenPool connects and applies pool bounds, filling any zero field from
// DefaultPool so an unbounded pool can never reach the shared database. The
// DSN may list several hosts; see pgdial for the failover defaults applied.
func OpenPool(dsn string, pool Pool) (*sql.DB, error) {
	cfg, err := pgdial.Config(dsn)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cfg)
	if pool.MaxOpen <= 0 {
		pool.MaxOpen = DefaultPool.MaxOpen
	}
	if pool.MaxIdle <= 0 {
		pool.MaxIdle = DefaultPool.MaxIdle
	}
	if pool.ConnMaxLifetime <= 0 {
		pool.ConnMaxLifetime = DefaultPool.ConnMaxLifetime
	}
	db.SetMaxOpenConns(pool.MaxOpen)
	db.SetMaxIdleConns(pool.MaxIdle)
	db.SetConnMaxLifetime(pool.ConnMaxLifetime)
	return db, nil
}

// CalDAV is the calendar backend over the sabre schema. Port of
// CalDAV\Backend\PDO.
type CalDAV struct {
	DB *sql.DB
	// Metrics times the named expensive operations (calendar_query,
	// changes_for_calendar, create/update_object, scheduling_objects) —
	// deliberately not every query. A nil Set records nothing.
	Metrics *metrics.Set
}

// propertyMap mirrors sabre's PDO::$propertyMap: calendar properties stored
// as real columns on calendarinstances.
var propertyMap = []struct {
	name   xml.Name
	column string
}{
	{xml.Name{Space: davxml.NSDav, Local: "displayname"}, "displayname"},
	{xml.Name{Space: davxml.NSCalDAV, Local: "calendar-description"}, "description"},
	{xml.Name{Space: davxml.NSCalDAV, Local: "calendar-timezone"}, "timezone"},
	{xml.Name{Space: "http://apple.com/ns/ical/", Local: "calendar-order"}, "calendarorder"},
	{xml.Name{Space: "http://apple.com/ns/ical/", Local: "calendar-color"}, "calendarcolor"},
}

func (b *CalDAV) CalendarsForUser(ctx context.Context, principalURI string) ([]caldav.CalendarInfo, error) {
	rows, err := b.DB.QueryContext(ctx, `
SELECT calendarinstances.id as id, displayname, description, timezone,
       calendarorder, calendarcolor, calendarid, uri, synctoken, components,
       principaluri, transparent, access
  FROM calendarinstances
  LEFT JOIN calendars ON calendarinstances.calendarid = calendars.id
 WHERE principaluri = $1 ORDER BY calendarorder ASC`, principalURI)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []caldav.CalendarInfo
	for rows.Next() {
		var (
			instanceID, calendarID, calendarorder, transparent, access    int64
			synctoken                                                     sql.NullInt64
			uri, principal                                                string
			displayname, description, timezone, calendarcolor, components sql.NullString
		)
		if err := rows.Scan(&instanceID, &displayname, &description, &timezone,
			&calendarorder, &calendarcolor, &calendarID, &uri, &synctoken,
			&components, &principal, &transparent, &access); err != nil {
			return nil, err
		}
		info := caldav.CalendarInfo{
			ID: caldav.CalendarID{
				Calendar: strconv.FormatInt(calendarID, 10),
				Instance: strconv.FormatInt(instanceID, 10),
			},
			URI:          uri,
			PrincipalURI: principal,
			Props:        map[xml.Name]any{},
			ShareAccess:  int(access),
		}
		// An orphaned instance (no calendars row) degrades to token "0",
		// like sabre's ?: '0' fallback.
		token := "0"
		if synctoken.Valid {
			token = strconv.FormatInt(synctoken.Int64, 10)
		}
		info.Props[xml.Name{Space: davxml.NSCalendarServer, Local: "getctag"}] = syncTokenPrefix + token
		info.Props[xml.Name{Space: davxml.NSSabreDAV, Local: "sync-token"}] = token
		// Sharee views carry sabre's read-only marker; the calendar ACLs
		// drop write privileges from it.
		if access == caldav.AccessRead {
			info.Props[xml.Name{Space: davxml.NSSabreDAV, Local: "read-only"}] = "1"
		}
		info.Props[xml.Name{Space: davxml.NSCalDAV, Local: "supported-calendar-component-set"}] = componentSetValue(components.String)
		transp := "opaque"
		if transparent != 0 {
			transp = "transparent"
		}
		info.Props[xml.Name{Space: davxml.NSCalDAV, Local: "schedule-calendar-transp"}] = davxml.Raw("<cal:" + transp + "/>")
		setNullable(info.Props, 0, displayname)
		setNullable(info.Props, 1, description)
		setNullable(info.Props, 2, timezone)
		if calendarorder != 0 {
			info.Props[propertyMap[3].name] = strconv.FormatInt(calendarorder, 10)
		}
		setNullable(info.Props, 4, calendarcolor)
		out = append(out, info)
	}
	return out, rows.Err()
}

func setNullable(props map[xml.Name]any, mapIndex int, v sql.NullString) {
	if v.Valid && v.String != "" {
		props[propertyMap[mapIndex].name] = v.String
	}
}

func componentSetValue(components string) davxml.Raw {
	var b strings.Builder
	for _, c := range strings.Split(components, ",") {
		if c != "" {
			fmt.Fprintf(&b, `<cal:comp name="%s"/>`, c)
		}
	}
	return davxml.Raw(b.String())
}

var compNameRe = regexp.MustCompile(`name="([A-Z]+)"`)

func (b *CalDAV) CreateCalendar(ctx context.Context, principalURI, calendarURI string, props map[xml.Name]any) (caldav.CalendarID, error) {
	components := "VEVENT,VTODO"
	if v, exists := props[xml.Name{Space: davxml.NSCalDAV, Local: "supported-calendar-component-set"}]; exists {
		if raw, isRaw := v.([]byte); isRaw {
			var names []string
			for _, m := range compNameRe.FindAllStringSubmatch(string(raw), -1) {
				names = append(names, m[1])
			}
			if len(names) > 0 {
				components = strings.Join(names, ",")
			}
		}
	}

	var calendarID, instanceID int64
	err := withTx(ctx, b.DB, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO calendars (synctoken, components) VALUES (1, $1) RETURNING id`,
			components).Scan(&calendarID); err != nil {
			return err
		}

		columns := []string{"calendarid", "principaluri", "uri", "transparent"}
		values := []any{calendarID, principalURI, calendarURI, 0}
		for _, pm := range propertyMap {
			if v, exists := props[pm.name]; exists {
				if s, isString := v.(string); isString {
					columns = append(columns, pm.column)
					values = append(values, s)
				}
			}
		}
		placeholders := make([]string, len(values))
		for i := range values {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
		return tx.QueryRowContext(ctx, fmt.Sprintf(
			`INSERT INTO calendarinstances (%s) VALUES (%s) RETURNING id`,
			strings.Join(columns, ", "), strings.Join(placeholders, ", ")),
			values...).Scan(&instanceID)
	})
	if err != nil {
		return caldav.CalendarID{}, err
	}
	return caldav.CalendarID{
		Calendar: strconv.FormatInt(calendarID, 10),
		Instance: strconv.FormatInt(instanceID, 10),
	}, nil
}

// UpdateCalendar transcribes sabre's updateCalendar: the propertyMap columns
// plus schedule-calendar-transp update calendarinstances (by instance ID)
// and bump the synctoken with an empty-uri change record.
func (b *CalDAV) UpdateCalendar(ctx context.Context, id caldav.CalendarID, pp *dav.PropPatch) {
	transp := xml.Name{Space: davxml.NSCalDAV, Local: "schedule-calendar-transp"}
	supported := []xml.Name{transp}
	for _, pm := range propertyMap {
		supported = append(supported, pm.name)
	}

	pp.Handle(supported, func(values map[xml.Name]any) (map[xml.Name]int, bool) {
		var assignments []string
		var args []any
		for name, value := range values {
			if name == transp {
				v := 0
				if raw, ok := value.([]byte); ok && strings.Contains(string(raw), "transparent") {
					v = 1
				}
				args = append(args, v)
				assignments = append(assignments, fmt.Sprintf("transparent = $%d", len(args)))
				continue
			}
			for _, pm := range propertyMap {
				if pm.name == name {
					args = append(args, toNullString(value))
					assignments = append(assignments, fmt.Sprintf("%s = $%d", pm.column, len(args)))
				}
			}
		}
		if len(assignments) == 0 {
			return nil, true
		}

		args = append(args, id.Instance)
		err := withTx(ctx, b.DB, func(tx *sql.Tx) error {
			if err := affectedOne(tx.ExecContext(ctx, fmt.Sprintf(
				`UPDATE calendarinstances SET %s WHERE id = $%d`,
				strings.Join(assignments, ", "), len(args)), args...)); err != nil {
				return err
			}
			return recordChange(ctx, tx, calendarLog, id.Calendar, "", 2)
		})
		return nil, err == nil
	})
}

// toNullString maps removal (nil) to NULL and values to their text form.
func toNullString(v any) any {
	switch value := v.(type) {
	case nil:
		return nil
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return fmt.Sprint(value)
	}
}

// DeleteCalendar transcribes sabre's access-aware deleteCalendar: only the
// shared owner (access=1) removes the calendar with all its data; a sharee
// merely drops their own instance row, leaving the owner's calendar intact.
func (b *CalDAV) DeleteCalendar(ctx context.Context, id caldav.CalendarID) error {
	return withTx(ctx, b.DB, func(tx *sql.Tx) error {
		var access int
		if err := tx.QueryRowContext(ctx,
			`SELECT access FROM calendarinstances WHERE id = $1`, id.Instance).Scan(&access); err != nil {
			return err
		}
		if access == 1 { // Sharing\Plugin::ACCESS_SHAREDOWNER
			return deleteCalendarRows(ctx, tx, id.Calendar)
		}
		_, err := tx.ExecContext(ctx,
			`DELETE FROM calendarinstances WHERE id = $1`, id.Instance)
		return err
	})
}

// deleteCalendarRows removes a calendar and everything hanging off it —
// objects, change log, every instance row (the owner's and any sharees'),
// and the calendars row itself. Shared by DeleteCalendar's owner branch and
// AdminWrites.DeleteUserCascade so the two delete paths cannot drift.
func deleteCalendarRows(ctx context.Context, tx *sql.Tx, calendarID string) error {
	for _, q := range []string{
		`DELETE FROM calendarobjects WHERE calendarid = $1`,
		`DELETE FROM calendarchanges WHERE calendarid = $1`,
		`DELETE FROM calendarinstances WHERE calendarid = $1`,
		`DELETE FROM calendars WHERE id = $1`,
	} {
		if _, err := tx.ExecContext(ctx, q, calendarID); err != nil {
			return err
		}
	}
	return nil
}

// IsUniqueViolation reports whether err is Postgres refusing a duplicate key
// (SQLSTATE 23505). Callers that check-then-insert use it to turn the race
// they cannot close — two requests for the same card or book arriving
// together — into the same answer the check would have given.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
