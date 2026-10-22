# Plan: Working Hours via VAVAILABILITY (RFC 7953)

## 0. Purpose

calweb's event editor is getting a **Suggested times** list next to the
date/time fields. To suggest a slot that works for all attendees, it needs
their busy times, which the outbox `POST` VFREEBUSY probe already provides,
**and their working hours**. Without working hours the client has to assume
something like "Mon–Fri 9–17 in the organizer's zone", and that is wrong as
soon as attendees live in different time zones or keep different hours. A
Berlin organizer would get a suggested 16:00 that is 07:00 for a guest in
New York.

Only the server knows each user's real hours and zone. This plan adds them in
the standard form: a per-user VAVAILABILITY stored on the schedule inbox.
Outbox free/busy then reports time outside those hours as
`FBTYPE=BUSY-UNAVAILABLE`, and calweb never suggests those slots. Because the
availability's AVAILABLE blocks are expanded in **the user's own TZID**, DST
is handled per attendee. A 09:00–17:00 Berlin day is 07:00–15:00Z in summer
and 08:00–16:00Z in winter, and the US and EU transition weeks don't line up.

Apple Calendar reads and writes the same property (CalendarServer's
`calendar-availability` on the inbox), so its "Availability" settings
interoperate for free.

## 1. Hard constraints

- **Own table, no sabre-owned table changes.** sabre is no longer a rollback
  target, so availability doesn't have to be a dead property in the shared
  `propertystorage` table. It gets its own calendsd table,
  `calendar_availability` (§3.1), in `scripts/schema/pgsql.calserver.sql`.
  Nothing changes in `users`, `principals`, `calendarinstances` or
  `propertystorage`.
- **Free/busy output format stays as it is.** `GenerateFreeBusy` is pinned
  against sabre in `testdata/freebusy-golden.ics`. Availability adds periods,
  but how BUSY/TENTATIVE periods are written (start/end, `FBTYPE` omitted for
  BUSY) is unchanged.
- **No new information leak.** Availability is only applied where the
  requester already passed the `read-free-busy` check in
  `freeBusyForPrincipal`. A denied or cross-domain probe still gets
  `3.7;Could not find principal` with no data (§5).
- **Auto-schedule behavior is unchanged.** `schedule_delivery.go` decides a
  room's accept/decline with `bytes.Contains(fb, "\r\nFREEBUSY")`. If
  availability were folded into that call, every room would decline every
  out-of-hours booking. In v1 availability is a separate input that the
  auto-schedule path does not pass (§4). This holds as long as
  `GenerateFreeBusy` and `schedule_delivery.go` are left alone; the §7 test
  pins it against later refactors.
- **Times are resolved in the availability's own zone, never in UTC or the
  server's zone.** Floating times are rejected (§3.3).

## 2. Scope

**In v1:**

- `PROPFIND`/`PROPPATCH` of the availability property on the inbox, with
  validation.
- One or more VAVAILABILITY components with optional DTSTART/DTEND (either
  may be unbounded).
- AVAILABLE subcomponents with DTSTART + DTEND/DURATION, RRULE, RDATE, EXDATE
  and RECURRENCE-ID overrides (CalDAVTester's `1.ics` uses an override).
  RDATE adds instances to an RRULE series. Without an RRULE, the AVAILABLE is
  its single DTSTART instance, the same as for events in `eventBusyPeriods`.
- `PRIORITY` between overlapping VAVAILABILITY components (RFC 7953 §4, as
  sabre does it): a component decides all the time it covers over every
  lower-priority one, and 0 or no PRIORITY ranks below 9. Components of equal
  priority combine, so time unavailable in any of them is unavailable.
- IANA TZIDs, plus non-IANA TZIDs with an embedded VTIMEZONE (Outlook style),
  expanded in that zone's wall-clock time (§4.1).
- `BUSY-UNAVAILABLE` in **outbox POST** free/busy.
- calweb: a "Working hours" settings section and Suggested times that use
  `BUSY-UNAVAILABLE`.

**Not in v1 (deliberate):**

- VAVAILABILITY stored as calendar objects. PUT keeps returning 403 (sabre
  behavior; CalDAVTester's "Verify cannot PUT VAVAILABILITY" expects that).
- `BUSY-TYPE` values other than the default `BUSY-UNAVAILABLE`.
  `BUSY-TENTATIVE`/`BUSY` are accepted on write but reported as
  BUSY-UNAVAILABLE.
- Availability in the per-calendar `free-busy-query` REPORT. Availability is
  per user, not per calendar, and CalendarServer doesn't apply it there
  either.
- Rooms honoring their own hours in auto-schedule (§9).

## 3. Server: storing the property

### 3.1 Storage

A new table in `scripts/schema/pgsql.calserver.sql`, following the
`contact_frecency` convention (keyed by the principal URI, no FK to sabre's
tables):

```sql
CREATE TABLE IF NOT EXISTS calendar_availability (
    principal  VARCHAR(320) NOT NULL PRIMARY KEY, -- 'principals/alice@example.org'
    data       TEXT         NOT NULL,             -- the VCALENDAR, CRLF line endings
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);
```

Its columns go into the `check.pgsql.calserver.sql` column list. A
`caldav.AvailabilityBackend` interface (`Availability(principal)`,
`SetAvailability(principal, data)`, `DeleteAvailability(principal)`) is
implemented by `storage.CalDAV`, so handler tests can use the in-memory fake
as they do for other backends.

One row per principal holds the whole VCALENDAR, every VAVAILABILITY
component included (§2 layers them by `PRIORITY`).

### 3.2 Property names

- `{http://calendarserver.org/ns/}calendar-availability`: what Apple and
  CalDAVTester use.
- `{urn:ietf:params:xml:ns:caldav}calendar-availability`: the RFC 7953 §7.2.4
  name.

Both are a **live** property of the `*Inbox` node, served by
`SchedulePlugin` from the one row:

- **PROPFIND:** `SchedulePlugin.PropFind` already runs for every node. For an
  `*Inbox` it calls `pf.Handle` for both names. `Handle` only resolves names
  the request asked for, so the property is automatically left out of
  `allprop` (as CalendarServer does), and `allprop` with `<include>` still
  returns it. The backend is only queried when one of the names was
  requested. Unset → 404 (CalDAVTester `PROPPATCH VAVAILABILITY/1`).
- **PROPPATCH:** see §3.3.

### 3.3 Validation and writes (PROPPATCH)

`SchedulePlugin` implements `dav.PrePropPatchHook`. For an `*Inbox` node it
claims whichever of the two names are being changed. The claim has to happen
in the pre-hook: otherwise `PropertyStorage` takes them through
`HandleRemaining` and they end up as dead properties.

1. **Validate first, inside the hook, before any callback runs.** If the
   value is invalid, `pp.SetResultCode(names, 403)`. That marks the PROPPATCH
   as failed, `Commit` runs no callback, and every other property in the
   request gets 424. Nothing is stored.
2. **Valid:** `pp.Handle(names, …)` with a callback that writes to (or, for
   `remove`, deletes from) `calendar_availability`.

**Transactions.** This callback and `PropertyStorage`'s commit in separate
transactions. Every *validation* failure is caught in step 1, before either
runs, so all-or-nothing holds for everything a client can cause. Only an
infrastructure error in the second transaction (the database going away
mid-request) could leave the first write in place. That's acceptable for a
settings property, and it's the same trade-off every PROPPATCH that touches
two backends already has. Clients in practice (Apple, calweb) send this
property on its own.

**Error response.** `PropPatch` can only report a status code per property.
It has no way to attach a `CALDAV:valid-calendar-data` error element to a
propstat, so v1 returns a plain 403 and logs the reason at debug level.
Per-propstat error elements would be a separate change to the PROPPATCH
multistatus writer, and v1 doesn't need it (CalDAVTester only checks the
status).

**Line endings.** The XML parser turns the body's CRLFs into bare LFs, and
go-ical needs CRLF. The hook runs `ical.NormalizeCRLF` before validating and
**stores the normalized value**, so every later reader gets CRLF and parses it
directly. Apart from line endings the stored value is exactly what the client
sent; validation never rewrites it, so Apple's round-trip is unchanged.

The value is rejected if any of these hold:

- It doesn't parse as iCalendar (CalDAVTester `16.xml`: "This is not a
  calendar!").
- It has a top-level component other than VTIMEZONE/VAVAILABILITY, or no
  VAVAILABILITY at all.
- A date-time is **floating** (no TZID and no `Z`). The server can't tell
  whose 09:00 it is, which is the whole problem this plan solves.
- An AVAILABLE or VAVAILABILITY uses a `DATE` value (all-day). There's no
  sane meaning without a zone; revisit if a client actually sends one.
- A TZID resolves neither through `time.LoadLocation` nor through an embedded
  VTIMEZONE.
- A VAVAILABILITY or AVAILABLE has no UID or no DTSTAMP (both required by
  RFC 7953).
- Two AVAILABLE components in the same VAVAILABILITY share a UID and neither
  has a RECURRENCE-ID. Expansion groups by UID with one master per UID, so the
  second master would silently replace the first (`expand.go` does exactly
  that for VEVENTs).
- An AVAILABLE has no DTSTART or no DTEND/DURATION, or its end is ≤ its start.
- An RRULE uses `FREQ=SECONDLY|MINUTELY|HOURLY`, or `BYHOUR`, `BYMINUTE` or
  `BYSECOND` (which turn a DAILY rule back into a sub-daily one). Working
  hours repeat daily at most, and that bound is what keeps a series anchored
  years back within `maxAvailabilityScan` (100,000 instances).
- A series doesn't stay within `maxAvailabilityScan` for 10 years from now
  (validation also expands a week then), so an accepted value can't silently
  stop applying later. A stored value that hits the cap anyway is treated as
  unusable (plain free/busy), never as "unavailable for the rest of the
  window".
- A VAVAILABILITY `PRIORITY` that isn't 0–9, or a `DURATION` without DTSTART,
  next to DTEND, or not positive.
- The value is larger than 64 KiB.
- The request sets or removes both property names at once. They name the same
  row, so the two writes would conflict.

### 3.4 Discovery

- `SchedulePlugin.Features()` adds `calendar-availability` and
  `inbox-availability` to the DAV header (CalDAVTester `Capabilities/1`; the
  first token is the RFC 7953 one). It adds them only when the calendar
  backend implements `AvailabilityBackend`, so a server without the store
  doesn't advertise it.
- `allprop` exclusion comes for free from the live property (§3.2).

### 3.5 Lifecycle

- **User deletion.** `DeleteUserCascade` deletes the user's
  `calendar_availability` row in its transaction, next to the
  `contact_frecency` delete, and so does `DeleteUser`. Neither relies on the
  doctor. The soft-delete purge goes through `DeleteUserCascade`, so it's
  covered.
- **Doctor.** Add a `calendar_availability` check for rows whose principal no
  longer exists (deletable through `DeleteOrphans`, like the others), in case
  a row is ever written outside the cascade.
- **One-time import of existing dead properties.** Before this plan, Apple
  clients could already PROPPATCH `calendar-availability` onto the inbox, and
  those values sit in `propertystorage` at `calendars/<user>/inbox`. A
  re-runnable SQL step in `pgsql.calserver.sql` moves them over: for every
  such row whose principal exists and has no `calendar_availability` row yet,
  insert the value (CRLF-normalized) and delete the `propertystorage` row.
  Values aren't validated at import time; one that fails to parse is handled
  like any corrupt row (§4.3: warn, no availability). The user sees it in
  calweb as "set in another app" (§6.1) and can replace it. A NULL or non-UTF-8 value
  can't become TEXT. It stays in `propertystorage` with a NOTICE rather than
  stopping the schema run. After the import,
  `SchedulePlugin` claims both names on the inbox, so `propertystorage` never
  receives them again.
- The doctor fix already made for inbox/outbox `propertystorage` rows stays.
  It protects any other dead properties clients have put on the inbox.

## 4. Server: free/busy computation

### 4.1 Expansion in `internal/caldav/ical` (`availability.go`)

```go
// unavailablePeriods returns the parts of [start, end) that the
// VAVAILABILITY data marks as unavailable: for each component, its coverage
// minus the union of its AVAILABLE instances, unioned across components.
func unavailablePeriods(availability []byte, start, end time.Time) ([]busyPeriod, error)
```

It's unexported. `GenerateFreeBusyWithAvailability` (§4.2) and
`ValidateAvailability` (§3.3) are the entry points, and every error wraps
`ErrInvalidAvailability`.

Algorithm, all in absolute time (UTC instants):

1. Decode (the stored value already has CRLF line endings, §3.3).
2. For each VAVAILABILITY: coverage = `[DTSTART or -∞, DTEND or +∞) ∩
   [start, end)` (DTSTART+DURATION is accepted in place of DTEND). If it's
   empty, skip the component.
3. Group its AVAILABLE components by UID: one master plus its RECURRENCE-ID
   overrides. Validation guarantees one master per UID (§3.3); a second
   master that got in anyway (an imported value) is ignored rather than
   replacing the first. Each override contributes its own span and removes
   the instance it replaces.
4. Expand each master **in its own zone**. For an IANA TZID, rrule runs in that
   `time.Location`, which keeps every instance at its local time. That one
   step is the DST correctness. The expansion reuses `recurrenceSet` (so
   comma-list EXDATE/RDATE work) and `masterDurationIn`. With an embedded
   (non-IANA) zone the rule runs on that zone's bare wall clock, and
   DTEND/EXDATE/RDATE/UNTIL written in UTC or another zone are moved onto
   that clock first, so they still name the right instances.
5. Expansion starts at the rule's DTSTART (a weekly rule from 2019 is normal)
   and is bounded by the window end and `maxAvailabilityScan`. It deliberately doesn't use `Expand`/`expandGroup`:
   those stop at `maxRecurrences` (3500), which a weekday rule reaches after
   about 13 years, and they build cloned components where intervals are
   enough.
6. unavailable = coverage − ⋃ instances (sort, subtract), FBTYPE
   `BUSY-UNAVAILABLE`, merged across components (the v1 rule, §2).

**Non-IANA TZIDs (Outlook style).** `resolveNonIANATZIDs` isn't used: it
rewrites such times to fixed UTC values, so a weekly rule would drift by an
hour at every DST change, and it only looks at top-level components, never
the AVAILABLE inside a VAVAILABILITY. Instead, an AVAILABLE in a zone defined
only by its embedded VTIMEZONE is expanded on the bare wall clock, and each
instance is placed with that VTIMEZONE's offset for its wall time
(`parseVTimezone`/`offsetAt`). A UTC `UNTIL` in the rule is moved onto
the same wall clock first, so the series ends at the right instance.

Writing the test for this found a bug in `parseVTimezone`, now fixed and
covered by `TestNonIANATZIDSummerOffset`. Outlook anchors its VTIMEZONE rules
in 1601, and rrule-go stops a series about 292 years after its DTSTART (the
range of a `time.Duration`). So the transitions ended in 1893, and every
Outlook-zone **event** in summer was read with the winter offset, an hour off.
Anchors before 1970 are now moved to 1970 before expanding.

### 4.2 Wiring into `GenerateFreeBusy`

Extract the body into an internal `generateFreeBusy(objects, extra
[]busyPeriod, …)`. `GenerateFreeBusy` keeps its exact signature and passes no
extra periods, so the REPORT, auto-schedule and golden tests are untouched. A
new `GenerateFreeBusyWithAvailability(objects, availability []byte, …)` calls
`unavailablePeriods` and passes the result through. If the availability is
unusable it returns an `ErrInvalidAvailability` error, and the outbox falls
back to plain `GenerateFreeBusy` (§4.3). BUSY-UNAVAILABLE periods
go through the existing per-FBTYPE `coalesce`, so a BUSY event outside hours
is still listed as BUSY. That matches CalendarServer, and the client can
ignore the overlap.

### 4.3 Callers

| Path | Availability? | Why |
|---|---|---|
| Outbox POST (`freeBusyForPrincipal`) | **Yes** | The scheduling probe; what Suggested times uses |
| `free-busy-query` REPORT on a calendar | No | Per calendar, not per user (§2) |
| Auto-schedule conflict check | No | Would make rooms decline out-of-hours (§1) |

`freeBusyForPrincipal` reads the principal's `calendar_availability` row
(through `AvailabilityBackend`) **only after** `readable` is true. An all-private
principal (every calendar transparent) still reports its hours. Hours aren't
calendar content, and "free but only 9–17" is the honest answer. If the
stored value fails to parse (for example a value imported from
`propertystorage` without validation, §3.5), log at warn and fall back to no
availability. It must never cause a 5.0 status.

## 5. Privacy

Working hours are visible to exactly the set of people who can already probe
the user's free/busy (same-domain principals, `acl.SameDomain` on
`read-free-busy`). They show *when* someone works, not *what* they do, the
same level of detail as busy blocks. Cross-domain and external requesters see
nothing new. The PROPFIND/PROPPATCH on the inbox is protected by the inbox's
existing owner-only ACL. Add a test that another user's PROPFIND on
`/calendars/alice/inbox/` gets 403/404, not the value.

## 6. calweb

### 6.1 Settings → "Working hours"

- A per-weekday on/off toggle plus one start–end range per day (one range per
  day covers the Outlook model; multiple ranges can come later). A time zone
  picker defaults to `Intl.DateTimeFormat().resolvedOptions().timeZone`.
- On save, build **one** VAVAILABILITY with no DTSTART/DTEND (it applies
  indefinitely) and one AVAILABLE per distinct range, with
  `RRULE:FREQ=WEEKLY;BYDAY=…` and `DTSTART;TZID=<zone>` anchored on a past
  date that falls on the first BYDAY. The anchor matters: under RFC 5545 the
  DTSTART is always an instance even if its weekday isn't in BYDAY, so a
  Monday-only range anchored on a Sunday would also mark that one Sunday as
  available. Give the VAVAILABILITY and every AVAILABLE its own fresh UID and
  a DTSTAMP (RFC 7953 requires both, and the server rejects a shared master
  UID, §3.3). Include the VTIMEZONE (Apple expects one). PROPPATCH it to the
  user's inbox (`schedule-inbox-URL`).
- PROPFIND may return the value with LF line endings (XML output, and
  the XML parser on the client side). Normalize to CRLF
  before parsing it.
- On load, PROPFIND the property. If it parses into that shape, fill in the
  form. If it doesn't (for example Apple wrote something richer), show
  "Working hours were set in another app" with a "Replace" action, rather
  than overwriting silently.
- "Clear working hours" sends a PROPPATCH `remove`.

### 6.2 Suggested times

- Probe the organizer **and** all internal guests in one outbox POST over the
  search window (for example the selected day ± 7 days).
- Parse `FREEBUSY` lines with their `FBTYPE`. BUSY, BUSY-UNAVAILABLE and
  (configurably) BUSY-TENTATIVE block a slot.
- Candidate slots step through the window at 15/30-minute granularity with
  the event's current duration. Rank by: all required guests free > earliest.
  Show the top 5, each with a one-line reason when not perfect ("outside
  Bob's working hours" is now something we *can* say).
- **Attendees with no hours set:** their response contains no
  BUSY-UNAVAILABLE anywhere in a window of at least a week. For them, apply
  the organizer's own hours as an assumption and label the suggestion "Bob's
  working hours unknown". That way suggestions degrade to the pre-plan
  behavior instead of turning into 3am slots.
  This is a heuristic and can be wrong both ways. A user with hours set
  produces no BUSY-UNAVAILABLE if their hours cover the whole window (24/7)
  or their VAVAILABILITY is bounded and has already ended. A user without
  hours can produce some, because a stored VFREEBUSY object keeps its own
  `FBTYPE` (`freeBusyComponentPeriods`). The first case is harmless (the
  fallback is applied to someone who is in fact free). The second case means
  we trust the stored data. Neither is worth changing the free/busy output
  format (§1) to carry an explicit "hours set" flag in v1.
- External guests (status 3.7) are ignored for ranking and listed as
  "availability unknown".
- Display every candidate in the **viewer's** zone. With mixed zones, show
  each guest's local time on hover ("10:00 for you · 04:00 for Bob").

## 7. Testing

**`internal/caldav/ical` (unit):**

- Mon–Fri 09–17 `Europe/Berlin`, window across 2026-10-25: the unavailable
  boundaries move from 15:00Z/07:00Z to 16:00Z/08:00Z on the right day.
- Same rule in `America/New_York` across 2026-11-01, and both together in one
  probe across the 2026-10-25…11-01 week, when the zones are 5h apart instead
  of 6h. This is the case the plan exists for.
- Spring forward: an AVAILABLE starting 02:30 on a gap day (2026-03-29 Berlin)
  lands where `recurrenceSet` already places gap times. Pin the behavior.
- The `1.ics` fixture: bounded VAVAILABILITY, RECURRENCE-ID override moving
  Friday to 12–17.
- Unbounded start/end, EXDATE, RDATE, window entirely outside coverage (no
  periods), window entirely inside an AVAILABLE (no periods).
- Non-IANA TZID + VTIMEZONE (`W. Europe Standard Time`), as in
  `TestBug6NonIANATZIDWithVTimezone`.
- Golden: `GenerateFreeBusy` output unchanged (existing test), plus a new
  golden for `GenerateFreeBusyWithAvailability`.

**`internal/caldav` (handler):**

- PROPPATCH valid/invalid/floating/DATE/oversized/MINUTELY/HOURLY/missing UID or
  DTSTAMP/duplicate AVAILABLE master UID/both names in one request → 403 and
  nothing stored. Invalid availability plus a valid other property in the
  same request → 403 + 424, and neither stored (all-or-nothing, §3.3).
- A value sent with CRLF line endings validates (the XML parser has turned
  them into LF by then) and is stored with CRLF.
- PROPFIND unset→404; set via either name → value under both names; remove
  via either name clears it. After the write, the `propertystorage` table has
  no row for the inbox (the live property claimed it).
- `allprop` PROPFIND on the inbox does not include the property; `allprop`
  with `<include>` of either name does.
- Outbox POST: owner with hours → BUSY-UNAVAILABLE; cross-domain probe → still
  3.7, no data; corrupt stored value → no availability, 2.0.
- Auto-schedule: a room with an availability property still accepts an
  out-of-hours booking (pins §1).
- `free-busy-query` REPORT ignores availability.
- Another user cannot read or write `/calendars/alice/inbox/`'s property.
  The inbox ACL only grants other authenticated users `schedule-deliver-*`,
  so this should pass without new code; the test pins it.

**`internal/storage`:**

- Backend round-trip: set, replace, get, delete.
- `DeleteUserCascade` and `DeleteUser` remove the row.
- Doctor: an availability row for a missing principal is flagged and
  deletable; a live one isn't.
- Import step: an inbox `calendar-availability` row in `propertystorage`
  moves to the table with CRLF line endings; re-running is a no-op; a row
  for a missing principal is left for the existing orphan check.
- (Done) `orphanedPropertyRows`: an inbox/outbox row is kept while the
  principal exists and flagged once it's deleted.

**CalDAVTester:** add `vavailability` to the features in
`scripts/caldavtester/serverinfo-overrides.py` and `CalDAV/availability.xml`
to `tests.txt`. Expect two known failures unrelated to correctness, and list
them in `scripts/caldavtester/README.md`:

- `Capabilities/2` expects VTIMEZONE and VFREEBUSY in
  `supported-calendar-component-set` (CalendarServer default; ours is
  `VEVENT,VTODO`).
- `free-busy reports/1` matches CalendarServer's exact text
  (`FBTYPE=BUSY:…/PT1H`, comma-joined periods). Our sabre-pinned format
  carries the same periods written differently (§1). The unit tests cover the
  same scenario.

## 8. Rollout

Steps 1–3 are implemented. Applying `pgsql.calserver.sql` (which creates the
table and moves existing dead properties) must happen **before** deploying the
server, because user deletion now deletes from `calendar_availability`.

1. `ical` expansion + `GenerateFreeBusyWithAvailability` + the DST unit tests
   (plus the Outlook VTIMEZONE fix, §4.1). No behavior change yet.
2. `calendar_availability` table, backend, import step, cascade deletes and
   doctor check (§3.1, §3.5). Then the live inbox property: PROPFIND,
   PROPPATCH validation and write, DAV features (§3.2–§3.4). (The doctor's
   inbox/outbox orphan fix for `propertystorage` is already in.)
   The inbox's privilege tree had no `write-properties` under `{DAV:}write`,
   so even the owner's PROPPATCH on the inbox was refused. It now matches
   sabre's default set (`write-properties`, `write-content`, `bind`,
   `unbind`).
3. Outbox POST uses it. Enable the CalDAVTester suite.
4. calweb Working hours settings.
5. calweb Suggested times consumes BUSY-UNAVAILABLE (§6.2 fallback for users
   without hours).

Steps 1–3 can ship before calweb changes. Apple Calendar users get correct
BUSY-UNAVAILABLE as soon as step 3 is deployed.

## 9. Later / open questions

- **Server-side default hours.** A config `default_working_hours =
  "MO-FR 09:00-17:00"` applied in the zone of each user's default calendar
  (`calendar-timezone`, as `calendarTimeZone` already reads it) for users
  without the property. That would make the client's "hours unknown"
  fallback rarely needed. It needs a decision on whether a synthesized
  availability should be visible to PROPFIND (probably not).
- **Rooms with hours.** Let auto-schedule decline bookings outside a room's
  availability, opt-in per room, with an explicit decline reason instead of
  overloading the busy-substring check.

## 10. Who can see your free/busy (per-user visibility)

Today `read-free-busy` on every calendar is a protected ACE for
`acl.SameDomain`: everyone in your mail domain sees your busy time and working
hours, and nobody else does. Users can hide single events (TRANSP) or whole
calendars (`schedule-calendar-transp`), but not choose *who* sees them.
Outlook's model is a default for the organization plus per-person grants. v1
takes its free/busy-only level and adds the "who".

Implemented server-side; the calweb part (§10.3) is still open.

### 10.1 Model

One setting per **user**, not per calendar. The outbox answer combines all
calendars and shows working hours as soon as any calendar is readable (§4.3),
so per-calendar grants would leave "who sees my hours" undefined.

- **Audience:** `domain` (the default, today's behavior) or `nobody`.
- **Allowed:** a list of principals (people or groups) who may always see it,
  whatever the audience. A grant can name someone outside your domain, by
  address (§10.3). That's an explicit choice by the owner.
- The owner always sees their own free/busy. Delegates keep seeing it through
  `{DAV:}read`, which includes `read-free-busy`.
- No title/location/details levels. Seeing more than busy time is calendar
  sharing, which exists already.

### 10.2 Enforcement

A new pseudo-principal `acl.FreeBusyAudience` replaces `acl.SameDomain` in
`Calendar.ACL()`'s `read-free-busy` ACE. The ACL plugin resolves it through an
`acl.FreeBusyPolicy` (the owner's setting):

1. The requester is the owner → yes.
2. The requester or one of their groups is in Allowed → yes.
3. Audience `domain` → the same-domain test as before.
4. Audience `nobody` → no.

No setting means `domain`. If the policy can't be read, the answer is **no**:
an outage can hide free/busy but never widen it.

Everything that checks `read-free-busy` goes through this one rule: the outbox
POST, the `free-busy-query` REPORT and `current-user-privilege-set`. They can't
disagree, and working hours follow automatically. In practice the outbox is
what other people reach. `BeforeMethod` gates every REPORT with `{DAV:}read`
(stricter than sabre), so `free-busy-query` stays owner-and-delegate only,
whatever the visibility. A hidden user is answered
`3.7;Could not find principal`, exactly like an unknown address, so the
setting can't be used to find out who exists (§5).

### 10.3 Storage and protocol

- **Table** `free_busy_visibility (principal PK, audience CHECK IN
  ('domain','nobody'), allowed TEXT[] of principal URIs, updated_at)` in
  `pgsql.calserver.sql`. Deleting a user deletes their row and removes them
  from everyone's Allowed list. The doctor flags a row whose principal is gone.
- **Property** `{urn:x-calserver}free-busy-visibility`, a live property on the
  inbox and owner-only like `calendar-availability`. It's always present; an
  unset setting reads as the default:

  ```xml
  <x:free-busy-visibility xmlns:x="urn:x-calserver" xmlns:d="DAV:">
    <x:audience>nobody</x:audience>
    <x:allow>
      <d:href>/principals/bob/</d:href>
      <d:href>mailto:team@example.org</d:href>
    </x:allow>
  </x:free-busy-visibility>
  ```

  The allowed list must not reveal which accounts exist outside the owner's
  domain, which the server hides everywhere else (listings, the `3.7`
  answer):
  - **A `mailto:` address belonging to a principal in the owner's domain** is
    stored as that principal's URI, so a group grant covers its members.
  - **Any other well-formed address** is stored as the lower-cased address,
    whether or not an account has it, and matched against the requester's
    calendar-user addresses at check time. So a cross-domain grant is by
    address, and cross-domain groups aren't supported.
  - **A principal path** is accepted only for a principal in the owner's
    domain. A path to another domain gets the same 403 as one that doesn't
    exist.

  At most 200 entries. An unknown audience, a refused href, a malformed
  address or too many entries → 403, nothing stored. `remove` restores the
  default. Deleting a user removes both their URI and their address from
  every list.
- **calweb:** a "Who can see when I'm busy" section in Settings, next to
  Working hours: Everyone in my organization / Only the people below, plus a
  people picker. It also needs the calendar transparency toggle ("Don't show
  this calendar as busy") and "Show as: Free" in the event editor, both of
  which the server already supports.
