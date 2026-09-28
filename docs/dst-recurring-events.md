# Recurring events shift by an hour across DST

## Problem

A recurring event created in calweb shows at the wrong local time after a
daylight-saving change: a weekly 09:00 meeting created in winter shows at 10:00
in summer (and vice versa). Every client shows the same shift, since the
stored data is wrong. Apple Calendar, Thunderbird and iMIP invitations are
affected alongside calweb.

### Evidence

The same weekly series, stored two ways, expanded by the server's own expander
(`internal/caldav/ical.Expand`) across the 29 March 2026 switch, shown in
Europe/Zurich:

| DTSTART as stored | Mar 13 | Mar 20 | Mar 27 | Apr 3 |
|---|---|---|---|---|
| `DTSTART:20260313T080000Z` (what calweb writes) | 09:00 | 09:00 | 09:00 | **10:00** |
| `DTSTART;TZID=Europe/Zurich:20260313T090000` | 09:00 | 09:00 | 09:00 | 09:00 |

### Root cause

calweb writes every timed date-time in UTC:

- `eventLines` (`cmd/calclient/web/shared/ical.js`): ``v => `:${utc(v)}` ``
  for DTSTART/DTEND of new events, "only this" overrides and "this and
  following" tails.
- The plain-event branch of the editor (`cal/js/event-dialog.js`, the
  `DTSTART: … ical.utc(fields.start)` props): this is also the path that turns a
  one-off event into a series by adding an RRULE.
- `shiftDate` (`shared/ical.js`) shifts UTC values only, and returns `null`
  for a TZID value, which the callers read as "can't shift".
- `ridValue` (`shared/store/events.js`) and `addExdate` write RECURRENCE-ID
  and EXDATE in UTC.

RFC 5545 expands an RRULE in the time zone of DTSTART. A UTC DTSTART pins
every occurrence to the same UTC instant, so its local time moves by the DST
offset. The server is correct; the data is wrong.

One-off events are not affected (a single instant is correct in any form),
and all-day events use `VALUE=DATE` and don't move either.

## Goals

- Timed events calweb writes carry local wall-clock time and the user's zone:
  `DTSTART;TZID=Europe/Zurich:20260313T090000`. Recurrences then stay at the
  same local time all year.
- Every calweb edit path keeps working on TZID data: "all events" edits,
  drag-move, "only this" and "this and following".
- Existing calweb-written UTC series are repaired on a master edit in calweb.
  Other UTC series require explicit opt-in in the editor.
- Objects stay interoperable: RFC 5545 requires a VTIMEZONE for every TZID
  used.

### Non-goals

- Automatically converting UTC series from other clients. Their UTC timing
  may be intentional and is RFC-correct.
- Floating (zone-less) times. calweb never writes them.
- Converting existing events that nobody edits. Stored series are left as
  they are unless an eligible calweb edit converts them (see 4). There is no
  bulk repair: it would change times people are used to seeing without anyone asking, and it
  would have to guess each owner's zone.
- Changing all-day events.

## Design

### 1. Which zone

The user's display zone: `prefs.tz` when set, otherwise the device zone
(`Intl.DateTimeFormat().resolvedOptions().timeZone`). The grid already renders
in this zone (`tzOffsetMs`/`tzDayStart` in `shared/time.js`), but the editor
currently formats and parses inputs in the device zone: `localDate`,
`formatTimeOfDay`, `new Date(...)` and `setHours` use device-local fields.
Changing serialization alone would therefore store the wrong wall time when
the preference and device zones differ.

Add `writeZone()` in `shared/time.js` returning that IANA name. If a device
reports no usable name, fall back to UTC (`…Z`), which keeps today's behavior
rather than guessing.

Update the editor's timed input formatting, parsing, defaults and date/time
changes to use this resolved display zone explicitly. Format instants into
zone-local fields and parse those fields with `zonedToInstant` (section 2);
do not parse a zone-less input string with `new Date(...)`. Keep all-day
inputs as calendar dates. Calculate offsets using `Intl.DateTimeFormat`
`formatToParts`, replacing `tzOffsetMs`'s current round-trip through
device-local string parsing.

For example, with a UTC device and `prefs.tz = Europe/Zurich`, entering
09:00 on 13 March 2026 must produce `08:00Z`, serialize as Zurich 09:00,
and reopen as 09:00. Existing series retain their anchor zone when edited,
including a "this and following" tail; pass that zone into the writer
instead of resolving the current preference again.

### 2. Writing date-times (client)

New helpers in `shared/ical.js`:

- `zonedParts(date, tz)` gives the wall-clock `YYYYMMDDTHHMMSS` of an instant
  in `tz`, via `Intl.DateTimeFormat(…, { timeZone: tz, hourCycle: 'h23' })`
  `formatToParts`.
- `dtLine(name, date, tz)` produces `NAME;TZID=<tz>:<wall>`, or `NAME:<utc>Z`
  when `tz` is empty or `UTC`.
- `zonedToInstant(wall, tz)` resolves local fields with explicit gap/fold
  handling. Find candidate instants using the offsets around the local date
  and validate them by formatting back into the zone. For repeated times,
  choose the earlier instant. For skipped times, use the offset before the
  gap, resolving forward by the gap's size. Do not assume a one-hour change
  or rely on a single offset correction. These are the DATE-TIME rules in
  [RFC 5545 §3.3.5](https://www.rfc-editor.org/rfc/rfc5545.html#section-3.3.5).
  Zurich `20261025T023000` resolves to `20261025T003000Z`, and
  `20260329T023000` to `20260329T013000Z`. Local fields cannot preserve the
  distinction between both occurrences of a repeated time: retain UTC when
  an existing instant in the second occurrence must be preserved, or skip
  automatic series conversion and report why. Recurrence expansion's rules
  for nonexistent generated slots remain separate from resolving explicit
  DATE-TIME values.

Then:

- `eventLines` accepts an explicit zone and writes timed DTSTART/DTEND with
  `dtLine`. `buildEvent` defaults new events to `writeZone()`; "only this"
  overrides and "this and following" tails inherit the master's zone or
  UTC form unless section 4 explicitly converts the series.
- The editor's plain-event branch builds DTSTART/DTEND the same way. A
  one-off event that gains an RRULE therefore becomes a zoned series.
- **RECURRENCE-ID and EXDATE follow the master's DTSTART form.** When the
  master has a TZID, `ridValue` and `addExdate` write
  `;TZID=<same zone>:<wall time of the occurrence>`. When the master is UTC,
  they stay UTC (until the series is converted, see 4). A mismatch still
  works on our server, which matches by instant (`overrides[t.Unix()]` in
  `expand.go`), but Apple and Google compare the value form. The helper looks
  up the master's DTSTART params in the stored text, which every one of these
  paths already fetches.
- The RRULE's `UNTIL` stays UTC, as RFC 5545 requires when DTSTART has a
  TZID. `rruleWithUntil` and `splitSeriesRRules` already write `…Z`.
- `buildFreeBusyRequest` and `DTSTAMP` stay UTC (instants, not recurrence
  anchors).

### 3. Shifting TZID values (client)

`shiftDate` must handle `TZID=` values instead of returning `null`, or
"all events" edits and drag-moves of the new data would silently change
nothing:

- Replace the unqualified `deltaMs` contract with an explicit wall-clock
  delta for TZID values. Project the original occurrence and requested
  target instant into **the value's own TZID**, interpret both sets of
  fields as UTC solely for calendar arithmetic, and subtract those synthetic
  timestamps. Add that difference to the stored master's wall fields, then
  resolve with `zonedToInstant`. A New York series stays a New York series.
- Update the editor's "all events" path and `moveEvent`/drag callers in the
  same change. Pass original and target instants (or explicitly typed deltas),
  not the existing `target - original` elapsed milliseconds into wall-clock
  arithmetic. Compute start and end deltas separately in each property's
  zone. UTC properties use elapsed deltas; all-day properties use calendar-day
  deltas.
- If the TZID isn't a name `Intl` knows (an Outlook "W. Europe Standard
  Time"), return `null` as today. Drag callers already surface an error;
  make the "all events" editor report an unsupported-zone error too, instead
  of silently skipping the requested date change.

Moving an occurrence by a whole number of days in wall-clock terms is what
users mean. Moving Zurich 09:00 on 28 March 2026 to 09:00 on 29 March is
23 elapsed hours but one wall-clock day. The corresponding autumn move takes
25 elapsed hours. Both must yield a one-day wall-clock delta when applied
to a zoned master, even when its first occurrence is in another season.

### 4. Converting existing UTC series on edit (client)

When a calweb edit touches a **recurring** event whose DTSTART is UTC — any
occurrence, in any scope ("only this event", the editor's default,
included) — automatically convert only objects with `PRODID:-//calclient//EN`.
Preserve UTC for other or missing PRODIDs unless the user explicitly selects
"Keep this series at a fixed local time" and accepts a preview of changed
occurrences. A title-only edit must not automatically change an imported UTC
series's timing. If the resolved target zone is UTC, no conversion is needed.

For an eligible or explicitly selected series, rewrite the whole object into
the user's zone in the same PUT:

- DTSTART/DTEND of the master become `TZID=<writeZone()>`, keeping each
  instant as it is for the *first* occurrence. The series then keeps the
  wall-clock time it had at its start. For a series created in winter that
  means summer occurrences move back to the time the user originally entered.
- Map every RECURRENCE-ID and EXDATE from its original recurrence slot to
  the corresponding slot in the converted series, then write that slot in
  the master's TZID form. Merely formatting the old instant in the new zone
  detaches exceptions after a transition: `20260403T080000Z` must name
  Zurich `20260403T090000`, not 10:00, for the example above. Preserve an
  override's explicitly chosen DTSTART/DTEND instants when zoning them,
  subject to the fold limitation in section 2. If slot correspondence or
  instant preservation cannot be established, skip automatic conversion
  and report why; do not partially rewrite the object.
- One notice in the editor: "This series was stored in UTC; saving fixes its
  times across daylight-saving changes." It is shown only when the conversion
  would move later occurrences. Users otherwise see some occurrences change
  time without having touched them.
- The editor shows the opened occurrence where it will be after conversion
  (the first occurrence's local time), not where it drifted to — opting in
  for a non-calweb series switches the shown times. What's on screen is what
  gets saved, so an untouched time lands there in every scope: an "only this"
  override is written in the converted series' zone, naming the converted
  slot; a "this and following" tail starts at the shown times.

This matters because today an "all events" edit shifts the stored UTC value
and keeps it UTC, so editing alone never repairs a series.

A time the user types is a target, not a shift: "this occurrence is at
09:00". So an "all events" edit that converts the series converts first and
then moves the converted series by the wall-clock difference between the
edited occurrence's converted position (found by the same slot mapping) and
the typed time (`ical.convertAndMove`). The edited occurrence therefore lands
exactly where the user put it. Example: a series anchored at 09:00 CET whose
summer occurrences drifted to 10:00 CEST; fixing one to 09:00 puts the whole
series at 09:00. Shifting the UTC master by the elapsed difference first
would give 08:00, and in the mirror case (a summer-anchored 10:00 series
fixed in winter) 11:00. A save that doesn't change the times is plain
conversion, anchored on the first occurrence as above.

### 5. VTIMEZONE (server)

RFC 5545 §3.6.5 requires a VTIMEZONE for each TZID in an object. Our server
resolves IANA TZIDs without one (`time.LoadLocation`), and most clients do
too, but not all.

Generating VTIMEZONEs in calweb means shipping rule tables to the browser.
Instead, the server adds a missing one on PUT, which also fixes other clients
that send bare IANA TZIDs:

- A `WriteInterceptor` step (in `caldav.Plugin.InterceptWrite`, before
  denormalization) collects the TZIDs used in the object. For each IANA TZID
  with no VTIMEZONE in the object, it appends a generated definition.
- Generation uses transition history and future rules from the same pinned
  IANA tzdb release used by server expansion. Use a TZif-aware generator that
  preserves historical changes and translates the database's future rules
  into VTIMEZONE observances. Do not infer a perpetual annual rule from one
  year's dates, or assume a year without transitions means a permanently
  fixed-offset zone.
- For finite objects, include the offset before the earliest referenced date
  and transitions through the last recurrence, including overrides and other
  explicit date properties. Unbounded series need history from that lower
  bound plus faithful open-ended future observances (or the database's
  permanent final offset). There is no arbitrary 20-year truncation:
  [RFC 5545 §3.6.5](https://www.rfc-editor.org/rfc/rfc5545.html#section-3.6.5)
  requires coverage for every recurrence instance. If the generator cannot
  represent a future rule faithfully, fail with an actionable error instead
  of attaching an approximation. Full-span generation is a release requirement.
- Cache one definition per TZID, open-ended from the earliest year any object
  asked for (requests are normalized to that shape, so memory stays bounded
  by the zones in use), keyed by the zone data it was built from. Reuse it
  only when its coverage contains the request and the zone data still
  matches expansion.
- **ETag:** the stored body now differs from what the client sent. RFC 4791
  §5.3.4 says the server must not return a strong ETag for a PUT it modified.
  `CorePlugin.put` currently returns the ETag unconditionally, even when the
  scheduling interceptor rewrote the body. Compare bytes before and after the
  interceptors and omit `ETag` when they differ. That fixes the latent
  scheduling case too. calweb is unaffected: `putObject`
  (`shared/store/objects.js`) never reads the response ETag, and every edit
  path `getObject`s the stored text first. Check the parity corpus for a
  pinned ETag on modified PUTs before changing it.

This is independent of the client change and can ship first or after.

## Tests

- **Client unit** (`tests/ical.test.js`, `tests/time.test.js`):
  - `zonedParts`/`zonedToInstant` round-trip unambiguous values across both
    transitions for Europe/Zurich and America/New_York. Assert exact UTC
    instants for gaps and folds, including section 2's Zurich examples and a
    non-hour transition. Test preservation/skip behavior for an existing
    instant in the second occurrence of a repeated time.
  - Editor formatting/parsing and offset calculation work with UTC and
    DST-observing device zones when the preference is Zurich; include defaults,
    date changes, save and reopen.
  - `eventLines` writes `TZID=` for timed events and `VALUE=DATE` for all-day
    events.
  - `shiftDate` on a TZID value keeps the value's own zone and wall-clock
    delta across a transition, and still returns `null` for a non-IANA TZID.
    Test callers too: 23- and 25-hour elapsed moves become one wall-clock day,
    including edits whose master starts in another season and a display zone
    different from the master's zone. Unsupported-zone edits report errors.
  - `ridValue`/`addExdate` follow the master's form, in both UTC and TZID.
  - UTC-to-zone conversion of a series with overrides and EXDATEs keeps every
    override attached to its slot, including exceptions after a transition.
    A title-only save converts an eligible calweb series but preserves UTC
    for other and missing PRODIDs; explicit opt-in converts those series.
    Cover conversion combined with a requested time edit.
- **Server** (`internal/caldav/ical`):
  - Expansion of a zoned weekly series keeps 09:00 local across both
    transitions (the probe from the Evidence section, as a regression test).
  - VTIMEZONE generation: evaluate the component with an independent parser
    that uses its embedded observances rather than looking up its IANA name.
    Compare against the pinned tzdb daily and immediately before/after
    transitions for a DST zone, a fixed-offset zone and a southern-hemisphere
    zone. Cover historical rule changes, series longer than 20 years and
    unbounded future rules. Assert cache reuse never substitutes narrower
    coverage or another tzdb version. Unsupported future rules fail without
    a partial write.
  - A PUT with a bare IANA TZID gets a VTIMEZONE and no ETag. A PUT that
    already has one is stored byte-for-byte and returns its ETag.
- **e2e** (Playwright): create a weekly event in calweb, navigate past the next
  DST change, and check it keeps its hour. Edit "all events" and drag-move a
  zoned event, and check the new time sticks. Run with a UTC browser zone and
  Zurich preference, asserting input, grid and reopened times agree. Exercise
  day moves across both transitions and a series anchored in another zone.

## Rollout

1. Client: zone-aware editor formatting/parsing and offset resolution, zoned
   writes, TZID-aware `shiftDate` and its callers, RECURRENCE-ID/EXDATE form.
   Ship these together so inputs and serialization agree.
2. Client: provenance-restricted convert-on-edit (with the notice), plus
   explicit opt-in for other UTC series.
3. Server: VTIMEZONE injection and ETag suppression on modified PUTs.

Steps 1–2 fix new calweb series and eligible existing series on master edits.
Other UTC series and series nobody edits keep their current behavior unless
explicitly converted. Step 3 improves interoperability and requires full-span
generation and cache validation before release.

## Open questions

- Should convert-on-edit (4) also apply to one-off UTC events, for
  uniformity? Proposed: no. They're correct as they are, and rewriting them
  just churns data.
- If a user changes their zone preference, should their existing series
  follow? Proposed: no. A series keeps the zone it was written in, as in
  every other calendar app.

## Implementation notes

Where it lives:

- `cmd/calclient/web/shared/time.js`: `writeZone`, `zoneFields`,
  `zonedToInstant` (gap/fold rules), `isFoldSecond`, `shiftWallDays`;
  `tzOffsetMs` now uses `formatToParts`.
- `cmd/calclient/web/shared/ical.js`: TZID-aware `parseICalDate`, `dtLine`,
  `occurrenceLine`, `shiftDate(name, prop, {from, to})`, zone-aware series
  stepping, `convertSeriesToZone`, `utcSeriesInfo`,
  `conversionMovesOccurrences`, `conversionPreview`.
- `cal/js/event-dialog.js` (zone-aware inputs, per-path zones, conversion
  notice/opt-in/preview), `cal/js/grid.js` (drag targets, timed clicks),
  `shared/store/events.js` (`moveEvent(inst, to)`, `ridLine`).
- `internal/caldav/ical/vtzgen.go`: generation and self-validation;
  `vtimezone.go` reads RDATE. `caldav.Plugin.AddVTimezones` (on in
  `internal/server`); `internal/dav/core.go` omits the ETag on modified PUTs.

Where the implementation refines or departs from the text above:

- **tzdb pinning.** Expansion resolves TZIDs through `time.LoadLocation`
  (inside go-ical), which re-reads zoneinfo on every call from a
  build-specific search path (Nix patches in its own directory). It cannot be
  pinned for the process lifetime without replacing go-ical's lookup. So the
  generator works from the same `*time.Location`: history comes from
  `Time.ZoneBounds`, and the future rule from a TZif footer that is used only
  if it reproduces that Location for 100 years. The cache is keyed by a
  fingerprint of the Location's transitions rather than by process lifetime,
  so a tzdb update on disk (which expansion picks up immediately) never reuses
  a stale definition. Every generated definition is also parsed back with
  `parseVTimezone` and compared with the Location over its whole coverage
  (through 2099 when unbounded) before it is attached.
- **Go quirk.** In rule-extended leap years, `ZoneBounds` stops advancing on
  31 December; `zoneTransitions` falls back to day steps plus bisection there.
- **Rules outside 00:00–24:00** (Jerusalem `M3.4.4/26`, Nuuk `/-1`, Cairo
  `M10.5.4/24`) are expressed exactly as a weekday within a shifted month-day
  range (`BYDAY=FR;BYMONTHDAY=23,…,29`). "Last" ranges count from the
  month's end (negative month-days), so February's length doesn't matter.
  A range that straddles two months (Cairo's can be 1 November) becomes two
  observances, one per month: a single RRULE with two BYMONTHs would apply
  the combined day list to both months. Only a positive range running past
  28 February would still fail the PUT. All 598 zones in the tzdb generate
  and pass the self-check.
- **Sabre parity.** Injection is a `caldav.Plugin` option, off in the zero
  value, so the sabre corpus composition keeps storing bodies as sent; the
  calserver composition turns it on. The ETag change moves toward sabre.
- **Coverage** treats a `COUNT` series as unbounded (a superset; COUNT isn't
  evaluated). Definitions are cached open-ended from the start of the
  earliest requested year, one per zone. A bounded object in a zone whose
  future rule can't be written still gets an exact bounded definition, which
  isn't cached.
- **Conversion anchor.** Decided: without a time change, the first
  occurrence anchors the conversion. With one, the typed time is applied as a
  target after converting (section 4, `ical.convertAndMove`).
- **What conversion maps, and what it refuses.** Rule slots move by the
  anchor's offset. RDATE occurrences keep their instants, and so does any
  RECURRENCE-ID or EXDATE naming one (it would otherwise detach). Values not
  stored in UTC (an override in another zone, a DATE) are left exactly as
  stored. Refused, besides the cases above: sub-daily frequencies, and
  MONTHLY/YEARLY rules whose anchor's UTC date isn't its local date (the
  month-day and month they take from DTSTART would change).
- **Untouched inputs keep their instant.** The editor re-resolves typed
  times with the earlier-instant rule, so date/time fields left as shown keep
  the shown instant — otherwise a title-only edit of an event at the second
  02:30 of the October change would move it an hour earlier.
- **Conversion scope.** Every edit of a UTC series converts it (not only the
  "all events"/"this and following" edits the first draft of section 4
  named): users edit a drifted occurrence with the default "only this event"
  scope and expect its original time back.
- **Tests.** The e2e spec (`e2e/tests/dst.spec.js`) runs with a UTC browser
  and a Zurich preference and uses bob's `default` calendar, so it doesn't
  need the demo seed.
