package storage_test

// Calendar-sharing backend tests against a scratch Postgres with the sabre
// schema plus the calendsd extension tables. Exercises every sharing surface
// storage.CalDAV implements:
//
//   - SharingSupport      — the invite list a {cs}share POST drives.
//   - ReplySupport        — a sharee accepting/declining an invite.
//   - NotificationsBackend — the per-principal invite inbox.
//   - PublishSupport      — publishing/unpublishing a calendar.
//
// Every write must round-trip the way a live sabre on the same database would
// read it back; the extension tables (notifications, calendarpublications) are
// additive and invisible to sabre. Postgres-only; skips when the dev Postgres
// isn't reachable, like the concurrency and admin-queries tests.

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/migadu/calendsd/internal/caldav"
	davxml "github.com/migadu/calendsd/internal/dav/xml"
	"github.com/migadu/calendsd/internal/storage"
	"github.com/migadu/calendsd/internal/storage/storagetest"
)

// sharingDB builds a scratch database (sabre schema + calendsd extensions)
// seeded with Alice (owner of one calendar) plus Bob and Carol as candidate
// sharees, and returns the backend over it.
func sharingDB(t *testing.T, dbName string) *storage.CalDAV {
	t.Helper()
	db := storagetest.NewDB(t, dbName)
	// Alice owns one calendar (access=1). Bob and Carol are candidate sharees
	// with principals; Dan is deliberately absent so his invite can't resolve.
	mustExec(t, db, `
		INSERT INTO principals (uri, email, displayname) VALUES
			('principals/alice', 'alice@example.org', 'Alice'),
			('principals/bob',   'bob@example.org',   'Bob'),
			('principals/carol', 'carol@example.org', 'Carol');
		INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT');
		INSERT INTO calendarinstances (calendarid, principaluri, access, uri, displayname, calendarcolor)
			VALUES (currval('calendars_id_seq'), 'principals/alice', 1, 'default', 'Alice cal', '#ff0000');
	`)

	return &storage.CalDAV{DB: db}
}

// ownerCalID returns Alice's calendar id (shared calendarid + her instance id),
// the value the sharing plugin hands the backend.
func ownerCalID(t *testing.T, backend *storage.CalDAV) caldav.CalendarID {
	t.Helper()
	cals, err := backend.CalendarsForUser(t.Context(), "principals/alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 1 {
		t.Fatalf("owner calendars = %d, want 1", len(cals))
	}
	return cals[0].ID
}

const (
	bobHref   = "mailto:bob@example.org"
	carolHref = "mailto:carol@example.org"
	danHref   = "mailto:dan@example.org"
)

// TestShareTwoCalendarsWithOnePerson guards the fix for sabre's over-strict
// calendarinstances_principaluri_share_href unique index: because a sharee row
// stores the invitee's address in share_href and their principal in
// principaluri — both identical for every calendar shared with that person —
// that index made the SECOND share to a given sharee fail with a duplicate-key
// error. The calendsd extension schema drops it; sharing two calendars with
// one person must now succeed and surface both in their home.
func TestShareTwoCalendarsWithOnePerson(t *testing.T) {
	backend := sharingDB(t, "calserver_sharing_two")

	// Give Alice a second calendar (the helper seeds one). RETURNING id avoids
	// a cross-connection currval() in the pooled DB.
	var cal2 int
	if err := backend.DB.QueryRow(
		`INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT') RETURNING id`).Scan(&cal2); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.DB.Exec(
		`INSERT INTO calendarinstances (calendarid, principaluri, access, uri, displayname)
		 VALUES ($1, 'principals/alice', 1, 'second', 'Alice cal 2')`, cal2); err != nil {
		t.Fatal(err)
	}

	cals, err := backend.CalendarsForUser(t.Context(), "principals/alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 2 {
		t.Fatalf("alice calendars = %d, want 2", len(cals))
	}

	// Share BOTH calendars with Bob — the second must not collide.
	for _, cal := range cals {
		if _, err := backend.UpdateInvites(t.Context(), cal.ID, []caldav.Sharee{
			{Href: bobHref, Principal: "principals/bob", Access: caldav.AccessReadWrite},
		}); err != nil {
			t.Fatalf("sharing calendar %q with bob: %v", cal.ID.Calendar, err)
		}
	}

	bobCals, err := backend.CalendarsForUser(t.Context(), "principals/bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(bobCals) != 2 {
		t.Fatalf("bob shared calendars = %d, want 2", len(bobCals))
	}
}

func TestCalendarSharing(t *testing.T) {
	backend := sharingDB(t, "calserver_sharing")
	calID := ownerCalID(t, backend)

	// --- Phase 1: add Bob (read-write) and Carol (read-only). ---------------
	_, err := backend.UpdateInvites(t.Context(), calID, []caldav.Sharee{
		{Href: bobHref, Principal: "principals/bob", CommonName: "Bob B", Access: caldav.AccessReadWrite},
		{Href: carolHref, Principal: "principals/carol", CommonName: "Carol C", Access: caldav.AccessRead},
	})
	if err != nil {
		t.Fatalf("UpdateInvites add: %v", err)
	}

	invites, err := backend.Invites(t.Context(), calID)
	if err != nil {
		t.Fatal(err)
	}
	// The owner is always part of the invite list (sabre returns her row too).
	if got := len(invites); got != 3 {
		t.Fatalf("invites = %d, want 3 (owner + 2 sharees)", got)
	}
	bob := findInvite(t, invites, bobHref)
	if bob.Access != caldav.AccessReadWrite {
		t.Errorf("bob access = %d, want %d", bob.Access, caldav.AccessReadWrite)
	}
	if bob.InviteStatus != caldav.InviteAccepted {
		t.Errorf("bob invite status = %d, want accepted (%d)", bob.InviteStatus, caldav.InviteAccepted)
	}
	if bob.Principal != "principals/bob" {
		t.Errorf("bob principal = %q, want principals/bob", bob.Principal)
	}
	if bob.CommonName != "Bob B" {
		t.Errorf("bob common-name = %q, want %q", bob.CommonName, "Bob B")
	}
	carol := findInvite(t, invites, carolHref)
	if carol.Access != caldav.AccessRead {
		t.Errorf("carol access = %d, want %d", carol.Access, caldav.AccessRead)
	}

	// Bob's shared view must now surface in his calendar home, cloned from the
	// owner's displayname, writable (no read-only marker).
	bobCals, err := backend.CalendarsForUser(t.Context(), "principals/bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(bobCals) != 1 {
		t.Fatalf("bob calendars = %d, want 1", len(bobCals))
	}
	if dn := bobCals[0].Props[xml.Name{Space: davxml.NSDav, Local: "displayname"}]; dn != "Alice cal" {
		t.Errorf("bob calendar displayname = %v, want cloned %q", dn, "Alice cal")
	}
	if _, readOnly := bobCals[0].Props[xml.Name{Space: davxml.NSSabreDAV, Local: "read-only"}]; readOnly {
		t.Error("bob has read-write access but calendar is marked read-only")
	}
	if bobCals[0].ID.Calendar != calID.Calendar {
		t.Errorf("bob shares calendarid %q, want owner's %q", bobCals[0].ID.Calendar, calID.Calendar)
	}

	// Carol's read-only share carries sabre's read-only marker.
	carolCals, err := backend.CalendarsForUser(t.Context(), "principals/carol")
	if err != nil {
		t.Fatal(err)
	}
	if len(carolCals) != 1 {
		t.Fatalf("carol calendars = %d, want 1", len(carolCals))
	}
	if _, readOnly := carolCals[0].Props[xml.Name{Space: davxml.NSSabreDAV, Local: "read-only"}]; !readOnly {
		t.Error("carol has read-only access but calendar is not marked read-only")
	}

	// --- Phase 2: downgrade Bob to read-only; must update, not duplicate. ---
	_, err = backend.UpdateInvites(t.Context(), calID, []caldav.Sharee{
		{Href: bobHref, Principal: "principals/bob", CommonName: "Bob B", Access: caldav.AccessRead},
	})
	if err != nil {
		t.Fatalf("UpdateInvites downgrade: %v", err)
	}
	invites, _ = backend.Invites(t.Context(), calID)
	if got := len(invites); got != 3 {
		t.Fatalf("invites after downgrade = %d, want 3 (updated, not duplicated)", got)
	}
	if bob := findInvite(t, invites, bobHref); bob.Access != caldav.AccessRead {
		t.Errorf("bob access after downgrade = %d, want %d", bob.Access, caldav.AccessRead)
	}
	if bobCals, _ := backend.CalendarsForUser(t.Context(), "principals/bob"); len(bobCals) != 1 {
		t.Fatalf("bob still has %d calendars after downgrade, want 1", len(bobCals))
	} else if _, readOnly := bobCals[0].Props[xml.Name{Space: davxml.NSSabreDAV, Local: "read-only"}]; !readOnly {
		t.Error("bob downgraded to read but calendar is not marked read-only")
	}

	// --- Phase 3: an unresolved principal is recorded as an invalid invite. -
	_, err = backend.UpdateInvites(t.Context(), calID, []caldav.Sharee{
		{Href: danHref, Principal: "", CommonName: "Dan D", Access: caldav.AccessRead},
	})
	if err != nil {
		t.Fatalf("UpdateInvites invalid: %v", err)
	}
	dan := findInvite(t, mustInvites(t, backend, calID), danHref)
	if dan.InviteStatus != caldav.InviteInvalid {
		t.Errorf("dan invite status = %d, want invalid (%d)", dan.InviteStatus, caldav.InviteInvalid)
	}

	// --- Phase 4: removing a sharee drops their instance row entirely. ------
	_, err = backend.UpdateInvites(t.Context(), calID, []caldav.Sharee{
		{Href: carolHref, Access: caldav.AccessNoAccess},
	})
	if err != nil {
		t.Fatalf("UpdateInvites remove: %v", err)
	}
	for _, s := range mustInvites(t, backend, calID) {
		if s.Href == carolHref {
			t.Fatalf("carol still present after removal: %+v", s)
		}
	}
	if carolCals, _ := backend.CalendarsForUser(t.Context(), "principals/carol"); len(carolCals) != 0 {
		t.Errorf("carol still has %d calendars after removal, want 0", len(carolCals))
	}
	// The owner must never be removable via a share request against her row.
	if aliceCals, _ := backend.CalendarsForUser(t.Context(), "principals/alice"); len(aliceCals) != 1 {
		t.Errorf("owner calendars = %d after sharee churn, want 1", len(aliceCals))
	}
}

// TestCalendarShareReply covers a sharee's accept/decline of a share invite.
func TestCalendarShareReply(t *testing.T) {
	backend := sharingDB(t, "calserver_sharereply")
	calID := ownerCalID(t, backend)

	if _, err := backend.UpdateInvites(t.Context(), calID, []caldav.Sharee{
		{Href: bobHref, Principal: "principals/bob", Access: caldav.AccessReadWrite},
		{Href: carolHref, Principal: "principals/carol", Access: caldav.AccessRead},
	}); err != nil {
		t.Fatalf("UpdateInvites: %v", err)
	}

	const hostURL = "/calendars/alice/default"

	// Force Carol's invite to no-response so the accept reply has real work to
	// do (drivers other than sabre's auto-accept can leave it pending).
	if _, err := backend.DB.Exec(
		`UPDATE calendarinstances SET share_invitestatus = $1 WHERE principaluri = 'principals/carol'`,
		caldav.InviteNoResponse); err != nil {
		t.Fatal(err)
	}

	// Carol accepts: her instance is confirmed and the shared-as path points at
	// her own calendar home.
	path, err := backend.ShareReply(t.Context(), "principals/carol", hostURL, caldav.InviteAccepted, "", "", "")
	if err != nil {
		t.Fatalf("ShareReply accept: %v", err)
	}
	// The return is request-relative — a bare calendar segment with no leading
	// slash, so the caldav layer's resolveHref keeps a non-root BaseURI. It is
	// Carol's own calendar URI.
	var carolURI string
	if err := backend.DB.QueryRow(
		`SELECT uri FROM calendarinstances WHERE principaluri = 'principals/carol'`).Scan(&carolURI); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, "/") || path != carolURI {
		t.Errorf("accepted shared-as path = %q, want Carol's bare calendar segment %q", path, carolURI)
	}
	var status int
	if err := backend.DB.QueryRow(
		`SELECT share_invitestatus FROM calendarinstances WHERE principaluri = 'principals/carol'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != caldav.InviteAccepted {
		t.Errorf("carol invite status after accept = %d, want accepted (%d)", status, caldav.InviteAccepted)
	}
	if cals, _ := backend.CalendarsForUser(t.Context(), "principals/carol"); len(cals) != 1 {
		t.Errorf("carol calendars after accept = %d, want 1", len(cals))
	}

	// Bob declines: his instance is dropped and the calendar disappears from
	// his home, while the owner's calendar is untouched.
	path, err = backend.ShareReply(t.Context(), "principals/bob", hostURL, caldav.InviteDeclined, "", "", "")
	if err != nil {
		t.Fatalf("ShareReply decline: %v", err)
	}
	if path != "" {
		t.Errorf("declined shared-as path = %q, want empty", path)
	}
	if cals, _ := backend.CalendarsForUser(t.Context(), "principals/bob"); len(cals) != 0 {
		t.Errorf("bob calendars after decline = %d, want 0", len(cals))
	}
	if cals, _ := backend.CalendarsForUser(t.Context(), "principals/alice"); len(cals) != 1 {
		t.Errorf("owner calendars after a sharee decline = %d, want 1", len(cals))
	}
}

// TestCalendarNotifications covers the per-principal invite inbox CRUD.
func TestCalendarNotifications(t *testing.T) {
	backend := sharingDB(t, "calserver_notifications")

	if items, err := backend.NotificationsForPrincipal(t.Context(), "principals/bob"); err != nil {
		t.Fatal(err)
	} else if len(items) != 0 {
		t.Fatalf("fresh inbox has %d items, want 0", len(items))
	}

	dtstamp := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	invite := caldav.InviteNotification{
		ID:           "invite-1",
		DTStamp:      dtstamp,
		HostURL:      "calendars/alice/default",
		Organizer:    "principals/alice",
		Access:       caldav.AccessReadWrite,
		InviteStatus: caldav.InviteNoResponse,
		Summary:      "join my calendar",
	}
	if err := backend.AddNotification(t.Context(), "principals/bob", invite); err != nil {
		t.Fatalf("AddNotification: %v", err)
	}

	items, err := backend.NotificationsForPrincipal(t.Context(), "principals/bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("inbox has %d items, want 1", len(items))
	}
	got := items[0]
	if got.ID != invite.ID || got.HostURL != invite.HostURL || got.Organizer != invite.Organizer ||
		got.Access != invite.Access || got.InviteStatus != invite.InviteStatus || got.Summary != invite.Summary {
		t.Errorf("notification round-trip mismatch:\n got %+v\nwant %+v", got, invite)
	}
	if !got.DTStamp.Equal(dtstamp) {
		t.Errorf("notification dtstamp = %v, want %v", got.DTStamp, dtstamp)
	}

	// Re-adding the same entry id updates in place rather than duplicating.
	invite.Access = caldav.AccessRead
	if err := backend.AddNotification(t.Context(), "principals/bob", invite); err != nil {
		t.Fatalf("AddNotification update: %v", err)
	}
	items, _ = backend.NotificationsForPrincipal(t.Context(), "principals/bob")
	if len(items) != 1 {
		t.Fatalf("inbox has %d items after re-add, want 1 (upsert)", len(items))
	}
	if items[0].Access != caldav.AccessRead {
		t.Errorf("notification access after update = %d, want %d", items[0].Access, caldav.AccessRead)
	}

	// The inbox is per principal — Carol sees nothing.
	if items, _ := backend.NotificationsForPrincipal(t.Context(), "principals/carol"); len(items) != 0 {
		t.Errorf("carol inbox has %d items, want 0 (per-principal isolation)", len(items))
	}

	if err := backend.DeleteNotification(t.Context(), "principals/bob", invite.ID); err != nil {
		t.Fatalf("DeleteNotification: %v", err)
	}
	if items, _ := backend.NotificationsForPrincipal(t.Context(), "principals/bob"); len(items) != 0 {
		t.Errorf("inbox has %d items after delete, want 0", len(items))
	}
}

// TestCalendarPublish covers publishing and unpublishing a calendar.
func TestCalendarPublish(t *testing.T) {
	backend := sharingDB(t, "calserver_publish")
	calID := ownerCalID(t, backend)

	publicURI := func() string {
		t.Helper()
		var uri string
		err := backend.DB.QueryRow(
			`SELECT publicuri FROM calendarpublications WHERE calendarid = $1`, calID.Calendar).Scan(&uri)
		if err != nil {
			return ""
		}
		return uri
	}

	if got := publicURI(); got != "" {
		t.Fatalf("calendar is published before any request: %q", got)
	}

	if err := backend.SetPublishStatus(t.Context(), calID, true); err != nil {
		t.Fatalf("SetPublishStatus publish: %v", err)
	}
	first := publicURI()
	if first == "" {
		t.Fatal("calendar not published after SetPublishStatus(true)")
	}

	// Re-publishing keeps the same public URI stable.
	if err := backend.SetPublishStatus(t.Context(), calID, true); err != nil {
		t.Fatalf("SetPublishStatus re-publish: %v", err)
	}
	if got := publicURI(); got != first {
		t.Errorf("public URI changed on re-publish: %q -> %q", first, got)
	}

	if err := backend.SetPublishStatus(t.Context(), calID, false); err != nil {
		t.Fatalf("SetPublishStatus unpublish: %v", err)
	}
	if got := publicURI(); got != "" {
		t.Errorf("calendar still published after unpublish: %q", got)
	}

	// Deleting a published calendar must cascade the publication away, so no
	// orphan row survives (the FK does this without DeleteCalendar knowing).
	if err := backend.SetPublishStatus(t.Context(), calID, true); err != nil {
		t.Fatalf("SetPublishStatus re-publish before delete: %v", err)
	}
	if err := backend.DeleteCalendar(t.Context(), calID); err != nil {
		t.Fatalf("DeleteCalendar: %v", err)
	}
	if got := publicURI(); got != "" {
		t.Errorf("publication survived calendar deletion (orphan): %q", got)
	}
}

func mustInvites(t *testing.T, b *storage.CalDAV, id caldav.CalendarID) []caldav.Sharee {
	t.Helper()
	invites, err := b.Invites(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return invites
}

func findInvite(t *testing.T, invites []caldav.Sharee, href string) caldav.Sharee {
	t.Helper()
	for _, s := range invites {
		if s.Href == href {
			return s
		}
	}
	t.Fatalf("no invite with href %q in %+v", href, invites)
	return caldav.Sharee{}
}

// TestShareeViewCarriesShareAccess guards the {DAV:}share grant: the calendar
// ACL grants it only on the owner's view, keyed off ShareAccess, so storage
// must fill it or every sharee could re-share (and raise their own access).
func TestShareeViewCarriesShareAccess(t *testing.T) {
	backend := sharingDB(t, "calserver_sharing_access")
	if _, err := backend.UpdateInvites(t.Context(), ownerCalID(t, backend), []caldav.Sharee{
		{Href: bobHref, Principal: "principals/bob", Access: caldav.AccessRead},
		{Href: carolHref, Principal: "principals/carol", Access: caldav.AccessReadWrite},
	}); err != nil {
		t.Fatal(err)
	}
	for principal, want := range map[string]int{
		"principals/alice": caldav.AccessSharedOwner,
		"principals/bob":   caldav.AccessRead,
		"principals/carol": caldav.AccessReadWrite,
	} {
		cals, err := backend.CalendarsForUser(t.Context(), principal)
		if err != nil {
			t.Fatal(err)
		}
		if len(cals) != 1 || cals[0].ShareAccess != want {
			t.Errorf("%s: calendars = %+v, want one with ShareAccess %d", principal, cals, want)
		}
	}
}
