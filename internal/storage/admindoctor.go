// Doctor: read-only invariant checks over the shared schema
// (ADMIN-PLAN.md §3.7), plus the per-collection change log and pending
// share invites the scheduling/sync debugger needs (§3.6).
package storage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/emersion/go-vcard"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/migadu/calendsd/internal/caldav/ical"
)

// ChangeLog returns the change rows of one collection, newest first.
// kind is "calendar" or "addressbook".
func (q *AdminQueries) ChangeLog(ctx context.Context, kind, collectionID string, limit int) ([]AdminChange, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	table, join, joinOn := "calendarchanges", "calendarobjects", "calendarid"
	if kind == "addressbook" {
		table, join, joinOn = "addressbookchanges", "cards", "addressbookid"
	} else if kind != "calendar" {
		return nil, fmt.Errorf("unknown change-log kind %q", kind)
	}
	rows, err := q.DB.QueryContext(ctx, fmt.Sprintf(`
SELECT c.uri, c.synctoken, c.operation, o.lastmodified
  FROM %s c
  LEFT JOIN %s o ON o.%s = c.%s AND o.uri = c.uri
 WHERE c.%s = $1
 ORDER BY c.synctoken DESC, c.id DESC LIMIT $2`,
		table, join, joinOn, joinOn, joinOn), collectionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminChange
	for rows.Next() {
		ch := AdminChange{Kind: kind, CollectionID: collectionID}
		var lastmod sql.NullInt64
		if err := rows.Scan(&ch.ObjectURI, &ch.SyncToken, &ch.Operation, &lastmod); err != nil {
			return nil, err
		}
		if lastmod.Valid {
			ch.LastModified = &lastmod.Int64
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

// PendingShareInvites lists one principal's calendar instances whose
// sharee has not responded (share_invitestatus = 1). The shared sabre
// schema stores no notification objects, so this is the DB truth for
// "pending invites".
func (q *AdminQueries) PendingShareInvites(ctx context.Context, principalURI string) ([]AdminCalendarInstance, error) {
	rows, err := q.DB.QueryContext(ctx, `
SELECT id, calendarid, coalesce(principaluri, ''), coalesce(uri, ''), access,
       coalesce(displayname, ''), coalesce(share_href, ''),
       coalesce(share_displayname, ''), share_invitestatus
  FROM calendarinstances
 WHERE share_invitestatus = 1 AND principaluri = $1 ORDER BY uri`, principalURI)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminCalendarInstance
	for rows.Next() {
		var in AdminCalendarInstance
		var instanceID, calID int64
		if err := rows.Scan(&instanceID, &calID, &in.PrincipalURI, &in.URI,
			&in.Access, &in.DisplayName, &in.ShareHref, &in.ShareDisplayName,
			&in.InviteStatus); err != nil {
			return nil, err
		}
		in.InstanceID = fmt.Sprint(instanceID)
		in.CalendarID = fmt.Sprint(calID)
		out = append(out, in)
	}
	return out, rows.Err()
}

// AdminFinding is one doctor result, linked to its inspector by the
// subject's identifiers.
type AdminFinding struct {
	// Check names the invariant, e.g. "orphaned-instance".
	Check   string `json:"check"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
}

// AdminDoctorReport is the full doctor run.
type AdminDoctorReport struct {
	Findings []AdminFinding `json:"findings"`
	// ObjectsScanned/CardsScanned count the deep-scan coverage; Truncated
	// reports that the scan cap cut it short (no silent partial results).
	ObjectsScanned int  `json:"objectsScanned"`
	CardsScanned   int  `json:"cardsScanned"`
	Truncated      bool `json:"truncated"`
}

// Doctor runs every §3.7 invariant check. scanCap bounds the expensive
// parse-every-object checks; <=0 means the default (adminScanCap).
func (q *AdminQueries) Doctor(ctx context.Context, scanCap int) (*AdminDoctorReport, error) {
	if scanCap <= 0 {
		scanCap = adminScanCap
	}
	report := &AdminDoctorReport{}

	// Referential orphans, each a plain anti-join.
	type orphanCheck struct {
		check, detail, query string
	}
	for _, c := range []orphanCheck{
		{"orphaned-instance", "calendarinstances row whose calendars row is gone",
			`SELECT 'instance id=' || ci.id || ' (' || coalesce(ci.principaluri,'?') || ' ' || coalesce(ci.uri,'?') || ', calendarid=' || ci.calendarid || ')'
			   FROM calendarinstances ci LEFT JOIN calendars c ON c.id = ci.calendarid WHERE c.id IS NULL`},
		{"orphaned-object", "calendarobjects row whose calendars row is gone",
			`SELECT 'object id=' || o.id || ' (' || coalesce(o.uri,'?') || ', calendarid=' || o.calendarid || ')'
			   FROM calendarobjects o LEFT JOIN calendars c ON c.id = o.calendarid WHERE c.id IS NULL`},
		{"calendar-without-instance", "calendars row no principal can reach",
			`SELECT 'calendar id=' || c.id
			   FROM calendars c LEFT JOIN calendarinstances ci ON ci.calendarid = c.id WHERE ci.id IS NULL`},
		{"orphaned-card", "cards row whose addressbooks row is gone",
			`SELECT 'card id=' || cd.id || ' (' || coalesce(cd.uri,'?') || ', addressbookid=' || cd.addressbookid || ')'
			   FROM cards cd LEFT JOIN addressbooks ab ON ab.id = cd.addressbookid WHERE ab.id IS NULL`},
		{"dangling-groupmember", "groupmembers row referencing a deleted principal",
			`SELECT 'groupmembers id=' || gm.id || ' (principal_id=' || gm.principal_id || ', member_id=' || gm.member_id || ')'
			   FROM groupmembers gm
			   LEFT JOIN principals pg ON pg.id = gm.principal_id
			   LEFT JOIN principals pm ON pm.id = gm.member_id
			  WHERE pg.id IS NULL OR pm.id IS NULL`},
		{"orphaned-scheduling-object", "schedulingobjects row for a nonexistent principal",
			`SELECT 'scheduling object id=' || s.id || ' (' || coalesce(s.uri,'?') || ', ' || coalesce(s.principaluri,'?') || ')'
			   FROM schedulingobjects s
			  WHERE s.principaluri IS NOT NULL
			    AND NOT EXISTS (SELECT 1 FROM principals p WHERE p.uri = s.principaluri)`},
		{"orphaned-availability", "calendar_availability row for a nonexistent principal",
			`SELECT 'availability of ' || ca.principal
			   FROM calendar_availability ca
			  WHERE NOT EXISTS (SELECT 1 FROM principals p WHERE p.uri = ca.principal)`},
		{"orphaned-visibility", "free_busy_visibility row for a nonexistent principal",
			`SELECT 'free-busy visibility of ' || v.principal
			   FROM free_busy_visibility v
			  WHERE NOT EXISTS (SELECT 1 FROM principals p WHERE p.uri = v.principal)`},
		{"orphaned-notification", "notifications row for a nonexistent recipient or sharer principal",
			`SELECT 'notification id=' || n.id || ' (' || n.principaluri || ' from ' || n.organizer || ')'
			   FROM notifications n
			  WHERE NOT EXISTS (SELECT 1 FROM principals p WHERE p.uri = n.principaluri)
			     OR NOT EXISTS (SELECT 1 FROM principals p WHERE p.uri = n.organizer)`},
		{"orphaned-push-subscription", "push_subscriptions row for a nonexistent principal or collection",
			`SELECT 'push subscription id=' || s.id || ' (' || s.principal_uri || ' on ' || s.collection_kind || ' ' || s.collection_id || ')'
			   FROM push_subscriptions s
			  WHERE NOT EXISTS (SELECT 1 FROM principals p WHERE p.uri = s.principal_uri)
			     OR (s.collection_kind = 'calendar' AND NOT EXISTS (SELECT 1 FROM calendars c WHERE c.id::text = s.collection_id))
			     OR (s.collection_kind = 'addressbook' AND NOT EXISTS (SELECT 1 FROM addressbooks a WHERE a.id::text = s.collection_id))`},
	} {
		rows, err := q.DB.QueryContext(ctx, c.query)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.check, err)
		}
		for rows.Next() {
			var subject string
			if err := rows.Scan(&subject); err != nil {
				rows.Close()
				return nil, err
			}
			report.Findings = append(report.Findings, AdminFinding{
				Check: c.check, Subject: subject, Detail: c.detail})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	// Principal/user mismatches (§3.2), folded into the same report.
	diags, err := q.AuthDiagnostics(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range diags.PrincipalsWithoutUser {
		report.Findings = append(report.Findings, AdminFinding{
			Check: "principal-without-user", Subject: p,
			Detail: "no users row — this principal cannot log in"})
	}
	for _, u := range diags.UsersWithoutPrincipal {
		report.Findings = append(report.Findings, AdminFinding{
			Check: "user-without-principal", Subject: u,
			Detail: "users row with no principal — logins succeed but no home exists"})
	}
	for _, u := range diags.MalformedDigestA1 {
		report.Findings = append(report.Findings, AdminFinding{
			Check: "malformed-digesta1", Subject: u,
			Detail: "digesta1 is not a 32-char md5 hex — auth always fails"})
	}

	if err := q.doctorPropertyPaths(ctx, report); err != nil {
		return nil, err
	}
	if err := q.doctorObjects(ctx, report, scanCap); err != nil {
		return nil, err
	}
	if err := q.doctorCards(ctx, report, scanCap); err != nil {
		return nil, err
	}
	return report, nil
}

// orphanedPropRow is one propertystorage row whose target no longer exists.
type orphanedPropRow struct {
	id     int64
	path   string
	reason string
}

// orphanedPropertyRows classifies propertystorage rows whose target path no
// longer exists. Only the known conventions are judged; unknown roots are
// left alone rather than guessed at. Shared by the doctor (reporting) and
// AdminWrites.DeleteOrphans (deleting) so they always agree. Four fixed
// queries total: the rows plus three reference sets loaded once, so the
// check stays flat no matter how many property rows exist. q is the pool for
// the doctor and the deleting transaction for DeleteOrphans.
func orphanedPropertyRows(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}) ([]orphanedPropRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, path FROM propertystorage ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []orphanedPropRow
	for rows.Next() {
		var p orphanedPropRow
		if err := rows.Scan(&p.id, &p.path); err != nil {
			return nil, err
		}
		all = append(all, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}

	loadSet := func(query string) (map[string]bool, error) {
		rows, err := q.QueryContext(ctx, query)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		set := map[string]bool{}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return nil, err
			}
			set[s] = true
		}
		return set, rows.Err()
	}
	principals, err := loadSet(`SELECT uri FROM principals`)
	if err != nil {
		return nil, err
	}
	// Collections keyed "principals/<name>\x01<uri>" (0x01 cannot appear
	// in either part).
	calendarInstances, err := loadSet(
		`SELECT coalesce(principaluri, '') || chr(1) || coalesce(uri, '') FROM calendarinstances`)
	if err != nil {
		return nil, err
	}
	addressBooks, err := loadSet(
		`SELECT coalesce(principaluri, '') || chr(1) || coalesce(uri, '') FROM addressbooks`)
	if err != nil {
		return nil, err
	}
	pair := func(principal, uri string) string { return principal + "\x01" + uri }

	var orphans []orphanedPropRow
	for _, p := range all {
		// Per-user overlay rows target path-before-"/\n" for a principal.
		target, overlayPrincipal, isOverlay := strings.Cut(p.path, "\n")
		target = strings.TrimSuffix(target, "/")
		if isOverlay && !principals[overlayPrincipal] {
			p.reason = "per-user overlay for a nonexistent principal " + overlayPrincipal
			orphans = append(orphans, p)
			continue
		}
		parts := strings.Split(target, "/")
		var ok bool
		switch {
		case parts[0] == "principals":
			ok = principals[target]
		case parts[0] == "calendars" && len(parts) == 2:
			ok = principals["principals/"+parts[1]]
		case parts[0] == "calendars" && len(parts) == 3 && (parts[2] == "inbox" || parts[2] == "outbox"):
			// The schedule inbox/outbox exist for as long as the principal
			// does; they are not calendarinstances rows. The inbox carries
			// calendar-availability (VAVAILABILITY-PLAN.md §3.4).
			ok = principals["principals/"+parts[1]]
		case parts[0] == "calendars" && len(parts) == 3:
			ok = calendarInstances[pair("principals/"+parts[1], parts[2])]
		case parts[0] == "addressbooks" && len(parts) == 2:
			ok = principals["principals/"+parts[1]]
		case parts[0] == "addressbooks" && len(parts) == 3:
			ok = addressBooks[pair("principals/"+parts[1], parts[2])]
		default:
			continue // unknown convention: not judged
		}
		if !ok {
			p.reason = "no collection or principal exists at this path"
			orphans = append(orphans, p)
		}
	}
	return orphans, nil
}

func (q *AdminQueries) doctorPropertyPaths(ctx context.Context, report *AdminDoctorReport) error {
	orphans, err := orphanedPropertyRows(ctx, q.DB)
	if err != nil {
		return err
	}
	for _, o := range orphans {
		report.Findings = append(report.Findings, AdminFinding{
			Check:   "orphaned-property",
			Subject: fmt.Sprintf("propertystorage id=%d path=%q", o.id, o.path),
			Detail:  o.reason})
	}
	return nil
}

// doctorObjects re-derives every calendarobjects row's denormalized columns
// with the production ical.Denormalize and reports parse failures and stale
// columns (§3.4 as a sweep). The query stops one row past the cap, which is
// enough to report truncation: closing a cursor mid-way still drains the rest
// of the table over the wire.
func (q *AdminQueries) doctorObjects(ctx context.Context, report *AdminDoctorReport, scanCap int) error {
	rows, err := q.DB.QueryContext(ctx, `
SELECT o.id, o.calendarid, o.uri, coalesce(o.uid, ''), coalesce(o.componenttype, ''),
       coalesce(o.etag, ''), o.size, o.firstoccurence, o.lastoccurence, o.calendardata
  FROM calendarobjects o ORDER BY o.id LIMIT $1`, scanCap+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	// Objects carrying a pending iMIP SCHEDULE-STATUS (1.0), collected during the
	// scan as {uid, subject} and cross-referenced against live delivery jobs
	// afterwards (a fresh pending send has a job and is not stale).
	var pending [][2]string
	for rows.Next() {
		if report.ObjectsScanned >= scanCap {
			report.Truncated = true
			break
		}
		report.ObjectsScanned++
		var (
			id, calID   int64
			uri         string
			row         AdminObjectRow
			first, last sql.NullInt64
			data        []byte
		)
		if err := rows.Scan(&id, &calID, &uri, &row.UID, &row.ComponentType,
			&row.ETag, &row.Size, &first, &last, &data); err != nil {
			return err
		}
		if first.Valid {
			row.FirstOccurrence = &first.Int64
		}
		if last.Valid {
			row.LastOccurrence = &last.Int64
		}
		subject := fmt.Sprintf("object %s (calendar %d)", uri, calID)
		den, err := ical.Denormalize(data)
		if err != nil {
			report.Findings = append(report.Findings, AdminFinding{
				Check: "unparseable-calendardata", Subject: subject, Detail: err.Error()})
			continue
		}
		var stale []string
		if row.ETag != den.ETag {
			stale = append(stale, "etag")
		}
		if row.Size != den.Size {
			stale = append(stale, "size")
		}
		if row.ComponentType != den.ComponentType {
			stale = append(stale, "componenttype")
		}
		if row.UID != den.UID {
			stale = append(stale, "uid")
		}
		if !equalNullableInt64(row.FirstOccurrence, den.FirstOccurrence) {
			stale = append(stale, "firstoccurence")
		}
		if !equalNullableInt64(row.LastOccurrence, den.LastOccurrence) {
			stale = append(stale, "lastoccurence")
		}
		if len(stale) > 0 {
			report.Findings = append(report.Findings, AdminFinding{
				Check: "stale-denormalized-columns", Subject: subject,
				Detail: "stored columns disagree with calendardata: " + strings.Join(stale, ", ")})
		}
		if bytes.Contains(data, []byte(imipPendingMarker)) {
			pending = append(pending, [2]string{den.UID, subject})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// A pending object with no live imip_send job is stale — its send was lost
	// (a write-back that never landed, a manually removed job). Objects still
	// awaiting a queued send have a job and are skipped.
	if len(pending) > 0 {
		return q.flagStaleIMipPending(ctx, report, pending)
	}
	return nil
}

// imipPendingMarker is the human-readable half of the pending SCHEDULE-STATUS
// (caldav.statusQueued), stable enough to grep for in stored calendar data.
const imipPendingMarker = "Scheduling message is queued for delivery"

// flagStaleIMipPending reports each pending object whose UID has no live
// (available|executing) imip_send job. When the jobs table is absent (a
// sabre-only or queue-less database) it skips silently — pending-at-1.0 can't be
// reconciled against a queue that doesn't exist. Any other failure is an
// error: reporting every pending object as stale, or none, off a half-read
// job list would be wrong either way.
func (q *AdminQueries) flagStaleIMipPending(ctx context.Context, report *AdminDoctorReport, pending [][2]string) error {
	rows, err := q.DB.QueryContext(ctx, `SELECT DISTINCT coalesce(args->>'uid','')
	  FROM jobs WHERE worker='imip_send' AND state IN ('available','executing')`)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
		return nil
	}
	if err != nil {
		return fmt.Errorf("imip-pending-stale: %w", err)
	}
	defer rows.Close()
	live := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return err
		}
		live[u] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range pending {
		if !live[p[0]] {
			report.Findings = append(report.Findings, AdminFinding{
				Check:   "imip-pending-stale",
				Subject: p[1],
				Detail:  "attendee stuck at SCHEDULE-STATUS 1.0 with no delivery job — the send was lost; resend from the organizer's client or via `calctl imip resend`",
			})
		}
	}
	return nil
}

// doctorCards checks every cards row: parseable vCard, correct etag/size.
// Capped in SQL like doctorObjects.
func (q *AdminQueries) doctorCards(ctx context.Context, report *AdminDoctorReport, scanCap int) error {
	rows, err := q.DB.QueryContext(ctx, `
SELECT c.id, c.addressbookid, c.uri, coalesce(c.etag, ''), c.size, c.carddata
  FROM cards c ORDER BY c.id LIMIT $1`, scanCap+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if report.CardsScanned >= scanCap {
			report.Truncated = true
			break
		}
		report.CardsScanned++
		var (
			id, bookID int64
			uri, etag  string
			size       int64
			data       []byte
		)
		if err := rows.Scan(&id, &bookID, &uri, &etag, &size, &data); err != nil {
			return err
		}
		subject := fmt.Sprintf("card %s (address book %d)", uri, bookID)
		if _, err := vcard.NewDecoder(bytes.NewReader(data)).Decode(); err != nil {
			report.Findings = append(report.Findings, AdminFinding{
				Check: "unparseable-carddata", Subject: subject, Detail: err.Error()})
			continue
		}
		var stale []string
		if etag != cardETag(data) {
			stale = append(stale, "etag")
		}
		if size != int64(len(data)) {
			stale = append(stale, "size")
		}
		if len(stale) > 0 {
			report.Findings = append(report.Findings, AdminFinding{
				Check: "stale-denormalized-columns", Subject: subject,
				Detail: "stored columns disagree with carddata: " + strings.Join(stale, ", ")})
		}
	}
	return rows.Err()
}

func equalNullableInt64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
