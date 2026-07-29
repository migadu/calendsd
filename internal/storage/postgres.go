// Package storage implements the backends against the shared sabre/dav
// Postgres schema (PLAN.md §5). Every query is transcribed from sabre's
// Backend\PDO classes; every value written must be readable by a live sabre
// instance on the same database, and vice versa. The schema is the
// compatibility contract — no DDL lives here.
package storage

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	// Registers the pgx database/sql driver.
	_ "github.com/jackc/pgx/v5/stdlib"

	"calserver/internal/acl"
	"calserver/internal/caldav"
	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
	"calserver/internal/metrics"
)

// syncTokenPrefix is sabre's Sync\Plugin::SYNCTOKEN_PREFIX; the ctag/token
// format existing clients have stored.
const syncTokenPrefix = "http://sabre.io/ns/sync/"

// Open connects to the shared Postgres database.
func Open(dsn string) (*sql.DB, error) {
	return sql.Open("pgx", dsn)
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

func (b *CalDAV) CalendarsForUser(principalURI string) ([]caldav.CalendarInfo, error) {
	rows, err := b.DB.Query(`
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
		if access == 2 { // Sharing\Plugin::ACCESS_READ
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

func (b *CalDAV) CreateCalendar(principalURI, calendarURI string, props map[xml.Name]any) (caldav.CalendarID, error) {
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

	tx, err := b.DB.Begin()
	if err != nil {
		return caldav.CalendarID{}, err
	}
	defer tx.Rollback()

	var calendarID int64
	if err := tx.QueryRow(
		`INSERT INTO calendars (synctoken, components) VALUES (1, $1) RETURNING id`,
		components).Scan(&calendarID); err != nil {
		return caldav.CalendarID{}, err
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
	var instanceID int64
	if err := tx.QueryRow(fmt.Sprintf(
		`INSERT INTO calendarinstances (%s) VALUES (%s) RETURNING id`,
		strings.Join(columns, ", "), strings.Join(placeholders, ", ")),
		values...).Scan(&instanceID); err != nil {
		return caldav.CalendarID{}, err
	}
	if err := tx.Commit(); err != nil {
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
func (b *CalDAV) UpdateCalendar(id caldav.CalendarID, pp *dav.PropPatch) {
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

		tx, err := b.DB.Begin()
		if err != nil {
			return nil, false
		}
		defer tx.Rollback()
		args = append(args, id.Instance)
		if _, err := tx.Exec(fmt.Sprintf(
			`UPDATE calendarinstances SET %s WHERE id = $%d`,
			strings.Join(assignments, ", "), len(args)), args...); err != nil {
			return nil, false
		}
		if err := addChange(tx, id.Calendar, "", 2); err != nil {
			return nil, false
		}
		if err := tx.Commit(); err != nil {
			return nil, false
		}
		return nil, true
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
func (b *CalDAV) DeleteCalendar(id caldav.CalendarID) error {
	tx, err := b.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var access int
	if err := tx.QueryRow(
		`SELECT access FROM calendarinstances WHERE id = $1`, id.Instance).Scan(&access); err != nil {
		return err
	}
	if access == 1 { // Sharing\Plugin::ACCESS_SHAREDOWNER
		for _, q := range []string{
			`DELETE FROM calendarobjects WHERE calendarid = $1`,
			`DELETE FROM calendarchanges WHERE calendarid = $1`,
			`DELETE FROM calendarinstances WHERE calendarid = $1`,
			`DELETE FROM calendars WHERE id = $1`,
		} {
			if _, err := tx.Exec(q, id.Calendar); err != nil {
				return err
			}
		}
	} else {
		if _, err := tx.Exec(
			`DELETE FROM calendarinstances WHERE id = $1`, id.Instance); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const objectColumns = `id, uri, lastmodified, etag, calendardata, size, componenttype`

func (b *CalDAV) CalendarObjects(id caldav.CalendarID) ([]caldav.ObjectInfo, error) {
	rows, err := b.DB.Query(
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
func (b *CalDAV) CalendarQuery(id caldav.CalendarID, componentType string, start, end time.Time) ([]string, error) {
	defer b.Metrics.DBOpTimer("calendar_query")()
	q := `SELECT uri FROM calendarobjects WHERE calendarid = $1`
	args := []any{id.Calendar}
	if componentType != "" {
		args = append(args, componentType)
		q += fmt.Sprintf(" AND componenttype = $%d", len(args))
	}
	if !end.IsZero() {
		args = append(args, int(end.Unix()))
		q += fmt.Sprintf(" AND (firstoccurence IS NULL OR firstoccurence < $%d)", len(args))
	}
	if !start.IsZero() {
		args = append(args, int(start.Unix()))
		q += fmt.Sprintf(" AND (lastoccurence IS NULL OR lastoccurence > $%d)", len(args))
	}
	rows, err := b.DB.Query(q, args...)
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

func (b *CalDAV) CalendarObject(id caldav.CalendarID, objectURI string) (*caldav.ObjectInfo, error) {
	rows, err := b.DB.Query(
		`SELECT `+objectColumns+` FROM calendarobjects WHERE calendarid = $1 AND uri = $2`,
		id.Calendar, objectURI)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanObject(rows, id)
}

func scanObject(rows *sql.Rows, id caldav.CalendarID) (*caldav.ObjectInfo, error) {
	var (
		rowID, lastmodified, size sql.NullInt64
		uri, componenttype        sql.NullString
		etag                      sql.NullString
		data                      []byte
	)
	if err := rows.Scan(&rowID, &uri, &lastmodified, &etag, &data, &size, &componenttype); err != nil {
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
func (b *CalDAV) CreateCalendarObject(id caldav.CalendarID, objectURI string, data []byte) (string, error) {
	defer b.Metrics.DBOpTimer("create_object")()
	d, err := ical.Denormalize(data)
	if err != nil {
		return "", err
	}
	tx, err := b.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
INSERT INTO calendarobjects
       (calendarid, uri, calendardata, lastmodified, etag, size, componenttype, firstoccurence, lastoccurence, uid)
VALUES ($1, $2, $3, extract(epoch from now())::int, $4, $5, $6, $7, $8, $9)`,
		id.Calendar, objectURI, data, d.ETag, d.Size, d.ComponentType,
		d.FirstOccurrence, d.LastOccurrence, d.UID); err != nil {
		return "", err
	}
	if err := addChange(tx, id.Calendar, objectURI, 1); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return `"` + d.ETag + `"`, nil
}

func (b *CalDAV) UpdateCalendarObject(id caldav.CalendarID, objectURI string, data []byte) (string, error) {
	defer b.Metrics.DBOpTimer("update_object")()
	d, err := ical.Denormalize(data)
	if err != nil {
		return "", err
	}
	tx, err := b.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
UPDATE calendarobjects
   SET calendardata = $1, lastmodified = extract(epoch from now())::int,
       etag = $2, size = $3, componenttype = $4,
       firstoccurence = $5, lastoccurence = $6, uid = $7
 WHERE calendarid = $8 AND uri = $9`,
		data, d.ETag, d.Size, d.ComponentType, d.FirstOccurrence, d.LastOccurrence,
		d.UID, id.Calendar, objectURI); err != nil {
		return "", err
	}
	if err := addChange(tx, id.Calendar, objectURI, 2); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return `"` + d.ETag + `"`, nil
}

func (b *CalDAV) DeleteCalendarObject(id caldav.CalendarID, objectURI string) error {
	tx, err := b.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`DELETE FROM calendarobjects WHERE calendarid = $1 AND uri = $2`,
		id.Calendar, objectURI); err != nil {
		return err
	}
	if err := addChange(tx, id.Calendar, objectURI, 3); err != nil {
		return err
	}
	return tx.Commit()
}

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

// SchedulingObjects reads a principal's schedule inbox from the shared
// schedulingobjects table. Port of PDO getSchedulingObjects.
func (b *CalDAV) SchedulingObjects(principalURI string) ([]caldav.SchedulingObjectInfo, error) {
	defer b.Metrics.DBOpTimer("scheduling_objects")()
	rows, err := b.DB.Query(
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

func (b *CalDAV) SchedulingObject(principalURI, objectURI string) (*caldav.SchedulingObjectInfo, error) {
	var data []byte
	err := b.DB.QueryRow(
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
func (b *CalDAV) CreateSchedulingObject(principalURI, objectURI string, data []byte) error {
	sum := md5.Sum(data)
	_, err := b.DB.Exec(`
INSERT INTO schedulingobjects (principaluri, calendardata, uri, lastmodified, etag, size)
VALUES ($1, $2, $3, extract(epoch from now())::int, $4, $5)`,
		principalURI, data, objectURI, hex.EncodeToString(sum[:]), len(data))
	return err
}

func (b *CalDAV) DeleteSchedulingObject(principalURI, objectURI string) error {
	_, err := b.DB.Exec(
		`DELETE FROM schedulingobjects WHERE principaluri = $1 AND uri = $2`,
		principalURI, objectURI)
	return err
}

// Principals is the principal backend over the sabre schema. Port of
// DAVACL\PrincipalBackend\PDO.
type Principals struct {
	DB *sql.DB
}

func (p *Principals) PrincipalsByPrefix(prefix string) ([]acl.Principal, error) {
	rows, err := p.DB.Query(`SELECT uri, email, displayname FROM principals`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []acl.Principal
	for rows.Next() {
		principal, err := scanPrincipal(rows)
		if err != nil {
			return nil, err
		}
		if parent, _ := splitURI(principal.URI); parent == prefix {
			out = append(out, *principal)
		}
	}
	return out, rows.Err()
}

func (p *Principals) PrincipalByPath(path string) (*acl.Principal, error) {
	rows, err := p.DB.Query(
		`SELECT uri, email, displayname FROM principals WHERE uri = $1`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanPrincipal(rows)
}

func scanPrincipal(rows *sql.Rows) (*acl.Principal, error) {
	var uri string
	var email, displayname sql.NullString
	if err := rows.Scan(&uri, &email, &displayname); err != nil {
		return nil, err
	}
	principal := acl.Principal{URI: uri, Props: map[xml.Name]any{}}
	if displayname.Valid && displayname.String != "" {
		principal.Props[xml.Name{Space: davxml.NSDav, Local: "displayname"}] = displayname.String
	}
	if email.Valid && email.String != "" {
		principal.Props[xml.Name{Space: davxml.NSSabreDAV, Local: "email-address"}] = email.String
	}
	return &principal, nil
}

func (p *Principals) GroupMemberSet(principal string) ([]string, error) {
	return p.groupQuery(`
SELECT members.uri
  FROM groupmembers
  JOIN principals AS groups ON groupmembers.principal_id = groups.id
  JOIN principals AS members ON groupmembers.member_id = members.id
 WHERE groups.uri = $1`, principal)
}

func (p *Principals) GroupMembership(principal string) ([]string, error) {
	return p.groupQuery(`
SELECT groups.uri
  FROM groupmembers
  JOIN principals AS groups ON groupmembers.principal_id = groups.id
  JOIN principals AS members ON groupmembers.member_id = members.id
 WHERE members.uri = $1`, principal)
}

// SetGroupMemberSet replaces a group's members, transcribing sabre's PDO
// setGroupMemberSet: resolve principal ids, delete the old rows, insert the new
// ones. The group and every member must already exist as principals.
func (p *Principals) SetGroupMemberSet(principal string, members []string) error {
	tx, err := p.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var groupID int64
	if err := tx.QueryRow(`SELECT id FROM principals WHERE uri = $1`, principal).Scan(&groupID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM groupmembers WHERE principal_id = $1`, groupID); err != nil {
		return err
	}
	for _, member := range members {
		var memberID int64
		if err := tx.QueryRow(`SELECT id FROM principals WHERE uri = $1`, member).Scan(&memberID); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO groupmembers (principal_id, member_id) VALUES ($1, $2)`,
			groupID, memberID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// searchColumns maps searchable principal properties to columns.
var searchColumns = map[xml.Name]string{
	{Space: davxml.NSDav, Local: "displayname"}:        "displayname",
	{Space: davxml.NSSabreDAV, Local: "email-address"}: "email",
}

// SearchPrincipals implements a substring search over the searchable
// principal columns. Port of PrincipalBackend\PDO::searchPrincipals.
func (p *Principals) SearchPrincipals(prefix string, search map[xml.Name]string, test string) ([]string, error) {
	if len(search) == 0 {
		return nil, nil
	}
	var clauses []string
	args := []any{prefix + "/%"}
	for name, needle := range search {
		col, ok := searchColumns[name]
		if !ok {
			// Unknown property never matches: allof → no rows, anyof →
			// this term contributes nothing.
			if test != "anyof" {
				return nil, nil
			}
			continue
		}
		args = append(args, "%"+escapeLike(strings.ToLower(needle))+"%")
		clauses = append(clauses, "lower("+col+") LIKE $"+strconv.Itoa(len(args)))
	}
	if len(clauses) == 0 {
		return nil, nil
	}
	joiner := " AND "
	if test == "anyof" {
		joiner = " OR "
	}
	rows, err := p.DB.Query(
		"SELECT uri FROM principals WHERE uri LIKE $1 AND ("+strings.Join(clauses, joiner)+")", args...)
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

func (p *Principals) groupQuery(query, principal string) ([]string, error) {
	rows, err := p.DB.Query(query, principal)
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

func splitURI(uri string) (parent, base string) {
	i := strings.LastIndexByte(uri, '/')
	if i < 0 {
		return "", uri
	}
	return uri[:i], uri[i+1:]
}

// Users reads the shared users table for authentication. Port of
// DAV\Auth\Backend\PDO: digesta1 = md5(user:realm:pass) with the realm baked
// in (PLAN.md §5).
type Users struct {
	DB *sql.DB
}

// HashA1 returns the stored digesta1 hash for a user.
func (u *Users) HashA1(realm, username string) (string, bool) {
	var hash string
	err := u.DB.QueryRow(
		`SELECT digesta1 FROM users WHERE username = $1`, username).Scan(&hash)
	return hash, err == nil
}
