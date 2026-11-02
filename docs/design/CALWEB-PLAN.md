# calweb — an end-user web calendar & contacts UI for calendsd

> **Status: implemented.** Served at `/cal/` by `cmd/calclient`; `/` redirects
> there. The invite accept flow, expanded-range queries (incl. all-day and
> recurring events), and free-busy were re-verified end-to-end at the
> protocol level during implementation. "Default calendar" resolves to the
> first calendar in the home (auto-provision names it `calendar`), and is
> user-selectable in Settings.

## Context

`cmd/calclient` (CALCLIENT-PLAN.md, implemented) is a protocol explorer: one
panel per DAV feature, a raw-exchange drawer, protocol vocabulary everywhere.
It is built for debugging, not for using. This plan adds **calweb**: a
calendar/contacts app a non-technical user can live in — month grid, "New
event" dialog, invitation cards with Accept/Decline, a searchable contact
list. No DAV terms anywhere in the UI; errors in plain language.

Everything here builds on facts verified while implementing calclient:

- The same-origin proxy approach works and needs no server changes (no CORS
  headers server-side; hrefs are absolute paths; `httputil.ReverseProxy`
  passes DAV methods through).
- The Postgres storage backend supports: calendars CRUD + props, object CRUD
  with ETags, `calendar-query` **with server-side `expand`**, `calendar-multiget`,
  `sync-collection` + getctag, `free-busy-query`, iTIP scheduling (inbox
  delivery, SCHEDULE-STATUS, outbox free-busy probes), addressbooks + vCards +
  `addressbook-query`, `principal-property-search`, `?export`.
- It does **not** support sharing, notifications, subscriptions, or invite
  replies (sabre-PDO parity). These are excluded from calweb, not hidden
  behind broken buttons.

**Key design lever:** the server expands recurrences (`calendar-query` +
`<c:expand>`, verified: RRULE → N instances, overrides come as separate
VEVENTs with RECURRENCE-ID). calweb therefore never does recurrence math —
it asks for the visible date range expanded and renders what comes back. No
rrule library, no drift from server semantics.

## Architecture

Same binary, second app. `cmd/calclient` already serves embedded static
assets and proxies everything else; calweb is another embedded SPA next to
the explorer:

```
cmd/calclient/web/
  app/      ← the existing protocol explorer (moved from web/ root)
  cal/      ← calweb (this plan)
  shared/   ← dav.js, ical.js (imported by both apps)
```

- Mux: `GET /app/` → explorer, `GET /cal/` → calweb, `GET /shared/` → shared
  modules, `GET /{$}` → 302 **to `/cal/`** (the friendly app becomes the
  front door; debuggers know to type /app/). Everything else → proxy,
  unchanged.
- Vanilla JS, no framework, no build step (matches the repo's stdlib-only
  culture). CSS is hand-written; one small icon set inlined as SVG.
- Reuse `dav.js` (instrumented fetch, multistatus parsing) and `ical.js`
  (build/parse) as-is; calweb adds what the explorer didn't need: a client
  cache + polling sync layer, and richer iCal fields (LOCATION, DESCRIPTION,
  all-day DATE values, EXDATE, PARTSTAT editing).
- Auth stays Basic-over-same-origin: friendly login screen, credentials in
  `sessionStorage` (an optional "stay signed in" checkbox moves them to
  `localStorage` with a note in Settings). Production deployments must front
  the proxy with TLS — same caveat as calclient, now user-facing.

## Components

### 1. `web/shared/store.js` — state + sync (new, ~250 lines)

The one piece of real client infrastructure. Holds the logged-in session and
a per-calendar object cache; views subscribe to changes.

- **Initial load**: discovery (as calclient), then `PROPFIND` depth 1 on the
  calendar home (names, colors, ctags) and the addressbook home.
- **Visible-range fetch**: per enabled calendar, `calendar-query` with
  `expand` over [viewStart − 1 week, viewEnd + 1 week]; instances cached by
  `href + RECURRENCE-ID`.
- **Refresh loop**: every 30 s, one `PROPFIND` depth 1 on the calendar home
  (ctags for all calendars in a single request); calendars whose ctag moved
  re-run the expanded range query. (Deviation from the first draft, which
  planned `sync-collection` + `calendar-multiget` deltas: multiget cannot
  expand recurrences, so delta objects would need client-side RRULE math —
  re-querying the visible range is both simpler and correct.) The inbox is
  polled the same way for the invitation badge.
- **Writes** go through the store so views stay consistent: PUT with
  `If-Match` (ETag tracked per object); on 412, reload the object and surface
  "This event was changed elsewhere — your view has been refreshed, please
  re-apply your edit."

### 2. `web/cal/` — the app (~5 JS files + html + css)

| View | What the user sees | DAV underneath |
|---|---|---|
| **Login** | big centered form, friendly errors ("Wrong username or password") | discovery PROPFINDs; 401 mapped to plain text |
| **Month grid** (default) | 6×7 grid, event chips colored by calendar, today highlighted, click day → New event, click chip → detail popover (edit/delete) | expanded `calendar-query` per visible range |
| **Week view** | 7-day columns with hour rows, all-day lane on top | same data, different projection |
| **Agenda** | chronological list (auto-selected on narrow screens) | same |
| **Sidebar** | calendar list with color dots + visibility checkboxes; "New calendar" (name + color); rename/recolor/delete via a small ⚙ menu | `MKCALENDAR`, `PROPPATCH` (displayname, `{a}calendar-color`), `DELETE` |
| **Event dialog** | Title, calendar picker, start/end (or All day), Location, Notes, **Repeat** preset (none / daily / weekly / monthly / custom "every N …, until date or count"), **Guests** (email chips with autocomplete), availability strip | PUT .ics (UTC times; DATE values for all-day); RRULE from preset; ORGANIZER/ATTENDEE when guests present |
| **Guest autocomplete** | type-ahead over contacts and directory | `addressbook-query` (EMAIL/FN contains) + `principal-property-search` |
| **Availability strip** | per-guest busy bars for the chosen slot, "everyone free ✓ / Bob is busy" | outbox `POST` VFREEBUSY probe (verified) |
| **Invitations** | card list from the inbox: title, time, organizer, Accept / Maybe / Decline buttons; badge in the top bar | poll `schedule-inbox`; on answer: PUT the event into the user's default calendar with own PARTSTAT set (server delivers the iTIP REPLY; SCHEDULE-STATUS verified), then DELETE the inbox item |
| **Sent-invite status** | on own events with guests: per-guest "delivered / accepted / declined" chips | parse ATTENDEE `SCHEDULE-STATUS` / `PARTSTAT` from the organizer copy |
| **Contacts** | searchable list, add/edit form (name, emails, phones), delete; avatar = initials | `PUT`/`DELETE` .vcf, `addressbook-query` |
| **Settings** | week start (Mon/Sun), 12/24 h, default calendar, "stay signed in", export links ("Download calendar as .ics" → `?export`), import (.ics/.vcf file picker → split components → per-object PUTs with progress) | `?export`; batched PUTs |

Files: `cal/index.html`, `cal/cal.css`, `cal/js/main.js` (routing/session),
`cal/js/grid.js` (month/week/agenda rendering), `cal/js/event-dialog.js`,
`cal/js/invites.js`, `cal/js/contacts.js`, `cal/js/settings.js`.

### 3. Recurrence editing rules (kept deliberately simple)

- Create: presets generate a clean RRULE.
- Edit a recurring event: dialog asks "This event only" or "All events".
  - *All events* → rewrite the master VEVENT (PUT).
  - *This event only* → for delete, append `EXDATE`; for modify, add an
    override VEVENT with `RECURRENCE-ID` to the same .ics (both round-trip
    through the server's expand, verified in the corpus).
- No "this and following" in v1 (UNTIL-split is easy to add later).

### 4. Error language

One mapper from DAV/HTTP to human text: 401 → sign-in problem; 403 → "You
don't have permission to do that"; 409/412 → refresh-and-retry message;
415 → "That file doesn't look like a calendar file"; network → "Can't reach
the server". The raw response never appears in calweb (that's what /app/ is
for); a "Details" disclosure shows status + message for bug reports.

## Explicit non-goals (v1)

- Sharing calendars between users, notifications, webcal subscriptions —
  blocked on storage backend support (see CALCLIENT-PLAN.md notes); the plan
  is ready to add an "Share" affordance when `SharingSupport` lands.
- Offline mode / service worker, push (polling only), drag-to-move events,
  print styles, VTODO/tasks UI (server supports VTODO; a Tasks list is a
  natural v2), timezone-aware recurring events (v1 stores UTC and notes the
  DST caveat; v2 can emit TZID + VTIMEZONE).

## Dev / run integration

- No new binary, no new Makefile targets: `make dev` already builds and runs
  calclient; calweb ships inside it. Root `/` now lands on calweb — the dev
  echo line gains "(end-user UI at /cal/, protocol explorer at /app/)".
- `make seed` already creates everything calweb needs to look alive on first
  login (colored calendar, plain + recurring events, an invite for bob,
  contacts). Extend it with one all-day event and a second calendar
  ("Private", different color) so the month grid shows multi-calendar color
  mixing and the all-day lane.
- A deployment can expose calclient (with calweb) behind
  the existing TLS termination for real users; document `-addr` binding and
  the Basic-over-TLS requirement.

## Verification

1. `make dev` + `make seed`; open `/` → lands on calweb login.
2. As **admin**: month grid shows the seeded events with the calendar's
   color; standup appears on 10 days (server expand); create an event by
   clicking a day; edit it; delete one occurrence of the standup (EXDATE) and
   confirm the grid drops exactly that day; create a "Private" calendar and
   recolor it.
3. Invite flow: create an event with guest `bob@example.org` — availability
   strip renders (free-busy), save; open calweb as **bob** (second browser
   profile): invitation card appears (inbox poll), Accept; back as admin: the
   guest chip flips to "accepted" (PARTSTAT from the REPLY).
4. Contacts: add a contact, see it in guest autocomplete; import a small .vcf
   and .ics file; download the calendar via the export link.
5. Conflict path: edit the same event in two tabs → second save shows the
   friendly 412 message and refreshed data.
6. Narrow the window → agenda layout; keyboard: arrows navigate the grid,
   Enter opens, Esc closes dialogs (basic a11y pass, labels + focus traps).
7. `go vet`, `gofmt`, `node --check` on all JS; the /app/ explorer still
   works unchanged (regression: its e2e curl walk-through from
   CALCLIENT-PLAN.md).

## Notes / risks

- **Month-grid rendering is the bulk of the work** (~40% of the code):
  overlapping chips, "+3 more" overflow, all-day lane, week/agenda
  projections. Pure DOM/CSS, no protocol risk.
- **Invitation semantics**: accept/decline is client-driven in CalDAV (PUT
  own copy with PARTSTAT + rely on the server's iTIP REPLY delivery). The
  flow was verified at the protocol level with calclient; calweb only wraps
  it. Edge case to test: declining without ever accepting (event never
  entered the calendar) — then the reply is sent via outbox POST instead.
- **Two apps, one binary**: moving the explorer from `web/` to `web/app/`
  changes no URLs (it was already served under `/app/`) but touches the
  `//go:embed` directive and the mux — a 20-line refactor, done first.
- **UTC-only v1**: recurring events created by users in DST zones will drift
  an hour across transitions. Called out in Settings ("times are stored in
  UTC"); fixing it means TZID+VTIMEZONE emission in `ical.js` (contained,
  v2).
- Basic auth in a browser without TLS is plaintext — same as calclient, but
  calweb targets real users, so the deployment section makes TLS a hard
  requirement rather than a note.
