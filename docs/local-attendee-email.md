# Per-domain email notification for local attendees

## Problem

CalDAV server-scheduling (RFC 6638) delivers an invitation to a **local**
attendee — one this server hosts — into their CalDAV *scheduling inbox*
(`/calendars/<user>/inbox/`), which their calendar client (including calclient's
invites panel, `cmd/calclient/web/cal/js/invites.js`) surfaces. No email is
sent. Only **remote** attendees (no local principal) get an email, via iMIP
(`internal/caldav/imip.go`).

Exchange/Outlook and Google instead unify mail and calendar, so every attendee —
internal included — effectively gets an email. Some domains want that
"you always get an email" behaviour; others are happy with inbox-only.

## Goal

A **per-domain, Postgres-backed** policy choosing how a local attendee is
notified, keyed on the **recipient's** email domain. It is the domain
operator's choice, applied to every user in the domain (a per-user override is
a plausible follow-up, out of scope for v1). Three mutually-exclusive states:

| policy         | behaviour for a local attendee                                            |
|----------------|---------------------------------------------------------------------------|
| `inbox_only`   | current RFC behaviour — scheduling-inbox item only, no email (**default**) |
| `email`        | inbox item **plus** the full actionable iTIP email (`text/calendar; method=REQUEST`) — Exchange-exact |
| `notification` | inbox item **plus** a plain notification email (no `METHOD`) — a heads-up that clients won't try to add |

`email` and `notification` are never both sent. `notification` avoids the
duplicate-calendar-entry hazard: an actionable `text/calendar` email can make a
mail client offer to add the event while the calendar client already added it
from the inbox; a `text/plain` heads-up cannot.

Unchanged by default: unlisted domains are `inbox_only`, so this is a no-op until
a domain opts in, and it is inert unless iMIP is configured.

## Data model

New table in `scripts/schema/pgsql.calserver.sql`:

```sql
CREATE TABLE domain_scheduling_policy (
    domain       TEXT NOT NULL PRIMARY KEY,           -- lowercased email domain
    local_notify TEXT NOT NULL DEFAULT 'inbox_only'
        CHECK (local_notify IN ('inbox_only','email','notification')),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- `domain` is stored lowercased; lookups lowercase the recipient's domain.
- Absent row → `inbox_only`.
- Migration snippet (for existing DBs, applied by the deployment) ships alongside the
  schema change.

## Storage layer

New `internal/storage/scheduling_policy.go`:

```go
type SchedulingPolicy struct { DB *sql.DB }

// LocalNotify returns the policy for a domain ("inbox_only" if no row).
func (s *SchedulingPolicy) LocalNotify(domain string) (string, error)

// admin writes
func (s *SchedulingPolicy) SetLocalNotify(domain, policy string) error
func (s *SchedulingPolicy) ListPolicies() ([]DomainPolicy, error)
func (s *SchedulingPolicy) DeletePolicy(domain string) error
```

**Caching.** Delivery runs per-attendee in the request hot path, and policies
change rarely. Wrap the store in a small TTL cache (e.g. 30–60 s per domain, or
load-all-and-refresh) invalidated on admin writes. v1 can ship a per-domain TTL
cache; correctness does not depend on instant propagation. The cache keeps the
*policy lookup* cheap; the SMTP send itself still runs synchronously inside the
organizer's PUT, once per local attendee in an opted-in domain — the same cost
external iMIP already pays, bounded by the durable delivery queue on failure.

**Errors fail open.** The cache wrapper swallows store errors into
`inbox_only` (that is why the delivery-side interface below returns a plain
string while the store returns `(string, error)`). Intentional: a DB hiccup
degrades a best-effort side notification to today's behaviour rather than
failing the scheduling write.

## Delivery wiring

Keep storage decoupled from caldav with a narrow interface (in
`internal/caldav`):

```go
// LocalNotifyPolicy reports how a local attendee's domain wants to be notified.
type LocalNotifyPolicy interface {
    LocalNotify(domain string) string // "inbox_only" | "email" | "notification"
}
```

- Add `LocalNotify LocalNotifyPolicy` to `SchedulePlugin`; wire it in
  `internal/server/server.go` (where `SchedulePlugin` is built, ~line 326)
  backed by the cached `storage.SchedulingPolicy`, only when a DB is present.
  Nil policy ⇒ everything is `inbox_only` (today's behaviour).

- In `scheduleLocalDelivery` (`internal/caldav/schedule_delivery.go:55`), **after**
  a successful inbox drop + calendar update (status already set to
  `1.2;Message delivered locally`):
  1. Skip unless `msg.SignificantChange` and the method is `REQUEST` or `CANCEL`
     (a REPLY to the organizer is not an "invitation"; decide separately — see
     open questions).
  2. Derive the recipient domain from `msg.Recipient` (strip `mailto:`, take the
     part after `@`, lowercase).
  3. `mode := p.LocalNotify.LocalNotify(domain)`; if `email` or `notification`,
     hand the message to the iMIP plugin for a **local-copy** send.

- **Auto-schedule principals (rooms/resources) never get a local-copy email** —
  nobody reads a room's mailbox, and they already auto-REPLY. The
  `autoProcess` branch returns before the hook point, which excludes them
  today, but the exclusion is a deliberate decision, not an accident of hook
  placement: keep it explicit (guard or test) so it survives refactors.

- The local-copy send must **not** touch `msg.ScheduleStatus`: local delivery
  already set the authoritative `1.2` status, and the email is a best-effort
  side notification. Its success/failure is logged and metered separately, never
  flipping the attendee's `SCHEDULE-STATUS` to an error.

Add to `IMipPlugin` a method that sends a copy without owning the status:

```go
// DeliverLocalCopy emails a local attendee a copy of an invitation they also
// received in their scheduling inbox. mode is "email" (actionable REQUEST) or
// "notification" (plain heads-up). It never sets msg.ScheduleStatus.
func (p *IMipPlugin) DeliverLocalCopy(r *http.Request, msg *ical.Message, mode string)
```

**Requires factoring the send core out of `deliverAsUser`.** As written,
`deliverAsUser` (`imip.go:262`) sets `msg.ScheduleStatus` on *every* path
(`5.3` not-authorized / no-credential, `1.1` on success, mapped errors on
failure) and records `IMipDelivery` metrics under the external-delivery
outcomes — it cannot be called from `DeliverLocalCopy` as-is without either
clobbering the `1.2` status or resorting to a save/restore of the field.
Instead, extract the transport core (sender authorization, credential lookup,
`SendAs`/relay send, retry-enqueue) into a helper that reports its outcome to
the caller; `deliverAsUser` keeps owning status + external metrics on top of
it, `DeliverLocalCopy` maps the same outcomes to the `local_copy` metric
dimension and logging only.

`SchedulePlugin` reaches it the same way it already finds `ExternalDeliverer`
(iterate `p.server.Plugins()` for the capability), or via a stored reference set
at wiring time.

## Email construction

- **`email` mode** reuses the existing `framedEmail` path — identical to the
  message sent to a remote attendee (`Content-Type: text/calendar; method=REQUEST`).
- **`notification` mode** needs a new framing, `notificationEmail(msg, recipient,
  subject)`:
  - `Content-Type: text/plain; charset=UTF-8` (optionally `multipart/alternative`
    with a minimal HTML part). **No `text/calendar` part and no `METHOD`**, so no
    client auto-adds it.
  - Body: human-readable, per method — `REQUEST`: *"You've been invited to
    «SUMMARY» on «when» by «organizer». Open your calendar to respond."*;
    `CANCEL`: *"«organizer» cancelled «SUMMARY» on «when»."* Reuse the existing
    subject logic (`Invitation:` / `Cancelled:` + summary).
  - **Every** interpolated value (summary, organizer name/address, when) goes
    through `stripHeaderChars` (header-injection defence), exactly as
    `framedEmail` already does.

## Send-as-whom / transport

The copy goes through the factored-out send core (see above): sent as the
logged-in organizer over Submission (or the resource relay for a room
organizer), the same authorization model as external iMIP — no new privilege.
On a legacy relay-only deployment (`Mailer` set, `Submission` nil), fall back
to `deliverViaRelay` framing exactly as `DeliverExternal` does — the transport
choice is a deployment property, not a per-feature one. If a domain is
`email`/`notification` but neither is configured, the copy is skipped, logged,
and metered as `no_mailer` while inbox delivery still succeeds.

Transient send failures use the same durable delivery path external iMIP uses
(the background job queue — see `docs/imip.md`), but queued local-copy retries are
metered under the `local_copy` dimension, not the external outcomes.

## Admin management

Runtime-editable via the admin app (`internal/admin`), mirroring the
`createUser` / `setPassword` pattern (BasicAuth-protected):
- `GET /api/domain-policies` — list.
- `POST /api/set-domain-policy` — `{domain, local_notify}`.
- `POST /api/delete-domain-policy` — `{domain}`.
- Optional: a small admin UI page listing domains with a policy dropdown.

Also editable directly by SQL for provisioning.

## Metrics & logging

Extend the iMIP metrics with a `local_copy` outcome dimension
(`sent` / `skipped_insignificant` / `no_mailer` / `failed` / `not_authorized`)
so local-copy sends are observable separately from external iMIP. Reuse
`logDelivery`.

## Tests

- **Storage** (dev-Postgres, `parity_test` style): CRUD, lowercasing, default
  `inbox_only` fallback, CHECK constraint rejects bad values.
- **Delivery** (`MockSchedulingBackend` + a fake `LocalNotifyPolicy` + a
  capturing mailer):
  - `inbox_only` → inbox item, **no** email.
  - `email` → inbox item + captured `text/calendar; method=REQUEST` email.
  - `notification` → inbox item + captured `text/plain` email with **no**
    `METHOD` and no `text/calendar` part — only the human-readable
    summary/when/organizer line.
  - insignificant change → no email in any mode.
  - a failed local-copy send leaves `SCHEDULE-STATUS` at `1.2` (delivered
    locally), not an error.
  - policy keyed on **recipient** domain (organizer's domain irrelevant); an
    event with attendees across domains notifies each per its own domain.
  - an auto-schedule (resource) recipient in an `email` domain → no email,
    pinning the deliberate exclusion.
- **Framing**: `notificationEmail` emits no `METHOD`, correct content-type,
  header-injection-safe.

## Rollout

1. Add table to `scripts/schema/pgsql.calserver.sql` + a migration snippet.
2. Ship storage + cache, the `LocalNotifyPolicy` interface, and wiring (default
   `inbox_only` ⇒ behaviour unchanged).
3. Factor the send core out of `deliverAsUser`; add `DeliverLocalCopy` +
   `notificationEmail`.
4. Admin endpoints (+ optional UI).
5. Metrics/logging.
6. Tests throughout.

Safe by construction: no domain opts in until an admin sets a row, and the whole
feature is dormant without an iMIP mailer.

## Security notes

- **Header injection**: all untrusted event fields run through
  `stripHeaderChars` (existing pattern).
- **Amplification**: one copy per local attendee, already bounded by the
  group-expansion cap (`maxExpandedAttendees`) and significant-change gating.
- **No new leak**: the email goes to the attendee about an event they were
  invited to; sent as the organizer via their own submission credential.

## Open questions / decisions

1. **REPLY handling** — apply local-copy emails to `REQUEST` and `CANCEL` only,
   or also notify a local organizer of `REPLY`s? Proposed: `REQUEST` + `CANCEL`
   for v1; revisit REPLY.
2. **Global default** for unlisted domains — proposed `inbox_only` (table
   default); a config override (`scheduling.local_notify_default`) is possible
   but probably unnecessary.
3. **`email` mode dupes** — accepted as the user's explicit choice; documented so
   operators know `notification` is the dedup-safe option.
4. **Admin UI now or SQL-only for v1** — endpoints are cheap; a UI page can
   follow.
