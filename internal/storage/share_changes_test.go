package storage_test

// What UpdateInvites reports (docs/share-invite-email.md): the committed
// []ShareChange a share-invite email is driven from. Postgres-only, on the
// sharingDB fixture (Alice owns one calendar; Bob and Carol resolve; Dan
// doesn't).

import (
	"sync"
	"testing"
	"time"

	"calserver/internal/caldav"
	"calserver/internal/storage"
)

func bobSet(access int) caldav.Sharee {
	return caldav.Sharee{Href: bobHref, Principal: "principals/bob", Access: access, Comment: "hi"}
}

func onlyChange(t *testing.T, changes []caldav.ShareChange) caldav.ShareChange {
	t.Helper()
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want exactly one", changes)
	}
	return changes[0]
}

func TestUpdateInvitesReportsCommittedChanges(t *testing.T) {
	backend := sharingDB(t, "calserver_share_changes")
	calID := ownerCalID(t, backend)

	// New local sharee: Created, with the instance the INSERT minted.
	c := onlyChange(t, mustUpdate(t, backend, calID, bobSet(caldav.AccessRead)))
	var id int64
	var uri string
	if err := backend.DB.QueryRow(
		`SELECT id, uri FROM calendarinstances WHERE principaluri = 'principals/bob'`).Scan(&id, &uri); err != nil {
		t.Fatal(err)
	}
	if !c.Created || c.Principal != "principals/bob" || c.InstanceID != id || c.InstanceURI != uri ||
		c.Access != caldav.AccessRead || c.Comment != "hi" {
		t.Fatalf("new share = %+v (row id=%d uri=%s)", c, id, uri)
	}

	// Access change: same instance, not Created.
	c = onlyChange(t, mustUpdate(t, backend, calID, bobSet(caldav.AccessReadWrite)))
	if c.Created || c.InstanceID != id || c.Access != caldav.AccessReadWrite {
		t.Fatalf("access change = %+v", c)
	}

	// An alias of the same principal is the same sharee: no second row.
	alias := bobSet(caldav.AccessReadWrite)
	alias.Href = "mailto:BOB@example.org"
	if c := onlyChange(t, mustUpdate(t, backend, calID, alias)); c.Created || c.InstanceID != id {
		t.Fatalf("alias = %+v", c)
	}

	// Unresolved address: reported, but with no principal.
	c = onlyChange(t, mustUpdate(t, backend, calID, caldav.Sharee{Href: danHref, Access: caldav.AccessRead}))
	if c.Principal != "" || !c.Created {
		t.Fatalf("unresolved = %+v", c)
	}

	// Removal reports nothing and drops the row.
	if changes := mustUpdate(t, backend, calID, caldav.Sharee{Href: bobHref, Principal: "principals/bob", Access: caldav.AccessNoAccess}); len(changes) != 0 {
		t.Fatalf("removal reported %+v", changes)
	}
	if _, err := backend.SharedInstance(t.Context(), calID.Calendar, "principals/bob"); err != nil {
		t.Fatal(err)
	}
	if inst, _ := backend.SharedInstance(t.Context(), calID.Calendar, "principals/bob"); inst != nil {
		t.Fatalf("bob still shared after removal: %+v", inst)
	}

	// A re-share is a new instance with a higher id (the generation).
	c = onlyChange(t, mustUpdate(t, backend, calID, bobSet(caldav.AccessRead)))
	if !c.Created || c.InstanceID <= id || c.InstanceURI == uri {
		t.Fatalf("re-share = %+v, want a new instance after id %d", c, id)
	}
}

func TestUpdateInvitesNormalizesTheRequest(t *testing.T) {
	backend := sharingDB(t, "calserver_share_normalize")
	calID := ownerCalID(t, backend)

	// Duplicate sets for one principal (two spellings): one change, last wins.
	first := bobSet(caldav.AccessRead)
	second := bobSet(caldav.AccessReadWrite)
	second.Href = "mailto:BOB@example.org"
	c := onlyChange(t, mustUpdate(t, backend, calID, first, second))
	if !c.Created || c.Access != caldav.AccessReadWrite {
		t.Fatalf("duplicate sets = %+v", c)
	}
	if n := countRows(t, backend, `principaluri = 'principals/bob'`); n != 1 {
		t.Fatalf("bob rows = %d, want 1", n)
	}

	// Set and remove of one sharee in one POST: the remove wins, no change.
	carol := caldav.Sharee{Href: carolHref, Principal: "principals/carol", Access: caldav.AccessRead}
	carolRm := caldav.Sharee{Href: carolHref, Principal: "principals/carol", Access: caldav.AccessNoAccess}
	if changes := mustUpdate(t, backend, calID, carol, carolRm); len(changes) != 0 {
		t.Fatalf("set+remove reported %+v", changes)
	}
	if n := countRows(t, backend, `principaluri = 'principals/carol'`); n != 0 {
		t.Fatalf("carol rows = %d, want 0", n)
	}

	// The owner is ignored.
	owner := caldav.Sharee{Href: "mailto:alice@example.org", Principal: "principals/alice", Access: caldav.AccessRead}
	if changes := mustUpdate(t, backend, calID, owner); len(changes) != 0 {
		t.Fatalf("owner entry reported %+v", changes)
	}
}

// TestUpdateInvitesUnresolvedBecomesLocal: an invite recorded as invalid that
// later resolves is bound to its principal in place, and reported Created —
// it is the first usable share for that person.
func TestUpdateInvitesUnresolvedBecomesLocal(t *testing.T) {
	backend := sharingDB(t, "calserver_share_resolve")
	calID := ownerCalID(t, backend)

	const erinHref = "mailto:erin@example.org"
	pending := onlyChange(t, mustUpdate(t, backend, calID, caldav.Sharee{Href: erinHref, Access: caldav.AccessRead}))

	if _, err := backend.DB.Exec(
		`INSERT INTO principals (uri, email, displayname) VALUES ('principals/erin', 'erin@example.org', 'Erin')`); err != nil {
		t.Fatal(err)
	}
	c := onlyChange(t, mustUpdate(t, backend, calID,
		caldav.Sharee{Href: erinHref, Principal: "principals/erin", Access: caldav.AccessRead}))
	if !c.Created || c.InstanceID != pending.InstanceID || c.Principal != "principals/erin" {
		t.Fatalf("transition = %+v, want Created on instance %d", c, pending.InstanceID)
	}
	inv := findInvite(t, mustInvites(t, backend, calID), erinHref)
	if inv.Principal != "principals/erin" || inv.InviteStatus != caldav.InviteAccepted {
		t.Fatalf("erin invite = %+v, want bound and accepted", inv)
	}
	// Once resolved, a re-send is an ordinary no-op update.
	if c := onlyChange(t, mustUpdate(t, backend, calID,
		caldav.Sharee{Href: erinHref, Principal: "principals/erin", Access: caldav.AccessRead})); c.Created {
		t.Fatalf("re-send after transition = %+v", c)
	}
}

// TestUpdateInvitesRollsBack: a failure mid-request commits nothing and
// reports no changes.
func TestUpdateInvitesRollsBack(t *testing.T) {
	backend := sharingDB(t, "calserver_share_rollback")
	calID := ownerCalID(t, backend)
	mustUpdate(t, backend, calID, bobSet(caldav.AccessRead))

	// Carol's INSERT clones from a nonexistent owner instance and fails after
	// Bob's update already ran in the same transaction.
	bad := calID
	bad.Instance = "999999"
	changes, err := backend.UpdateInvites(t.Context(), bad, []caldav.Sharee{
		bobSet(caldav.AccessReadWrite),
		{Href: carolHref, Principal: "principals/carol", Access: caldav.AccessRead},
	})
	if err == nil || changes != nil {
		t.Fatalf("UpdateInvites = %+v, %v; want an error and no changes", changes, err)
	}
	if inst, _ := backend.SharedInstance(t.Context(), calID.Calendar, "principals/bob"); inst == nil || inst.Access != caldav.AccessRead {
		t.Fatalf("bob after rollback = %+v, want the original read share", inst)
	}

	// A calendar with no owner instance is refused outright.
	if _, err := backend.UpdateInvites(t.Context(), caldav.CalendarID{Calendar: "999999", Instance: "1"},
		[]caldav.Sharee{bobSet(caldav.AccessRead)}); err == nil {
		t.Fatal("share on a calendar without an owner instance succeeded")
	}
}

// TestUpdateInvitesConcurrent: identical share POSTs racing on one calendar
// both succeed, but only one reports the new share — the owner-instance lock
// makes the second see the first's committed roster.
func TestUpdateInvitesConcurrent(t *testing.T) {
	backend := sharingDB(t, "calserver_share_concurrent")
	calID := ownerCalID(t, backend)

	const n = 8
	var wg sync.WaitGroup
	results := make([][]caldav.ShareChange, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = backend.UpdateInvites(t.Context(), calID, []caldav.Sharee{bobSet(caldav.AccessRead)})
		}(i)
	}
	wg.Wait()
	created := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("POST %d: %v", i, errs[i])
		}
		for _, c := range results[i] {
			if c.Created {
				created++
			}
		}
	}
	if created != 1 {
		t.Fatalf("Created reported %d times, want 1", created)
	}
	if rows := countRows(t, backend, `principaluri = 'principals/bob'`); rows != 1 {
		t.Fatalf("bob rows = %d, want 1", rows)
	}

	// A decline racing an access change serializes on the same lock: both
	// succeed and bob ends with at most one instance.
	for i := 0; i < 4; i++ {
		var wg sync.WaitGroup
		var replyErr, updErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, replyErr = backend.ShareReply(t.Context(), "principals/bob", "/calendars/alice/default", caldav.InviteDeclined, "", "", "")
		}()
		go func() {
			defer wg.Done()
			_, updErr = backend.UpdateInvites(t.Context(), calID, []caldav.Sharee{bobSet(caldav.AccessReadWrite)})
		}()
		wg.Wait()
		if replyErr != nil || updErr != nil {
			t.Fatalf("decline=%v update=%v", replyErr, updErr)
		}
		if rows := countRows(t, backend, `principaluri = 'principals/bob'`); rows > 1 {
			t.Fatalf("bob rows = %d after decline/update race", rows)
		}
	}
}

func TestSharedInstanceReportsOwnerAndName(t *testing.T) {
	backend := sharingDB(t, "calserver_share_instance")
	calID := ownerCalID(t, backend)
	c := onlyChange(t, mustUpdate(t, backend, calID, bobSet(caldav.AccessReadWrite)))

	inst, err := backend.SharedInstance(t.Context(), calID.Calendar, "principals/bob")
	if err != nil || inst == nil {
		t.Fatalf("SharedInstance = %+v, %v", inst, err)
	}
	if inst.ID != c.InstanceID || inst.URI != c.InstanceURI || inst.Access != caldav.AccessReadWrite ||
		inst.OwnerPrincipal != "principals/alice" || inst.CalendarName != "Alice cal" {
		t.Fatalf("SharedInstance = %+v", inst)
	}
	// The owner's own row is not a shared instance.
	if inst, err := backend.SharedInstance(t.Context(), calID.Calendar, "principals/alice"); err != nil || inst != nil {
		t.Fatalf("owner lookup = %+v, %v; want none", inst, err)
	}
}

func TestDomainPolicy(t *testing.T) {
	backend := sharingDB(t, "calserver_domain_policy")
	policy := &storage.DomainPolicy{DB: backend.DB}

	if v, err := policy.ShareNotify(t.Context(), "example.org"); err != nil || v != "off" {
		t.Fatalf("no row = %q, %v; want off", v, err)
	}
	if _, err := backend.DB.Exec(
		`INSERT INTO domain_scheduling_policy (domain, share_notify) VALUES ('example.org', 'email')`); err != nil {
		t.Fatal(err)
	}
	if v, err := policy.ShareNotify(t.Context(), "Example.ORG"); err != nil || v != "email" {
		t.Fatalf("opted in = %q, %v; want email (lookups lowercase)", v, err)
	}
	// The table only takes known values and lowercased domains.
	if _, err := backend.DB.Exec(
		`INSERT INTO domain_scheduling_policy (domain, share_notify) VALUES ('other.org', 'always')`); err == nil {
		t.Fatal("bad share_notify value accepted")
	}
	if _, err := backend.DB.Exec(
		`INSERT INTO domain_scheduling_policy (domain) VALUES ('Mixed.org')`); err == nil {
		t.Fatal("mixed-case domain accepted")
	}

	// The cache serves a changed row until its TTL lapses.
	cached := &storage.CachedSharePolicy{Source: policy, TTL: time.Hour}
	if v, _ := cached.ShareNotify(t.Context(), "example.org"); v != "email" {
		t.Fatalf("cached = %q", v)
	}
	if _, err := backend.DB.Exec(`UPDATE domain_scheduling_policy SET share_notify = 'off'`); err != nil {
		t.Fatal(err)
	}
	if v, _ := cached.ShareNotify(t.Context(), "example.org"); v != "email" {
		t.Fatalf("cached after change = %q, want the cached email", v)
	}
	if v, _ := (&storage.CachedSharePolicy{Source: policy, TTL: time.Hour}).ShareNotify(t.Context(), "example.org"); v != "off" {
		t.Fatalf("fresh cache = %q, want off", v)
	}
}

func mustUpdate(t *testing.T, b *storage.CalDAV, id caldav.CalendarID, sharees ...caldav.Sharee) []caldav.ShareChange {
	t.Helper()
	changes, err := b.UpdateInvites(t.Context(), id, sharees)
	if err != nil {
		t.Fatalf("UpdateInvites: %v", err)
	}
	return changes
}

func countRows(t *testing.T, b *storage.CalDAV, where string) int {
	t.Helper()
	var n int
	if err := b.DB.QueryRow(`SELECT count(*) FROM calendarinstances WHERE ` + where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestUpdateInvitesUnresolvedAddressReusesItsRow: re-sending a share for an
// address that no longer resolves updates the row shared under it instead of
// adding a second, invalid one.
func TestUpdateInvitesUnresolvedAddressReusesItsRow(t *testing.T) {
	backend := sharingDB(t, "calserver_share_unresolved_again")
	calID := ownerCalID(t, backend)
	first := onlyChange(t, mustUpdate(t, backend, calID, bobSet(caldav.AccessRead)))

	c := onlyChange(t, mustUpdate(t, backend, calID, caldav.Sharee{Href: bobHref, Access: caldav.AccessRead}))
	if c.InstanceID != first.InstanceID || c.Created || c.Principal != "" {
		t.Fatalf("unresolved re-send = %+v, want an update of instance %d", c, first.InstanceID)
	}
	if n := countRows(t, backend, `lower(share_href) = lower('`+bobHref+`')`); n != 1 {
		t.Fatalf("rows for %s = %d, want 1", bobHref, n)
	}
	if n := countRows(t, backend, `principaluri = 'principals/bob'`); n != 1 {
		t.Fatalf("bob lost his instance")
	}
}

// TestUpdateInvitesRegrantUnderNewAddressSurvivesStaleRemove: re-sharing Bob
// under a new address while removing his stale alias in the same POST keeps
// the share the request granted.
func TestUpdateInvitesRegrantUnderNewAddressSurvivesStaleRemove(t *testing.T) {
	backend := sharingDB(t, "calserver_share_stale_alias")
	calID := ownerCalID(t, backend)
	const stale = "mailto:bob@old.example.org"
	first := onlyChange(t, mustUpdate(t, backend, calID,
		caldav.Sharee{Href: stale, Principal: "principals/bob", Access: caldav.AccessRead}))

	// bob@old no longer resolves; bob@example.org does.
	changes := mustUpdate(t, backend, calID,
		caldav.Sharee{Href: bobHref, Principal: "principals/bob", Access: caldav.AccessReadWrite},
		caldav.Sharee{Href: stale, Access: caldav.AccessNoAccess})
	c := onlyChange(t, changes)
	if c.InstanceID != first.InstanceID || c.Access != caldav.AccessReadWrite {
		t.Fatalf("re-grant = %+v", c)
	}
	inst, err := backend.SharedInstance(t.Context(), calID.Calendar, "principals/bob")
	if err != nil || inst == nil || inst.ID != first.InstanceID || inst.Access != caldav.AccessReadWrite {
		t.Fatalf("bob's share after the POST = %+v, %v; want instance %d read-write", inst, err, first.InstanceID)
	}
}
