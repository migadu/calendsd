# Email a sharee when a calendar is shared with them

## Problem

Sharing a calendar (`POST {cs}share`, `shareRequest` in
`internal/caldav/sharing.go`) sends no email. What happens today:

- `UpdateInvites` → `applyShareeChanges` (`internal/storage/sharing.go`)
  **auto-accepts** a sharee that resolves to a local principal: it inserts
  their `calendarinstances` row with `share_invitestatus = accepted`, so the
  calendar is already in their calendar home. An unresolved address (no local
  principal) is stored as `invalid` and grants nothing.
- `shareRequest` then drops a calendarserver invite notification
  (`AddNotification`, the `notifications` table) into each local sharee's
  notification collection.
- The sharee only learns about it if their client reads that collection:
  calclient's invitations panel (`renderShareInvites` in
  `cmd/calclient/web/cal/js/invites.js`) and Apple Calendar do; Thunderbird,
  DAVx⁵ and most others don't. On those clients a calendar just appears in the
  list with no explanation — or the sharee never notices it at all.

Every mainstream sharing product (Google, iCloud, Fastmail) sends a "X shared a
calendar with you" email. We should too.

## Goal

When a calendar is **newly** shared with a **local** principal, email them a
plain notification: who shared what, at which access level, with a link that
opens calclient on that calendar. The email is informational — the share is
already in effect (auto-accept); declining stays in the invitations panel.

### Non-goals

- Emailing external (non-local) addresses. They cannot use the share (no
  principal to log in as), and sending mail to arbitrary addresses would turn
  the share endpoint into an open "send mail to anyone" path on the shared
  relay. Instead, calclient's share dialog should warn/refuse an address that
  comes back `invalid` (separate UI change, see follow-ups).
- An actionable accept/decline-by-email flow (tokens in links). The link opens
  the app; the user acts there.
- Changing the auto-accept model.

## When an email is sent

Only for a sharee that is, in this request:

| case                                                       | email? |
|------------------------------------------------------------|--------|
| new sharee, resolves to a local principal                  | **yes** |
| new sharee, unresolved (`invalid`)                         | no     |
| existing invalid share becomes a usable local share        | **yes** |
| existing sharee, access changed (read ↔ read-write)       | no     |
| existing sharee re-sent unchanged (client re-POSTs roster) | no     |
| removal (`AccessNoAccess`)                                 | no     |
| sharee is the owner themselves                             | no     |
| sharee is an auto-schedule (room/resource) principal       | no     |

"New" means the update committed a new sharee instance, or made a previously
unresolved entry usable locally, and that share still exists at the end of
the request. `UpdateInvites` returns the committed changes; the
handler does not pre-read and diff the roster. Existing local shares are
identified by resolved principal, so address spelling or aliases cannot turn
a roster re-POST into another notification. Unresolved entries retain href
identity. See Storage for locking and duplicate-entry handling.

Clients commonly re-POST the full roster. `AddNotification` upserts its stored
item, but that alone cannot prevent duplicate email.

Un-sharing and re-sharing later qualifies for a new email. If the earlier
email is still pending, the queue replaces it with the latest share; once a
send has started, it cannot be retracted.

## Per-domain policy

Reuse the domain-policy design from `docs/local-attendee-email.md` rather than
inventing a second knob: a per-recipient-domain setting, default off.
If that table has not shipped yet when this is built, add the column now so
both features share it:

```sql
-- in the same domain policy table as local_notify
share_notify text NOT NULL DEFAULT 'off' CHECK (share_notify IN ('off', 'email'))
```

Keyed on the domain of the **resolved sharee's canonical mailbox** (the
recipient's domain operator chooses the policy), cached with the same TTL
wrapper. On DB errors, skip email and record `policy_error`; the share still
succeeds. With no DB (in-memory server) the feature is off.

Resolve the mailbox from the principal's `{http://sabredav.org/ns}email-address`
property (`principals.email` in Postgres), validate it as a single mailbox,
and lowercase its domain for policy lookup. Use that same mailbox for the SMTP
recipient and `To` header. Do not send to the submitted href or infer an
address from the principal URI. A missing or malformed canonical mailbox skips
email with `invalid_recipient`. Resolve the owner's address from the same
property for `Reply-To`; omit `Reply-To` if it is unavailable or invalid.

Rationale for default-off: it changes user-visible behaviour (new mail in
inboxes), so domains opt in, exactly like local-attendee email.

## The link

calclient's routes (`cmd/calclient/web/cal/js/router.js`) have no per-calendar
or invitations URL today. Add one route:

```
/cal/calendar?cal=<sharee's calendar uri>
```

- The sharee's instance URI is the UUID `applyShareeChanges` mints for the new
  row. `UpdateInvites` must return it (see wiring) so the email can carry it.
- On load, calclient ensures that calendar is visible (un-hides it if the
  user had hidden it), scrolls it into view in the sidebar, briefly highlights
  it, and opens the invitations panel if a share notification for it is still
  pending — so Decline is one click away.
- After calendars have loaded successfully, an unknown/removed `cal` shows a
  small message: "This calendar isn't available to this account. It may have
  been removed, or you may need to sign in with another account." Keep the
  normal calendar view usable; do not expose the intended recipient's identity.
  A failed calendar load uses the usual load-error handling and retains the
  target for retry.
- The server needs no change for the route: `spaFallback`
  (`cmd/calclient/main.go`) already serves the shell for any `/cal/...` path.
- Logged out: login is an in-page form (`main.js`), so the URL — including
  `?cal=` — survives sign-in and is honoured once the session is up.
- Update `parseRoute`, `calendarPath`, and `main.js` canonicalization together:
  preserve `cal` through login and URL rewriting until calendar and notification
  loading completes. Apply the focus/open-panel action once per navigation,
  so subsequent renders do not keep unhiding the calendar or reopening the
  panel. Keep the target in the URL so a reload or account switch can retry it.

The absolute base comes from a new calendsd flag:

```
-web-url https://calendar.example.com   # calclient origin for links in emails; empty disables share emails
```

Empty `-web-url` disables the feature regardless of domain policy — an email
without a working link is worse than none.

## Email content

V1 uses `text/plain; charset=UTF-8` only. **No `text/calendar` part** — there
is no event to add. An HTML alternative can follow if needed.

```
From: "Alice Example via Calendar" <calendar@server-domain>
Reply-To: alice@example.org
To: bob@example.org
Subject: Alice Example shared "Work" with you

Alice Example (alice@example.org) shared the calendar "Work" with you.
You can view and edit events.            # or: You can view events.

<optional comment the sharer typed — the {cs}share summary>

Open it: https://calendar.example.com/cal/calendar?cal=<uri>

It's already in your calendar list. If you don't want it, open the
invitations panel (bell icon) and choose Decline.
```

- Sharer name: the owner principal's display name, falling back to the address.
- Calendar name: the owner's instance displayname, falling back to its URI.
- Every interpolated value (names, calendar name, comment) goes through
  `stripHeaderChars` for headers, as `framedEmail` does; the comment is
  length-capped (e.g. 500 chars) and plain text only.
- Sent **from the server address** with the sharer in the display name and
  `Reply-To`, i.e. the relay identity. Not send-as-user: that needs the
  sharer's submission credential in the request and brings in the DKIM/From
  constraints of `docs/design/IMIP-SEND-AS-USER-PLAN.md` for no real gain in a
  notification.

## Wiring

### Storage

`UpdateInvites` currently returns only `error`. Change the `SharingSupport`
contract to report what it did:

```go
// ShareChange is one sharee row UpdateInvites created or updated.
type ShareChange struct {
    Href        string
    Principal   string
    InstanceID  int64  // calendarinstances.id; generation ordering for re-shares
    InstanceURI string // the sharee's calendarinstances.uri
    Access      int    // committed access level
    Comment     string // final request comment for this sharee
    Created     bool   // new row or invalid-to-local transition; otherwise false
}

UpdateInvites(id CalendarID, sharees []Sharee) ([]ShareChange, error)
```

Returning changes alone does not make this race-free: today `UpdateInvites`
reads `Invites` before starting its transaction. Change the transaction flow:

1. Lock the owner's calendar instance with `SELECT ... FOR UPDATE`, then read
   the current roster through that transaction. A second update to the same
   calendar waits and then sees the first update's committed roster.
2. Resolve local share identity by principal, with href matching for unresolved
   entries. Normalize duplicate changes before writing: the last set for a
   principal/href wins, and a remove wins over a set in the same POST (matching
   the parser's sets-then-removes order). Owner entries are ignored. An existing
   unresolved entry that becomes local must acquire its principal and valid
   status; treat that transition as a new usable share for notification.
3. Apply the final changes, using `INSERT ... RETURNING id, uri` for creations
   and checking affected rows for updates. Return at most one change per final
   sharee instance, with its actual committed identity and access. A removed
   entry returns no change and cannot generate an email.
4. Return changes only after commit succeeds; on rollback or commit failure,
   return no changes and an error.

Use the same owner-instance lock before `ShareReply` changes or deletes a
sharee instance, keeping lock order consistent. Unshare through `UpdateInvites`
already takes it. The existing unique principal/calendar constraint remains a
backstop. Locks coordinate participating calendsd writers; a co-resident sabre
writer does not automatically follow this protocol. Do not claim concurrency
guarantees across those writers without a shared locking strategy.

Update `MockSharingBackend` to match the final-state and change-result semantics.
`Created` includes a previously unresolved entry becoming a usable local share;
ordinary access changes remain `false`.

### Plugin

A new narrow interface in `internal/caldav`, implemented by a small
`ShareMailer` (or a method on `IMipPlugin`, which already holds `Mailer`,
`SenderEmail`, `Durable` and `Metrics`):

```go
// ShareNotifier emails a local principal that a calendar was shared with them.
// Best effort: it never fails the share request.
type ShareNotifier interface {
    NotifyShare(r *http.Request, n ShareNotice)
}
```

In `shareRequest`, after `UpdateInvites` succeeds and alongside the existing
`AddNotification` loop, for each `ShareChange` with `Created && Principal != ""`
that passes the exclusions above, and whose domain policy is `email`, call
`NotifyShare`. `SharingPlugin` finds the notifier the way `SchedulePlugin`
finds `ExternalDeliverer` (scan `server.Plugins()`), or via a field set in
`internal/server/server.go`.

### Transport

- Enqueue on the existing durable queue (`internal/jobs`) as a new worker,
  `share_notify`, registered next to `imip_send` in `internal/server/server.go`.
  Args include calendar ID, principal URI, instance ID and URI, canonical
  recipient, and the capped comment. Use structured notice data so the worker
  can frame the message using the current calendar name and access level.
  No credentials; use the configured server relay identity.
- Before every send attempt, load the share and require the same principal,
  instance ID, and URI to still be present with access 2/3. A removed, declined,
  deleted, or superseded instance is discarded as `skipped_obsolete`. A lookup
  error retries; it must not be mistaken for a missing share. Recheck mailbox,
  resource/owner exclusions, policy, mailer, and web URL. If the principal's
  canonical mailbox changed, discard the old job rather than send to its former
  address. Frame the email using current access so a queued notice does not
  promise edit rights after a downgrade.
- The handler sends via `Mailer` with `SenderEmail` as envelope; retry/terminal
  classification reuses `retryableSend` / `classifySendError`. There is no
  SCHEDULE-STATUS to write back.
- No durable queue with a DB-backed policy: run the same validation and send
  synchronously. With no DB the policy is off, so in-memory servers do not
  send. No `Mailer` configured: skip, metered as `no_mailer`.

### Coalescing and ordering

The existing queue coalesces iMIP by `(uid, recipient, method)` and orders by
`(sequence, dtstamp)`. The share worker's arguments do not implement that
contract. Add an explicit, separate share enqueue path and schema migration:

- A partial unique index for available `share_notify` jobs on
  `(worker, calendar ID, principal URI)`. Principal identity keeps aliases and
  mailbox spelling from creating multiple pending notices. Keep the existing
  iMIP index and enqueue behavior unchanged; share jobs omit its keys.
- On conflict, replace the pending payload only when the incoming instance ID
  is greater; an equal or older instance is a no-op. The database-generated
  instance ID orders new shares to the same calendar/principal because their
  creation is serialized by the owner lock. It also prevents a delayed enqueue
  from an earlier request overwriting a newer share's URI. Reactivating an
  unresolved row retains its ID; subsequent delete/recreate obtains a new ID.
- Replacement resets retry state and scheduling as the existing queue does.
  A share → unshare → share sequence replaces an available job with the latest
  instance. Without a later share, the old job is suppressed by worker validation.
- Account for an executing job failing while a newer available job exists:
  retry/reclaim must discard the superseded generation instead of violating
  the partial unique index when moving it back to available. Apply this rule
  to both transient failures and recovery of abandoned executing jobs.
- Coalescing applies only while a job is available. There is no guaranteed
  debounce window: an executing job may already have sent, and SMTP retries
  can duplicate a message after an ambiguous acknowledgement. Revalidation
  narrows the removal race but cannot make a database read and SMTP atomic.
  Do not promise exactly-once delivery or cancellation of an in-flight email.

### Delivery guarantee

V1 deliberately keeps email best effort. The share commits before enqueueing;
a process crash in that gap, or an enqueue failure, can lose the email. The
share request still succeeds, and a roster re-POST does not recover the notice
because it is no longer a new share. Record enqueue failures as `enqueue_error`
and log them; a crash in the gap cannot reliably emit a metric. Successfully
enqueued jobs survive restarts and use the normal retry policy.

If eventual notification after every committed share becomes a requirement,
add an outbox entry in the sharing transaction and a dispatcher that retries
enqueueing. That stronger guarantee is outside v1.

## Metrics & logging

`share_notify_total{outcome}` includes `queued`, `sent`, `skipped_policy`,
`skipped_obsolete`, `invalid_recipient`, `no_mailer`, `no_web_url`,
`policy_error`, `enqueue_error`, `smtp_error`, and `retry_exhausted`.
`queued` measures accepted enqueue calls (including coalesced no-ops), not
unique messages; `sent` measures successful SMTP sends.

Log sends at Info. Expected skips (policy off, feature disabled, obsolete share,
owner/resource exclusions) are Debug, not Warn. Log unexpected configuration,
lookup, enqueue, and delivery failures at Warn with recipient and calendar path
when available, never the comment body. Alert on enqueue errors, policy lookup
errors, SMTP errors, and exhausted retries; ordinary policy skips are not alerts.

## Tests

- **Storage** (dev Postgres): `UpdateInvites` returns `Created=true` + the
  minted URI for a new sharee, `Created=false` for an access change, nothing
  for a removal; an unresolved href is reported with empty `Principal`.
  Concurrent identical POSTs both succeed but report only one new share;
  duplicate sets/aliases yield one final change; set-plus-remove yields no
  notice; an unresolved-to-local transition reports the usable share. Exercise
  rollback, commit failure, and concurrent decline/unshare against updates.
- **Plugin** (mock backend + capturing notifier):
  - new local sharee in an `email` domain → exactly one notice, correct
    instance URI and access level;
  - same roster re-POSTed → no second notice;
  - access change → none; removal → none; unresolved → none; owner → none;
    resource principal → none;
  - domain `off` / empty `-web-url` → none, share still succeeds;
  - canonical principal mailbox supplies both recipient and policy domain;
    submitted aliases do not change that choice; invalid mailbox → none;
  - policy lookup or enqueue failure → share request still 200, correct metric;
    re-POST after enqueue failure does not retry the lost notice (documented
    best-effort behavior).
- **Framing**: text/plain, no `text/calendar`, header-injection-safe names,
  comment capped, link built from `-web-url` + instance URI.
- **Queue**: worker send / retry / discard paths with a fake mailer; real
  Postgres tests for the share-specific index and enqueue ordering. Newer
  re-share replaces the pending URI, older delayed enqueue cannot overwrite
  it, equal generation is a no-op, and existing iMIP coalescing is unaffected.
  Removed/declined shares and changed mailboxes are suppressed; lookup failure
  retries; access downgrade changes the framed wording; policy disabled before
  execution skips delivery. Cover available versus executing job behavior,
  including retry/reclaim when a newer available job already exists.
- **calclient**: `parseRoute` reads `?cal=`; e2e — open
  `/cal/calendar?cal=<uri>` logged out, sign in, calendar is visible and
  highlighted, invitations panel open while the notification is pending.
  Verify canonicalization preserves the target, repeated renders do not repeat
  the action, and removed/unknown calendars or a different signed-in account
  show the unavailable message. A load failure retains the target for retry.

## Rollout

1. Serialize sharing mutations, normalize final changes, and return committed
   `[]ShareChange`; retain auto-accept behavior.
2. calclient `?cal=` route (useful on its own for deep links).
3. Domain policy column + cache; `-web-url` flag.
4. Share queue index/migration and enqueue path; `ShareNotifier`, worker
   validation/framing, and metrics. Document the best-effort delivery gap.
5. Enable for one domain, check delivery and spam placement, then open up.

Dormant by construction until a domain opts in **and** `-web-url` and a mailer
are configured.

## Follow-ups (out of scope)

- Share dialog: warn when an address has no local principal (the sharee comes
  back `invalid`) instead of silently storing a useless invite.
- Optional email to the owner when a sharee declines.
- Per-user opt-out overriding the domain policy.

## V1 decisions

- No email for access changes; a pending new-share notice uses current access
  at send time.
- Plain text only; an HTML alternative can follow.
- Best-effort enqueue after commit; a transactional outbox is deferred.
- Coalesce available notices and suppress obsolete shares; no exactly-once
  delivery guarantee.

## Implementation notes

Where it lives:

- `internal/storage/sharing.go` — `UpdateInvites` (owner-instance lock,
  normalization via `caldav.FinalSharees`, committed `[]ShareChange`),
  `ShareReply` (same lock), `SharedInstance` (worker revalidation).
- `internal/storage/domain_policy.go` — `DomainPolicy` + `CachedSharePolicy`
  (60 s TTL in `internal/server`).
- `internal/caldav/share_notify.go` — `ShareMailer` (screening, enqueue,
  `share_notify` handler, framing); wired in `internal/server/server.go`.
- `internal/jobs` — `Queue.EnqueueLatest`; `reschedule` / `reclaimOrphans`
  retire the superseded generation.
- `scripts/schema/pgsql.calserver.sql` — `domain_scheduling_policy`,
  `jobs_latest_ix`.
- calclient — `?cal=` in `router.js`, `applyCalTarget` in `cal/js/main.js`.

Turning it on for a domain (all three are needed):

```sql
INSERT INTO domain_scheduling_policy (domain, share_notify) VALUES ('example.org', 'email')
ON CONFLICT (domain) DO UPDATE SET share_notify = 'email', updated_at = now();
```

plus `[server] web_url` (or `-web-url`) and `[scheduling.imip] smtp`.

Where the implementation differs from or refines the text above:

- The coalescing index is generic: `jobs_latest_ix` on `(worker,
  args->>'coalesce_key')`, with the queue stamping `coalesce_key`
  (`<calendar id>|<principal>`) and `generation` into args. Any future
  latest-wins worker can reuse `EnqueueLatest`.
- Two extra metric outcomes: `skipped_excluded` (owner/resource) and
  `lookup_error` (principal or share lookup failed; retried by the worker).
- The invitations panel opens when *any* share notification is unanswered. A
  notification names the owner's calendar path, and the sharee's instance
  exposes no property linking back to it, so the client cannot match them
  exactly without a new DAV property. With auto-accept the pending notice is
  almost always the one the email was about.
- A manual `calctl` retry of a discarded `share_notify` job while a newer one
  is pending fails with a unique violation; the pending one is the one to keep.
- Policy rows are managed by SQL; no admin endpoint yet.
- Known, not addressed here: the iMIP coalesce index has the same
  retry/reclaim-versus-newer-pending collision that `share_notify` now handles;
  its reclaim sweep can fail as a whole if it occurs.
