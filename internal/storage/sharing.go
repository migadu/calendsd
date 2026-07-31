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
// known href updates that instance's access/displayname/status; a new href
// clones the owner's instance into a fresh per-sharee row. Because there is no
// invitation UI, a resolved principal is auto-accepted and an unresolved one
// is recorded as invalid, exactly as sabre does.
func (b *CalDAV) UpdateInvites(id caldav.CalendarID, sharees []caldav.Sharee) error {
	current, err := b.Invites(id)
	if err != nil {
		return err
	}

	return withTx(b.DB, func(tx *sql.Tx) error {
		return applyShareeChanges(tx, id, sharees, current)
	})
}

// applyShareeChanges is UpdateInvites' per-sharee loop, run inside its
// transaction.
func applyShareeChanges(tx *sql.Tx, id caldav.CalendarID, sharees, current []caldav.Sharee) error {
	for _, sharee := range sharees {
		if sharee.Access == caldav.AccessNoAccess {
			// Removal: only sharee rows (access 2/3) match, so the owner's
			// own row can never be dropped by a share request.
			if _, err := tx.Exec(
				`DELETE FROM calendarinstances WHERE calendarid = $1 AND share_href = $2 AND access IN (2, 3)`,
				id.Calendar, sharee.Href); err != nil {
				return err
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

		if old := findSharee(current, sharee.Href); old != nil {
			// Update in place. Keep the prior display name when the request
			// omits a common-name (sabre merges the old properties in).
			displayName := sharee.CommonName
			if displayName == "" {
				displayName = old.CommonName
			}
			// Restrict to sharee rows (access 2/3), symmetric with the delete
			// branch, so a share update can never touch the owner's own row.
			if _, err := tx.Exec(`
UPDATE calendarinstances SET access = $1, share_displayname = $2, share_invitestatus = $3
 WHERE calendarid = $4 AND share_href = $5 AND access IN (2, 3)`,
				sharee.Access, nullIfEmpty(displayName), status, id.Calendar, sharee.Href); err != nil {
				return err
			}
			continue
		}

		// New sharee: clone the owner's instance (its displayname, description,
		// order, color, timezone) into a fresh row keyed to the sharee.
		uri, err := newUUID()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
INSERT INTO calendarinstances
    (calendarid, principaluri, access, displayname, uri, description,
     calendarorder, calendarcolor, timezone, transparent,
     share_href, share_displayname, share_invitestatus)
SELECT $1, $2, $3, displayname, $4, description,
       calendarorder, calendarcolor, timezone, 1,
       $5, $6, $7
  FROM calendarinstances WHERE id = $8`,
			id.Calendar, principal, sharee.Access, uri, sharee.Href,
			nullIfEmpty(sharee.CommonName), status, id.Instance); err != nil {
			return err
		}
	}
	return nil
}

// findSharee returns the existing invite with the given href, or nil.
func findSharee(invites []caldav.Sharee, href string) *caldav.Sharee {
	for i := range invites {
		if invites[i].Href == href {
			return &invites[i]
		}
	}
	return nil
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
		var calendarID int64
		err := tx.QueryRow(
			`SELECT calendarid FROM calendarinstances WHERE principaluri = $1 AND uri = $2 AND access = 1`,
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
