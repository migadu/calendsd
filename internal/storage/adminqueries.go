// Admin panel read-only aggregates (ADMIN-PLAN.md §4). The backend
// interfaces are per-principal shaped; the dashboard, inspectors, and object
// browser need cross-user joins and counts, collected here. Everything in
// this file is SELECT-only — admin mutations go through the regular backends
// (and, for users/principals, the AdminWrites carve-out).
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"calserver/scripts/schema"
)

// AdminQueries bundles the read-only SQL the admin panel needs beyond the
// per-principal backend interfaces.
type AdminQueries struct {
	DB *sql.DB
}

// AdminCounts is the dashboard's row-count overview.
type AdminCounts struct {
	Principals        int `json:"principals"`
	Users             int `json:"users"`
	Calendars         int `json:"calendars"`
	CalendarInstances int `json:"calendarInstances"`
	CalendarObjects   int `json:"calendarObjects"`
	AddressBooks      int `json:"addressBooks"`
	Cards             int `json:"cards"`
	SchedulingObjects int `json:"schedulingObjects"`
	PropertyRows      int `json:"propertyRows"`
}

// Counts tallies every table the panel inspects.
func (q *AdminQueries) Counts() (*AdminCounts, error) {
	var c AdminCounts
	for _, t := range []struct {
		table string
		dst   *int
	}{
		{"principals", &c.Principals},
		{"users", &c.Users},
		{"calendars", &c.Calendars},
		{"calendarinstances", &c.CalendarInstances},
		{"calendarobjects", &c.CalendarObjects},
		{"addressbooks", &c.AddressBooks},
		{"cards", &c.Cards},
		{"schedulingobjects", &c.SchedulingObjects},
		{"propertystorage", &c.PropertyRows},
	} {
		if err := q.DB.QueryRow("SELECT count(*) FROM " + t.table).Scan(t.dst); err != nil {
			return nil, fmt.Errorf("counting %s: %w", t.table, err)
		}
	}
	return &c, nil
}

// SchemaPresent verifies every sabre table the admin surface reads exists, and
// every table pgsql.calserver.sql creates, via a LIMIT 0 probe per table.
// Unlike Counts it stays O(1) on large databases, so it is safe in a health
// check that runs on a monitoring interval. A missing extension table fails it
// too: the server starts without one and only fails the requests that need it.
func (q *AdminQueries) SchemaPresent() error {
	return q.SchemaPresentContext(context.Background())
}

// SchemaPresentContext is SchemaPresent bounded by ctx.
func (q *AdminQueries) SchemaPresentContext(ctx context.Context) error {
	for _, table := range append([]string{
		"principals", "users", "calendars", "calendarinstances",
		"calendarobjects", "addressbooks", "cards", "schedulingobjects",
		"propertystorage", "groupmembers", "calendarchanges", "addressbookchanges",
	}, schema.Tables()...) {
		rows, err := q.DB.QueryContext(ctx, "SELECT 1 FROM "+table+" LIMIT 0")
		if err != nil {
			return fmt.Errorf("table %s: %w", table, err)
		}
		rows.Close()
	}
	return nil
}

// AdminChange is one change-log row for the dashboard's recent-activity
// list. The change tables carry no timestamp (ADMIN-PLAN.md §3.1):
// LastModified is joined from the live object and is nil for deletes.
type AdminChange struct {
	Kind          string `json:"kind"` // "calendar" or "addressbook"
	CollectionID  string `json:"collectionId"`
	CollectionURI string `json:"collectionUri"`
	PrincipalURI  string `json:"principalUri"`
	ObjectURI     string `json:"objectUri"`
	SyncToken     int64  `json:"syncToken"`
	Operation     int    `json:"operation"` // 1 add, 2 modify, 3 delete
	LastModified  *int64 `json:"lastModified"`
}

// RecentChanges returns the newest change-log rows across calendars and
// address books, most recent first (by change-row id per table, interleaved
// by nothing better — there is no cross-table ordering without timestamps).
func (q *AdminQueries) RecentChanges(limit int) ([]AdminChange, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var out []AdminChange
	rows, err := q.DB.Query(`
SELECT c.calendarid, coalesce(ci.uri, ''), coalesce(ci.principaluri, ''),
       c.uri, c.synctoken, c.operation, o.lastmodified
  FROM calendarchanges c
  LEFT JOIN calendarinstances ci ON ci.calendarid = c.calendarid AND ci.access = 1
  LEFT JOIN calendarobjects o ON o.calendarid = c.calendarid AND o.uri = c.uri
 ORDER BY c.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		ch := AdminChange{Kind: "calendar"}
		var id int64
		var lastmod sql.NullInt64
		if err := rows.Scan(&id, &ch.CollectionURI, &ch.PrincipalURI,
			&ch.ObjectURI, &ch.SyncToken, &ch.Operation, &lastmod); err != nil {
			return nil, err
		}
		ch.CollectionID = fmt.Sprint(id)
		if lastmod.Valid {
			ch.LastModified = &lastmod.Int64
		}
		out = append(out, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.DB.Query(`
SELECT c.addressbookid, coalesce(ab.uri, ''), coalesce(ab.principaluri, ''),
       c.uri, c.synctoken, c.operation, cd.lastmodified
  FROM addressbookchanges c
  LEFT JOIN addressbooks ab ON ab.id = c.addressbookid
  LEFT JOIN cards cd ON cd.addressbookid = c.addressbookid AND cd.uri = c.uri
 ORDER BY c.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		ch := AdminChange{Kind: "addressbook"}
		var id int64
		var lastmod sql.NullInt64
		if err := rows.Scan(&id, &ch.CollectionURI, &ch.PrincipalURI,
			&ch.ObjectURI, &ch.SyncToken, &ch.Operation, &lastmod); err != nil {
			return nil, err
		}
		ch.CollectionID = fmt.Sprint(id)
		if lastmod.Valid {
			ch.LastModified = &lastmod.Int64
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

// AdminAuthDiagnostics surfaces the classic silent-login-failure setups
// (ADMIN-PLAN.md §3.2). A realm mismatch cannot be detected offline —
// digesta1 is an opaque md5 — so that check is the interactive credential
// test in the admin app, not a query here.
type AdminAuthDiagnostics struct {
	// PrincipalsWithoutUser lists top-level principals with no users row
	// (proxy sub-principals are exempt: they never log in).
	PrincipalsWithoutUser []string `json:"principalsWithoutUser"`
	// UsersWithoutPrincipal lists usernames with no principals row.
	UsersWithoutPrincipal []string `json:"usersWithoutPrincipal"`
	// MalformedDigestA1 lists usernames whose digesta1 is not 32 hex chars.
	MalformedDigestA1 []string `json:"malformedDigestA1"`
}

// AuthDiagnostics cross-checks principals against users.
func (q *AdminQueries) AuthDiagnostics() (*AdminAuthDiagnostics, error) {
	var d AdminAuthDiagnostics
	collect := func(dst *[]string, query string) error {
		rows, err := q.DB.Query(query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			*dst = append(*dst, s)
		}
		return rows.Err()
	}
	if err := collect(&d.PrincipalsWithoutUser, `
SELECT p.uri FROM principals p
 WHERE p.uri LIKE 'principals/%' AND p.uri NOT LIKE 'principals/%/%'
   AND NOT EXISTS (SELECT 1 FROM users u WHERE 'principals/' || u.username = p.uri)
 ORDER BY p.uri`); err != nil {
		return nil, err
	}
	if err := collect(&d.UsersWithoutPrincipal, `
SELECT u.username FROM users u
 WHERE NOT EXISTS (SELECT 1 FROM principals p WHERE p.uri = 'principals/' || u.username)
 ORDER BY u.username`); err != nil {
		return nil, err
	}
	if err := collect(&d.MalformedDigestA1, `
SELECT u.username FROM users u
 WHERE u.digesta1 IS NULL OR u.digesta1 !~ '^[0-9a-f]{32}$'
 ORDER BY u.username`); err != nil {
		return nil, err
	}
	return &d, nil
}

// AdminUserRow is the users-table view of one principal.
type AdminUserRow struct {
	Username string `json:"username"`
	DigestA1 string `json:"digestA1"`
}

// UserForPrincipal returns the users row matching a principal path
// ("principals/<name>" → username "<name>"), or nil when absent.
func (q *AdminQueries) UserForPrincipal(principalURI string) (*AdminUserRow, error) {
	username := strings.TrimPrefix(principalURI, "principals/")
	var u AdminUserRow
	var digest sql.NullString
	err := q.DB.QueryRow(`SELECT username, digesta1 FROM users WHERE username = $1`,
		username).Scan(&u.Username, &digest)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.DigestA1 = digest.String
	return &u, nil
}

// AdminCalendarInstance is one calendarinstances row: a principal's view of
// a shared calendar. Sharing state lives here (ADMIN-PLAN.md §3.3).
type AdminCalendarInstance struct {
	InstanceID       string `json:"instanceId"`
	CalendarID       string `json:"calendarId"`
	PrincipalURI     string `json:"principalUri"`
	URI              string `json:"uri"`
	Access           int    `json:"access"` // 1 owner, 2 read, 3 read-write
	DisplayName      string `json:"displayName"`
	Description      string `json:"description"`
	Color            string `json:"color"`
	Order            int    `json:"order"`
	Timezone         string `json:"timezone"`
	Transparent      bool   `json:"transparent"`
	ShareHref        string `json:"shareHref"`
	ShareDisplayName string `json:"shareDisplayName"`
	InviteStatus     int    `json:"inviteStatus"` // 1 noresponse, 2 accepted, 3 declined, 4 invalid
}

// CalendarInstances lists every principal's instance of one calendar,
// owner first.
func (q *AdminQueries) CalendarInstances(calendarID string) ([]AdminCalendarInstance, error) {
	rows, err := q.DB.Query(`
SELECT id, calendarid, coalesce(principaluri, ''), coalesce(uri, ''), access,
       coalesce(displayname, ''), coalesce(description, ''),
       coalesce(calendarcolor, ''), calendarorder, coalesce(timezone, ''),
       transparent, coalesce(share_href, ''), coalesce(share_displayname, ''),
       share_invitestatus
  FROM calendarinstances WHERE calendarid = $1 ORDER BY access, principaluri`,
		calendarID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminCalendarInstance
	for rows.Next() {
		var in AdminCalendarInstance
		var instanceID, calID int64
		var transparent int
		if err := rows.Scan(&instanceID, &calID, &in.PrincipalURI, &in.URI,
			&in.Access, &in.DisplayName, &in.Description, &in.Color, &in.Order,
			&in.Timezone, &transparent, &in.ShareHref, &in.ShareDisplayName,
			&in.InviteStatus); err != nil {
			return nil, err
		}
		in.InstanceID = fmt.Sprint(instanceID)
		in.CalendarID = fmt.Sprint(calID)
		in.Transparent = transparent != 0
		out = append(out, in)
	}
	return out, rows.Err()
}

// LookupCalendar resolves (principal, calendar URI) to the shared calendars
// row id, or "" when no such instance exists.
func (q *AdminQueries) LookupCalendar(principalURI, uri string) (string, error) {
	var id int64
	err := q.DB.QueryRow(
		`SELECT calendarid FROM calendarinstances WHERE principaluri = $1 AND uri = $2`,
		principalURI, uri).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprint(id), nil
}

// AdminCalendarMeta is the shared (calendars-table) side of a calendar plus
// object aggregates.
type AdminCalendarMeta struct {
	SyncToken   int64  `json:"syncToken"`
	Components  string `json:"components"`
	ObjectCount int    `json:"objectCount"`
	TotalSize   int64  `json:"totalSize"`
}

// CalendarMeta returns sync token, supported components, and object totals
// for one calendar, or nil when the calendar does not exist.
func (q *AdminQueries) CalendarMeta(calendarID string) (*AdminCalendarMeta, error) {
	var m AdminCalendarMeta
	var components sql.NullString
	err := q.DB.QueryRow(`SELECT synctoken, components FROM calendars WHERE id = $1`,
		calendarID).Scan(&m.SyncToken, &components)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.Components = components.String
	if err := q.DB.QueryRow(
		`SELECT count(*), coalesce(sum(size), 0) FROM calendarobjects WHERE calendarid = $1`,
		calendarID).Scan(&m.ObjectCount, &m.TotalSize); err != nil {
		return nil, err
	}
	return &m, nil
}

// AdminPropRow is one propertystorage row for a collection path: a dead
// property, or a per-user overlay when OverlayPrincipal is set (the
// path + "/\n" + principal synthetic rows, ADMIN-PLAN.md §3.3).
type AdminPropRow struct {
	Path             string `json:"path"`
	Name             string `json:"name"`
	ValueType        int    `json:"valueType"`
	Value            string `json:"value"`
	SabreOnly        bool   `json:"sabreOnly"` // valuetype 3: PHP-serialized, sabre owns it
	OverlayPrincipal string `json:"overlayPrincipal"`
}

// PropertyRows returns the propertystorage rows for path: its global dead
// props plus every per-user overlay row.
func (q *AdminQueries) PropertyRows(path string) ([]AdminPropRow, error) {
	// Overlay rows are path + "/" + LF + principal; LF needs no LIKE
	// escaping, but the path itself does (escapeLike, backslash escapes).
	rows, err := q.DB.Query(`
SELECT path, name, coalesce(valuetype, 1), coalesce(value, ''::bytea)
  FROM propertystorage
 WHERE path = $1 OR path LIKE $2 ESCAPE '\'
 ORDER BY path, name`,
		path, escapeLike(path)+"/"+"\n"+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminPropRow
	for rows.Next() {
		var p AdminPropRow
		var value []byte
		if err := rows.Scan(&p.Path, &p.Name, &p.ValueType, &value); err != nil {
			return nil, err
		}
		p.SabreOnly = p.ValueType == vtObject
		if p.SabreOnly {
			p.Value = fmt.Sprintf("(PHP-serialized, %d bytes)", len(value))
		} else {
			p.Value = string(value)
		}
		if _, principal, found := strings.Cut(p.Path, "\n"); found {
			p.OverlayPrincipal = principal
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AdminAddressBook is one addressbooks row plus card aggregates.
type AdminAddressBook struct {
	ID           string `json:"id"`
	PrincipalURI string `json:"principalUri"`
	URI          string `json:"uri"`
	DisplayName  string `json:"displayName"`
	Description  string `json:"description"`
	SyncToken    int64  `json:"syncToken"`
	CardCount    int    `json:"cardCount"`
	TotalSize    int64  `json:"totalSize"`
}

// AddressBook returns one address book with card totals, or nil.
func (q *AdminQueries) AddressBook(id string) (*AdminAddressBook, error) {
	var b AdminAddressBook
	var principal, uri, displayname, description sql.NullString
	var rowID int64
	err := q.DB.QueryRow(`
SELECT id, principaluri, uri, displayname, description, synctoken
  FROM addressbooks WHERE id = $1`, id).Scan(
		&rowID, &principal, &uri, &displayname, &description, &b.SyncToken)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.ID = fmt.Sprint(rowID)
	b.PrincipalURI, b.URI = principal.String, uri.String
	b.DisplayName, b.Description = displayname.String, description.String
	if err := q.DB.QueryRow(
		`SELECT count(*), coalesce(sum(size), 0) FROM cards WHERE addressbookid = $1`,
		id).Scan(&b.CardCount, &b.TotalSize); err != nil {
		return nil, err
	}
	return &b, nil
}

// LookupAddressBook resolves (principal, book URI) to the addressbooks row
// id, or "".
func (q *AdminQueries) LookupAddressBook(principalURI, uri string) (string, error) {
	var id int64
	err := q.DB.QueryRow(
		`SELECT id FROM addressbooks WHERE principaluri = $1 AND uri = $2`,
		principalURI, uri).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprint(id), nil
}

// AdminObjectRow is one calendarobjects row's stored (denormalized)
// metadata; the browser diffs these against values recomputed from
// calendardata (ADMIN-PLAN.md §3.4).
type AdminObjectRow struct {
	URI             string `json:"uri"`
	UID             string `json:"uid"`
	ComponentType   string `json:"componentType"`
	ETag            string `json:"etag"`
	Size            int64  `json:"size"`
	LastModified    *int64 `json:"lastModified"`
	FirstOccurrence *int64 `json:"firstOccurrence"` // column firstoccurence (sabre's spelling)
	LastOccurrence  *int64 `json:"lastOccurrence"`  // column lastoccurence
}

// AdminObjectQuery scopes an object-browser search to one calendar
// (ADMIN-PLAN.md §3.4: never a whole-table scan).
type AdminObjectQuery struct {
	CalendarID string
	// Match is a case-insensitive substring tested against uri and uid —
	// and against calendardata when InData is set (summary search).
	Match  string
	InData bool
	// Start/End constrain the stored occurrence range (overlap test, like
	// the time-range prefilter).
	Start, End *int64
	Limit      int
}

// adminScanCap bounds how many rows an in-data search reads before giving
// up; the browser reports the truncation rather than scanning unbounded.
const adminScanCap = 5000

// Objects searches one calendar's objects. truncated is only reported when
// results are actually incomplete: the limit was reached with more matches
// (not merely more rows) behind it, or an in-data scan hit the scan cap.
// Unless InData asks for a payload search, filtering happens in SQL and
// calendardata never leaves the database.
func (q *AdminQueries) Objects(query AdminObjectQuery) (objects []AdminObjectRow, truncated bool, err error) {
	if query.Limit <= 0 || query.Limit > 500 {
		query.Limit = 100
	}
	where := "calendarid = $1"
	args := []any{query.CalendarID}
	// bigint, not the columns' int4: a bound past 2038 must compare, not
	// overflow (see CalDAV.CalendarQuery).
	if query.Start != nil {
		args = append(args, *query.Start)
		where += fmt.Sprintf(" AND (lastoccurence IS NULL OR lastoccurence > $%d::bigint)", len(args))
	}
	if query.End != nil {
		args = append(args, *query.End)
		where += fmt.Sprintf(" AND (firstoccurence IS NULL OR firstoccurence < $%d::bigint)", len(args))
	}

	deep := query.InData && query.Match != ""
	if !deep {
		if query.Match != "" {
			args = append(args, "%"+escapeLike(query.Match)+"%")
			where += fmt.Sprintf(" AND (uri ILIKE $%d OR uid ILIKE $%d)", len(args), len(args))
		}
		// One row past the limit answers "were there more matches".
		rows, err := q.DB.Query(fmt.Sprintf(`
SELECT uri, coalesce(uid, ''), coalesce(componenttype, ''), coalesce(etag, ''),
       size, lastmodified, firstoccurence, lastoccurence
  FROM calendarobjects WHERE %s ORDER BY id DESC LIMIT %d`, where, query.Limit+1), args...)
		if err != nil {
			return nil, false, err
		}
		defer rows.Close()
		for rows.Next() {
			var o AdminObjectRow
			var lastmod, first, last sql.NullInt64
			if err := rows.Scan(&o.URI, &o.UID, &o.ComponentType, &o.ETag, &o.Size,
				&lastmod, &first, &last); err != nil {
				return nil, false, err
			}
			setInt64(&o.LastModified, lastmod)
			setInt64(&o.FirstOccurrence, first)
			setInt64(&o.LastOccurrence, last)
			objects = append(objects, o)
		}
		if err := rows.Err(); err != nil {
			return nil, false, err
		}
		if len(objects) > query.Limit {
			return objects[:query.Limit], true, nil
		}
		return objects, false, nil
	}

	// In-data search: stream payloads with a scan cap. After the limit
	// fills, keep scanning until one more MATCH proves truncation — a
	// further non-matching row is not truncation. The SQL stops one row
	// past the cap, so the database never ships payloads the loop would
	// discard.
	args = append(args, adminScanCap+1)
	rows, err := q.DB.Query(fmt.Sprintf(`
SELECT uri, coalesce(uid, ''), coalesce(componenttype, ''), coalesce(etag, ''),
       size, lastmodified, firstoccurence, lastoccurence, calendardata
  FROM calendarobjects WHERE %s ORDER BY id DESC LIMIT $%d`, where, len(args)), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	needle := strings.ToLower(query.Match)
	scanned := 0
	for rows.Next() {
		if scanned >= adminScanCap {
			truncated = true
			break
		}
		scanned++
		var o AdminObjectRow
		var lastmod, first, last sql.NullInt64
		var data []byte
		if err := rows.Scan(&o.URI, &o.UID, &o.ComponentType, &o.ETag, &o.Size,
			&lastmod, &first, &last, &data); err != nil {
			return nil, false, err
		}
		hit := strings.Contains(strings.ToLower(o.URI), needle) ||
			strings.Contains(strings.ToLower(o.UID), needle) ||
			strings.Contains(strings.ToLower(string(data)), needle)
		if !hit {
			continue
		}
		if len(objects) >= query.Limit {
			truncated = true
			break
		}
		setInt64(&o.LastModified, lastmod)
		setInt64(&o.FirstOccurrence, first)
		setInt64(&o.LastOccurrence, last)
		objects = append(objects, o)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return objects, truncated, nil
}

func setInt64(dst **int64, v sql.NullInt64) {
	if v.Valid {
		*dst = &v.Int64
	}
}

// AdminObjectDetail is one object's stored row plus its raw payload; the
// admin app recomputes the denormalized fields from Data and diffs them.
type AdminObjectDetail struct {
	Row  AdminObjectRow `json:"row"`
	Data []byte         `json:"data"`
}

// Object fetches one calendar object with its stored metadata, or nil.
func (q *AdminQueries) Object(calendarID, uri string) (*AdminObjectDetail, error) {
	var d AdminObjectDetail
	var lastmod, first, last sql.NullInt64
	err := q.DB.QueryRow(`
SELECT uri, coalesce(uid, ''), coalesce(componenttype, ''), coalesce(etag, ''),
       size, lastmodified, firstoccurence, lastoccurence, calendardata
  FROM calendarobjects WHERE calendarid = $1 AND uri = $2`, calendarID, uri).Scan(
		&d.Row.URI, &d.Row.UID, &d.Row.ComponentType, &d.Row.ETag, &d.Row.Size,
		&lastmod, &first, &last, &d.Data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastmod.Valid {
		d.Row.LastModified = &lastmod.Int64
	}
	if first.Valid {
		d.Row.FirstOccurrence = &first.Int64
	}
	if last.Valid {
		d.Row.LastOccurrence = &last.Int64
	}
	return &d, nil
}

// AdminCardRow is one cards row.
type AdminCardRow struct {
	URI          string `json:"uri"`
	ETag         string `json:"etag"`
	Size         int64  `json:"size"`
	LastModified *int64 `json:"lastModified"`
}

// CardsSearch lists one address book's cards, optionally filtered by a
// case-insensitive substring over uri (and carddata when inData is set).
// Same contract as Objects: SQL-side filtering without payload transfer
// unless inData, and truncated only when matches were actually cut off.
func (q *AdminQueries) CardsSearch(bookID, match string, inData bool, limit int) (cards []AdminCardRow, truncated bool, err error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	deep := inData && match != ""
	if !deep {
		where := "addressbookid = $1"
		args := []any{bookID}
		if match != "" {
			args = append(args, "%"+escapeLike(match)+"%")
			where += fmt.Sprintf(" AND uri ILIKE $%d", len(args))
		}
		rows, err := q.DB.Query(fmt.Sprintf(`
SELECT uri, coalesce(etag, ''), size, lastmodified
  FROM cards WHERE %s ORDER BY id DESC LIMIT %d`, where, limit+1), args...)
		if err != nil {
			return nil, false, err
		}
		defer rows.Close()
		for rows.Next() {
			var c AdminCardRow
			var lastmod sql.NullInt64
			if err := rows.Scan(&c.URI, &c.ETag, &c.Size, &lastmod); err != nil {
				return nil, false, err
			}
			setInt64(&c.LastModified, lastmod)
			cards = append(cards, c)
		}
		if err := rows.Err(); err != nil {
			return nil, false, err
		}
		if len(cards) > limit {
			return cards[:limit], true, nil
		}
		return cards, false, nil
	}

	rows, err := q.DB.Query(`
SELECT uri, coalesce(etag, ''), size, lastmodified, carddata
  FROM cards WHERE addressbookid = $1 ORDER BY id DESC LIMIT $2`, bookID, adminScanCap+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	needle := strings.ToLower(match)
	scanned := 0
	for rows.Next() {
		if scanned >= adminScanCap {
			truncated = true
			break
		}
		scanned++
		var c AdminCardRow
		var lastmod sql.NullInt64
		var data []byte
		if err := rows.Scan(&c.URI, &c.ETag, &c.Size, &lastmod, &data); err != nil {
			return nil, false, err
		}
		hit := strings.Contains(strings.ToLower(c.URI), needle) ||
			strings.Contains(strings.ToLower(string(data)), needle)
		if !hit {
			continue
		}
		if len(cards) >= limit {
			truncated = true
			break
		}
		setInt64(&c.LastModified, lastmod)
		cards = append(cards, c)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return cards, truncated, nil
}

// AdminCardDetail is one card's stored row plus payload.
type AdminCardDetail struct {
	Row  AdminCardRow `json:"row"`
	Data []byte       `json:"data"`
}

// Card fetches one card with its stored metadata, or nil.
func (q *AdminQueries) Card(bookID, uri string) (*AdminCardDetail, error) {
	var d AdminCardDetail
	var lastmod sql.NullInt64
	err := q.DB.QueryRow(`
SELECT uri, coalesce(etag, ''), size, lastmodified, carddata
  FROM cards WHERE addressbookid = $1 AND uri = $2`, bookID, uri).Scan(
		&d.Row.URI, &d.Row.ETag, &d.Row.Size, &lastmod, &d.Data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastmod.Valid {
		d.Row.LastModified = &lastmod.Int64
	}
	return &d, nil
}
