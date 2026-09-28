package storage

// Calendar sharing over the sabre schema: the SharingSupport port of
// CalDAV\Backend\PDO's updateInvites/getInvites. A share lives entirely in
// calendarinstances — the owner's row (access=1) plus one row per sharee,
// each cloned from the owner's instance and carrying its own access level,
// share_href (the invitee's calendar-user-address), and share_invitestatus.
// The values written here must be readable by a live sabre on the same DB.

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"calserver/internal/caldav"
)

// CalDAV satisfies the CalDAV sharing backends so the SharingPlugin's
// {cs}share / invite-reply / publish POSTs mutate real state, and the
// notification inbox is served from Postgres. SharingSupport lives on the
// sabre calendarinstances table; the notification inbox and publish state use
// the additive calserver extension tables (scripts/schema/pgsql.calserver.sql)
// that a live sabre on the same database never touches.
var (
	_ caldav.SharingSupport       = (*CalDAV)(nil)
	_ caldav.ReplySupport         = (*CalDAV)(nil)
	_ caldav.PublishSupport       = (*CalDAV)(nil)
	_ caldav.NotificationsBackend = (*CalDAV)(nil)
	_ caldav.ShareLookup          = (*CalDAV)(nil)
)

// Invites returns the sharees of a calendar — the owner plus every principal
// it has been shared with. Port of sabre's PDO::getInvites: the invite list
// is exactly the set of calendarinstances rows for the shared calendarid.
func (b *CalDAV) Invites(id caldav.CalendarID) ([]caldav.Sharee, error) {
	rows, err := b.DB.Query(`
SELECT principaluri, access, share_href, share_displayname, share_invitestatus
  FROM calendarinstances WHERE calendarid = $1 ORDER BY id`, id.Calendar)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []caldav.Sharee
	for rows.Next() {
		var (
			principal, shareHref, shareDisplayName sql.NullString
			access, inviteStatus                   int
		)
		if err := rows.Scan(&principal, &access, &shareHref, &shareDisplayName, &inviteStatus); err != nil {
			return nil, err
		}
		// sabre falls back to encodePath(principaluri) when no share_href is
		// stored (the owner's own row); ASCII principal URIs pass through.
		href := shareHref.String
		if href == "" {
			href = principal.String
		}
		out = append(out, caldav.Sharee{
			Href:         href,
			Principal:    principal.String,
			CommonName:   shareDisplayName.String,
			Access:       access,
			InviteStatus: inviteStatus,
		})
	}
	return out, rows.Err()
}

// UpdateInvites applies a set of sharee changes to a calendar. Port of sabre's
// PDO::updateInvites: a NOACCESS entry deletes the matching sharee instance; a
// known sharee's instance is updated in place; a new one clones the owner's
// instance into a fresh per-sharee row. Because there is no invitation UI, a
// resolved principal is auto-accepted and an unresolved one is recorded as
// invalid, exactly as sabre does.
//
// Unlike sabre it is serialized per calendar and reports what it committed
// (docs/share-invite-email.md): the owner's instance row is locked FOR UPDATE
// before the roster is read, so two concurrent share requests for the same
// calendar apply one after the other and the second sees the first's result.
// The request is first reduced to one final entry per sharee (identified by
// principal, or by address while unresolved; the last entry wins, so a remove
// beats a set in the same POST), which is what makes a re-POSTed roster — or
// the same person under two aliases — report no new share.
func (b *CalDAV) UpdateInvites(id caldav.CalendarID, sharees []caldav.Sharee) ([]caldav.ShareChange, error) {
	var changes []caldav.ShareChange
	err := withTx(b.DB, func(tx *sql.Tx) error {
		owner, err := lockOwnerInstance(tx, id.Calendar)
		if err != nil {
			return err
		}
		current, err := shareeRows(tx, id.Calendar)
		if err != nil {
			return err
		}
		changes, err = applyShareeChanges(tx, id, caldav.FinalSharees(sharees, owner), current)
		return err
	})
	if err != nil {
		return nil, err
	}
	return changes, nil
}

// lockOwnerInstance takes the per-calendar sharing lock — a row lock on the
// owner's (access=1) instance — and returns the owner principal. Every sharing
// mutation (UpdateInvites, ShareReply) takes it first, so they never
// interleave. A co-resident sabre does not follow this protocol.
func lockOwnerInstance(tx *sql.Tx, calendarID string) (string, error) {
	var owner sql.NullString
	err := tx.QueryRow(
		`SELECT principaluri FROM calendarinstances WHERE calendarid = $1 AND access = 1 FOR UPDATE`,
		calendarID).Scan(&owner)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("sharing: calendar %s has no owner instance", calendarID)
	}
	return owner.String, err
}

// shareeRow is one existing sharee instance (access 2/3) as the sharing
// transaction sees it.
type shareeRow struct {
	id          int64
	uri         string
	principal   string // "" for an unresolved (invalid) invite
	href        string
	displayName string
}

func shareeRows(tx *sql.Tx, calendarID string) ([]shareeRow, error) {
	rows, err := tx.Query(`
SELECT id, uri, principaluri, share_href, share_displayname
  FROM calendarinstances WHERE calendarid = $1 AND access IN (2, 3) ORDER BY id`, calendarID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []shareeRow
	for rows.Next() {
		var r shareeRow
		var principal, href, name sql.NullString
		if err := rows.Scan(&r.id, &r.uri, &principal, &href, &name); err != nil {
			return nil, err
		}
		r.principal, r.href, r.displayName = principal.String, href.String, name.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// matchSharee finds the existing instance a final request entry refers to: by
// principal for a resolved sharee, falling back to an unresolved row with the
// same address (an invite that now resolves). An unresolved entry matches by
// address — an unresolved row first, else any row stored under it (an address
// that no longer resolves still names the row it was shared under, as in
// sabre, rather than adding a second one).
func matchSharee(current []shareeRow, s caldav.Sharee) *shareeRow {
	if s.Principal != "" {
		for i := range current {
			if current[i].principal == s.Principal {
				return &current[i]
			}
		}
	}
	for i := range current {
		if current[i].principal == "" && strings.EqualFold(current[i].href, s.Href) {
			return &current[i]
		}
	}
	if s.Principal == "" {
		for i := range current {
			if strings.EqualFold(current[i].href, s.Href) {
				return &current[i]
			}
		}
	}
	return nil
}

// applyShareeChanges applies the already-normalized final entries inside the
// sharing transaction and returns one ShareChange per sharee instance left in
// place.
func applyShareeChanges(tx *sql.Tx, id caldav.CalendarID, final []caldav.Sharee, current []shareeRow) ([]caldav.ShareChange, error) {
	var changes []caldav.ShareChange
	// Rows a set in this request granted or updated. A removal never deletes
	// them: re-sharing someone under a new address while removing a stale
	// alias of theirs in the same POST must leave the share they were just
	// granted (the stale alias still names their row).
	granted := map[int64]bool{}
	for _, sharee := range final {
		match := matchSharee(current, sharee)

		if sharee.Access == caldav.AccessNoAccess {
			// Removal: only sharee rows (access 2/3) match, so the owner's
			// own row can never be dropped by a share request. An unresolved
			// address also clears any other pre-existing row stored under it
			// (sabre removes by share_href).
			var drop []int64
			if match != nil && !granted[match.id] {
				drop = append(drop, match.id)
			}
			if sharee.Principal == "" {
				for _, r := range current {
					if strings.EqualFold(r.href, sharee.Href) && !granted[r.id] && (match == nil || r.id != match.id) {
						drop = append(drop, r.id)
					}
				}
			}
			for _, rowID := range drop {
				if _, err := tx.Exec(
					`DELETE FROM calendarinstances WHERE id = $1 AND access IN (2, 3)`, rowID); err != nil {
					return nil, err
				}
			}
			continue
		}

		// No invitation system yet: a resolved principal is accepted outright,
		// an unresolved one (no local principal) is marked invalid.
		status := caldav.InviteAccepted
		var principal any // NULL for an unresolved sharee
		if sharee.Principal == "" {
			status = caldav.InviteInvalid
		} else {
			principal = sharee.Principal
		}

		if match != nil {
			// Update in place. Keep the prior display name when the request
			// omits a common-name (sabre merges the old properties in). An
			// unresolved row that now resolves is bound to its principal; a
			// bound row matched by an address that no longer resolves keeps
			// its principal (only its status becomes invalid, as in sabre).
			displayName := sharee.CommonName
			if displayName == "" {
				displayName = match.displayName
			}
			res, err := tx.Exec(`
UPDATE calendarinstances
   SET access = $1, share_displayname = $2, share_invitestatus = $3, principaluri = COALESCE($4, principaluri)
 WHERE id = $5 AND access IN (2, 3)`,
				sharee.Access, nullIfEmpty(displayName), status, principal, match.id)
			if err != nil {
				return nil, err
			}
			if n, err := res.RowsAffected(); err != nil {
				return nil, err
			} else if n != 1 {
				return nil, fmt.Errorf("sharing: sharee instance %d vanished under the calendar lock", match.id)
			}
			granted[match.id] = true
			changes = append(changes, caldav.ShareChange{
				Href: sharee.Href, Principal: sharee.Principal,
				InstanceID: match.id, InstanceURI: match.uri,
				Access: sharee.Access, Comment: sharee.Comment,
				Created: match.principal == "" && sharee.Principal != "",
			})
			continue
		}

		// New sharee: clone the owner's instance (its displayname, description,
		// order, color, timezone) into a fresh row keyed to the sharee.
		uri, err := newUUID()
		if err != nil {
			return nil, err
		}
		var instanceID int64
		var instanceURI string
		if err := tx.QueryRow(`
INSERT INTO calendarinstances
    (calendarid, principaluri, access, displayname, uri, description,
     calendarorder, calendarcolor, timezone, transparent,
     share_href, share_displayname, share_invitestatus)
SELECT $1, $2, $3, displayname, $4, description,
       calendarorder, calendarcolor, timezone, 1,
       $5, $6, $7
  FROM calendarinstances WHERE id = $8
RETURNING id, uri`,
			id.Calendar, principal, sharee.Access, uri, sharee.Href,
			nullIfEmpty(sharee.CommonName), status, id.Instance).Scan(&instanceID, &instanceURI); err != nil {
			return nil, err
		}
		granted[instanceID] = true
		changes = append(changes, caldav.ShareChange{
			Href: sharee.Href, Principal: sharee.Principal,
			InstanceID: instanceID, InstanceURI: instanceURI,
			Access: sharee.Access, Comment: sharee.Comment, Created: true,
		})
	}
	return changes, nil
}

// SharedInstance returns principal's current instance of a shared calendar,
// with the owner and the owner's name for the calendar, or nil when they hold
// none. The share-notice worker revalidates against it before every send.
func (b *CalDAV) SharedInstance(calendarID, principal string) (*caldav.SharedInstance, error) {
	var (
		inst  caldav.SharedInstance
		owner sql.NullString
	)
	err := b.DB.QueryRow(`
SELECT s.id, s.uri, s.access, o.principaluri, COALESCE(NULLIF(o.displayname, ''), o.uri)
  FROM calendarinstances s
  JOIN calendarinstances o ON o.calendarid = s.calendarid AND o.access = 1
 WHERE s.calendarid = $1 AND s.principaluri = $2 AND s.access IN (2, 3)`,
		calendarID, principal).Scan(&inst.ID, &inst.URI, &inst.Access, &owner, &inst.CalendarName)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	inst.OwnerPrincipal = owner.String
	return &inst, nil
}

// nullIfEmpty maps an empty string to a SQL NULL so cleared display names
// round-trip as NULL rather than "".
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// newUUID returns a random RFC 4122 v4 UUID, matching the identifiers sabre's
// UUIDUtil writes into a new instance's uri column.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ShareReply records a sharee's accept/decline of a share invite. Because
// UpdateInvites auto-accepts resolvable invites, the sharee's instance already
// exists; a reply simply confirms it (marking share_invitestatus accepted and
// returning the sharee's calendar path) or, on decline, drops the sharee's own
// instance row — exactly what a sharee deleting their shared calendar does.
// It touches only the sharee's own instance and the sabre-owned
// share_invitestatus column, so a live sabre reads the result unchanged.
//
// href is the invite's hosturl: the owner's calendar path (…/calendars/<owner>/<uri>).
func (b *CalDAV) ShareReply(principalURI, href string, status int, calendarURI, inReplyTo, summary string) (string, error) {
	ownerPrincipal, ownerURI, ok := ownerCalendarFromHref(href)
	if !ok {
		return "", nil
	}
	// out stays "" unless the reply is an accept; the calendar segment is
	// returned relative to the replying principal's home (the invite-reply
	// request target). A leading-slash absolute path would be passed through
	// verbatim by resolveHref and drop a non-root BaseURI; a request-relative
	// segment inherits the request's prefix instead.
	var out string
	err := withTx(b.DB, func(tx *sql.Tx) error {
		// Resolve the shared calendar from the owner's own (access=1) instance,
		// then the replying principal's instance of that calendar.
		// Take the per-calendar sharing lock (the owner's instance row, as
		// UpdateInvites does) before touching the sharee's instance.
		var calendarID int64
		err := tx.QueryRow(
			`SELECT calendarid FROM calendarinstances WHERE principaluri = $1 AND uri = $2 AND access = 1 FOR UPDATE`,
			ownerPrincipal, ownerURI).Scan(&calendarID)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}

		var instanceID int64
		var shareeURI string
		err = tx.QueryRow(
			`SELECT id, uri FROM calendarinstances WHERE calendarid = $1 AND principaluri = $2 AND access IN (2, 3)`,
			calendarID, principalURI).Scan(&instanceID, &shareeURI)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}

		if status == caldav.InviteDeclined {
			_, err := tx.Exec(`DELETE FROM calendarinstances WHERE id = $1`, instanceID)
			return err
		}

		if _, err := tx.Exec(
			`UPDATE calendarinstances SET share_invitestatus = $1 WHERE id = $2`,
			caldav.InviteAccepted, instanceID); err != nil {
			return err
		}
		out = shareeURI
		return nil
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// ownerCalendarFromHref extracts the owning principal and calendar URI from a
// share invite's hosturl (…/calendars/<owner>/<calendar-uri>[/]).
func ownerCalendarFromHref(href string) (principalURI, uri string, ok bool) {
	if i := strings.Index(href, "://"); i >= 0 {
		if j := strings.IndexByte(href[i+3:], '/'); j >= 0 {
			href = href[i+3+j:]
		}
	}
	parts := strings.Split(strings.Trim(href, "/"), "/")
	for i, p := range parts {
		if p == "calendars" && i+2 < len(parts) {
			return "principals/" + parts[i+1], parts[i+2], true
		}
	}
	return "", "", false
}

// NotificationsForPrincipal returns a principal's invite notifications, newest
// last. Read from the calserver notifications table.
func (b *CalDAV) NotificationsForPrincipal(principalURI string) ([]caldav.InviteNotification, error) {
	rows, err := b.DB.Query(`
SELECT entryid, dtstamp, hosturl, organizer, organizername, access, invitestatus, summary
  FROM notifications WHERE principaluri = $1 ORDER BY id`, principalURI)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []caldav.InviteNotification
	for rows.Next() {
		var (
			n                          caldav.InviteNotification
			dtstamp                    int64
			organizerName, summaryText sql.NullString
		)
		if err := rows.Scan(&n.ID, &dtstamp, &n.HostURL, &n.Organizer,
			&organizerName, &n.Access, &n.InviteStatus, &summaryText); err != nil {
			return nil, err
		}
		n.DTStamp = time.Unix(dtstamp, 0).UTC()
		n.OrganizerName = organizerName.String
		n.Summary = summaryText.String
		out = append(out, n)
	}
	return out, rows.Err()
}

// AddNotification stores (or refreshes) an invite notification in a principal's
// inbox, keyed by its stable entry id.
func (b *CalDAV) AddNotification(principalURI string, n caldav.InviteNotification) error {
	_, err := b.DB.Exec(`
INSERT INTO notifications
    (principaluri, entryid, dtstamp, hosturl, organizer, organizername, access, invitestatus, summary)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (principaluri, entryid) DO UPDATE SET
    dtstamp = EXCLUDED.dtstamp, hosturl = EXCLUDED.hosturl,
    organizer = EXCLUDED.organizer, organizername = EXCLUDED.organizername,
    access = EXCLUDED.access, invitestatus = EXCLUDED.invitestatus,
    summary = EXCLUDED.summary`,
		principalURI, n.ID, n.DTStamp.UTC().Unix(), n.HostURL, n.Organizer,
		nullIfEmpty(n.OrganizerName), n.Access, n.InviteStatus, nullIfEmpty(n.Summary))
	return err
}

// DeleteNotification removes a notification from a principal's inbox.
func (b *CalDAV) DeleteNotification(principalURI, id string) error {
	_, err := b.DB.Exec(
		`DELETE FROM notifications WHERE principaluri = $1 AND entryid = $2`, principalURI, id)
	return err
}

// SetPublishStatus publishes or unpublishes a calendar. Publishing mints an
// opaque public URI stored in the calserver calendarpublications table;
// unpublishing removes the row. Port of the calendarserver publish POST that
// sabre's PDO backend leaves unimplemented.
func (b *CalDAV) SetPublishStatus(id caldav.CalendarID, value bool) error {
	if !value {
		_, err := b.DB.Exec(`DELETE FROM calendarpublications WHERE calendarid = $1`, id.Calendar)
		return err
	}
	publicURI, err := newUUID()
	if err != nil {
		return err
	}
	// Keep an existing publication (and its URI) stable across re-publish.
	_, err = b.DB.Exec(`
INSERT INTO calendarpublications (calendarid, publicuri) VALUES ($1, $2)
ON CONFLICT (calendarid) DO NOTHING`, id.Calendar, publicURI)
	return err
}
