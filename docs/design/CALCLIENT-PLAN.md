# calclient — a small CalDAV/CardDAV demo client for calendsd

## Context

`calendsd` is a full-featured CalDAV/CardDAV server (a Go port of sabre/dav) with a
large protocol surface: discovery, calendar/contact CRUD, `calendar-query` /
`multiget` / `free-busy-query` / `sync-collection` / `addressbook-query` REPORTs,
iTIP scheduling with inbox/outbox and iMIP, calendar sharing, proxy delegation,
notifications, webcal subscriptions, whole-collection export, RFC 5995 Add-Member,
and ACL/privilege introspection. Today the only ways to see these work are `curl`,
the parity/corpus test suites, and the separate read-only `calendsd-admin` panel —
there is no interactive client that exercises the *client-facing* protocol.

This plan adds a **small, vanilla-JS web client** that drives the server the way a
real CalDAV client would, so every feature is visibly demonstrable in a browser.

**Key constraint that shapes the whole design:** the server sends **no CORS
headers**, and CalDAV depends on non-standard HTTP methods (`PROPFIND`, `REPORT`,
`MKCALENDAR`, `PROPPATCH`, `MOVE`). A browser can only issue those without a
blocked CORS preflight when the page and the API are **same-origin**. We therefore
ship a tiny Go binary (`cmd/calclient`) that serves the static client **and**
reverse-proxies DAV requests to a configurable target server. The browser only ever
talks to `calclient` → same-origin → no CORS, no preflight, no server changes.
Chosen over embedding in `calendsd` so the DAV server stays untouched and the
client can point at any local or remote server.

## Architecture

```
browser ──same-origin──> cmd/calclient ──reverse proxy──> calendsd (-target)
   │                          │
   └ vanilla JS (fetch)       ├ GET /app/*        → embedded static assets
     DAV requests to /        └ everything else   → proxied to -target
```

- The SPA lives under `/app/` (reserved prefix). `/` (exact) redirects to `/app/`.
- All other paths (`/principals/…`, `/calendars/…`, `/addressbooks/…`,
  `/.well-known/…`) are proxied verbatim to `-target`. Because DAV hrefs in
  responses are absolute paths rooted at `/` (e.g. `/calendars/admin/default/`),
  they stay valid same-origin with zero rewriting.
- Auth: the JS login form captures username/password, base64-encodes them, keeps
  them in `sessionStorage`, and attaches `Authorization: Basic …` to every fetch.
  The proxy forwards the header to `-target`. (`fetch()` receiving a 401 does not
  trigger the browser's native auth dialog, so the JS controls login.)

## Component 1 — `cmd/calclient` (Go proxy, ~120 lines, stdlib only)

New package under the existing `calendsd` module; imports only stdlib
(`embed`, `flag`, `net/http`, `net/http/httputil`, `net/url`, `log/slog`). No
dependency on `internal/*`, keeping it decoupled.

Files:
- `cmd/calclient/main.go`
  - Flags: `-addr` (default `127.0.0.1:8091` — not 8090, which the `-rcptd-url`
    docs use as their example port), `-target` (default `http://127.0.0.1:8080`),
    `-dev-assets` (optional dir to serve assets from disk instead of the
    embedded copy, for iterating without rebuilds).
  - Builds an `httputil.NewSingleHostReverseProxy(target)`; `ReverseProxy` passes
    arbitrary methods/bodies through unchanged, which is exactly what DAV needs.
  - Mux: `GET /app/` → static handler (embedded FS); `GET /{$}` → 302 to
    `/app/`; `/` (the catch-all pattern) → proxy. **Note the `/{$}` exact-match
    pattern**: a plain `GET /` pattern in the Go 1.22+ `ServeMux` is a subtree
    wildcard that would swallow *every* GET (event `.ics` GETs, `?export`
    downloads, `/.well-known/*`) and break all GET-based panels; only the
    exact root may redirect.
  - `ModifyResponse` hook: on a proxied `401`, rewrite/strip `WWW-Authenticate`
    to `Basic realm="calclient"` (defensive — keeps any native dialog out of the
    way and normalizes the challenge the JS sees).
- `cmd/calclient/web/…` — the client assets (see Component 2), pulled in with
  `//go:embed web` in `main.go`.

## Component 2 — the web client (`cmd/calclient/web/`)

Vanilla JS, no framework, no build step. Kept to a small handful of files; DAV
plumbing centralized so panels stay short.

- `index.html` — login bar (user/pass/target-status), a left nav of feature tabs,
  and a right "raw exchange" drawer that shows the last request/response
  (method, path, headers, body) for whichever action ran — this is how the
  protocol-level features stay visible even where the UI is friendly.
- `app.css` — minimal hand-written styles (no CDN; CSP-free since same-origin).
- `js/dav.js` — the core: `req(method, path, {headers, body})` wrapper over
  `fetch` that injects auth and records the exchange for the drawer; helpers to
  build and parse `multistatus` XML (`propfind`, `report`), extract hrefs/props,
  and small XML builders for filter/report bodies.
- `js/ical.js` — tiny iCalendar (VEVENT/VTODO) and vCard build/parse helpers —
  just enough to round-trip the fields the UI edits (SUMMARY, DTSTART/DTEND,
  RRULE, ATTENDEE/ORGANIZER; FN/EMAIL/TEL for vCards).
- `js/app.js` — session/discovery + tab router; wires each panel below.

### Panels (one per feature area) and the DAV they exercise

| Panel | Server features demonstrated | Requests |
|---|---|---|
| **Discover** | current-user-principal, home sets, inbox/outbox URLs, user-address-set, well-known | `PROPFIND /` and `/principals/{u}/`; `GET /.well-known/caldav` (301) |
| **Calendars** | list, create, edit props, delete; color/description/timezone/component-set; getctag | `PROPFIND` depth 1, `MKCALENDAR`, `PROPPATCH`, `DELETE` |
| **Events** | event CRUD in iCal & jCal; recurrence; overridden instances; conditional writes | `PUT`/`GET`/`DELETE` `.ics` with `If-Match`/`If-None-Match`; `GET .ics` with `Accept: application/calendar+json` |
| **Query** | time-range + comp/prop filters; expand recurrences; multiget | `REPORT calendar-query` (with `expand`), `REPORT calendar-multiget` |
| **Sync** | RFC 6578 incremental sync; getctag fast-path | `REPORT sync-collection` (empty then returned token); `PROPFIND` getctag/sync-token |
| **Free/Busy** | availability lookup | `REPORT free-busy-query` |
| **Scheduling** | iTIP invites/replies/cancel; attendees; inbox/outbox; group expansion; iMIP status | outbox `POST` (VFREEBUSY/REQUEST); `PUT` event with ATTENDEEs; read `PROPFIND /calendars/{u}/inbox/`; show `SCHEDULE-STATUS` |
| **Sharing** | share/unshare a calendar; invite reply; notifications | `POST` `{cs}share` **to the calendar**; `POST` `{cs}invite-reply` **to the calendar home** `/calendars/{u}/` (the server only handles it there); `PROPFIND /calendars/{u}/notifications/` |
| **Delegation** | calendar-proxy-read/write management + discovery | `PROPPATCH group-member-set` on proxy principals; `PROPFIND` proxy-*-for |
| **Subscriptions** | webcal subscription create/list | `MKCALENDAR` with `{cs}subscribed` resourcetype + `{cs}source` |
| **Export / Add-Member** | whole-calendar ICS/jCal export; server-assigned names | `GET …/?export[&expand&start&end&componentType]` (add `&accept=jcal` for jCal — usable in a plain download link where headers can't be set); `POST …/;add-member` (the only form; advertised via the `{DAV:}add-member` prop) |
| **Contacts** | addressbook create; vCard CRUD; search; multiget; export | `MKCOL`, `PUT`/`GET`/`DELETE` `.vcf`, `REPORT addressbook-query` (text-match), `addressbook-multiget`, `?export` |
| **ACL** | privilege introspection; principal search | `PROPFIND` `current-user-privilege-set`/`acl`/`supported-privilege-set`; `REPORT principal-property-search` |
| **Raw** | anything not covered above | free-form method/path/body console reusing `dav.js` |

Every panel writes to the shared "raw exchange" drawer, so the hybrid UI doubles
as a protocol explorer without separate code.

## Dev / run integration

- **Makefile**: add `calclient` to the `build` target (`bin/calclient`), start
  it from the default `dev` target alongside the two servers, keep a
  standalone `make client` run target (`./bin/calclient -addr 127.0.0.1:8091
  -target http://127.0.0.1:8080`), and build it in the `freebsd` cross-compile
  target. Update the `dev` target's echo lines with the client URL.
- **`make seed`** (`scripts/seed-demo.sh`): idempotent demo data against the
  running dev server — ensures the `bob` principal, creates a `demo` calendar
  (color/description via PROPPATCH), PUTs a plain event, a recurring event,
  and an invite with ATTENDEE bob (exercises inbox delivery +
  SCHEDULE-STATUS), and creates a `contacts` addressbook with two vCards.
- **Server flags for a full demo**: run `calendsd` with the Calendar-Server
  behaviors on so every panel has something to show:
  `-auto-provision -per-user-props -expand-groups -principals-readable`
  (and `-auto-schedule-prefixes resource` to demo auto-booking).
  Note this in the run instructions; the defaults keep these off for sabre
  parity. Do **not** add `-browser`: the CorePlugin serves the browser
  plugin's HTML index for GETs on collections before the CalDAV plugin sees
  them, which shadows `?export` (a server quirk vs sabre, where the export
  plugin outranks the browser plugin) — and calclient replaces the HTML
  index anyway.
- **Second principal for sharing/delegation/iTIP**: the dev DB
  (`scripts/dev-db.sh`) seeds only `admin`. Sharing, proxy delegation, and
  local iTIP need a target user. Add an optional seed for a `bob`/`bob`
  principal — simplest via the existing `calendsd-admin` write action
  (`storage.AdminWrites.CreateUser`) run with `-readonly=false`, or a small
  `INSERT` appended to the `load_schema()` block in `scripts/dev-db.sh` mirroring
  what `CreateUser` does: a users row with `digesta1 = md5("bob:SabreDAV:bob")`
  plus **three** principals rows — `principals/bob` itself *and* its
  `principals/bob/calendar-proxy-read` / `…/calendar-proxy-write` children
  (without the proxy children the Delegation panel silently breaks for `bob`).
  Set the principal's `email` column too so `user-address-set`/iTIP addressing
  resolves `bob`. Document both; the SQL seed is the zero-dependency path.

## Verification

1. `make build` (builds `calendsd`, `calendsd-admin`, `calclient`).
2. `make dev` (starts DB + calendsd + admin + calclient; add the demo flags
   above to the calendsd line for the full feature set), then `make seed`
   in a second terminal for demo data.
3. Open `http://127.0.0.1:8091/app/`, log in as `admin`/`admin`.
4. Walk each panel; confirm in the raw-exchange drawer that the expected
   method/REPORT ran and returned 2xx/207. Concretely:
   - Discover shows `calendar-home-set` = `/calendars/admin/`.
   - Create a calendar → it appears; `PROPPATCH` a color → re-`PROPFIND` shows it.
   - Create a recurring event → `calendar-query` with `expand` returns multiple
     instances; edit with a stale `If-Match` → expect `412`.
   - `sync-collection` with an empty token returns all + a token; after a create,
     the same token returns only the delta.
   - Set `bob` as calendar-proxy-write and confirm `calendar-proxy-write-for`
     from `bob`'s principal. (Sharing/notifications/invite-reply and webcal
     subscriptions are **not implemented by the Postgres storage backend** —
     sabre-PDO parity. Those panels still demonstrate the protocol; expect the
     server's honest 403/501/404 in the drawer, and note the panels say so.)
   - PUT an event with an ATTENDEE for `bob` → confirm a scheduling object lands in
     `/calendars/bob/inbox/` and `SCHEDULE-STATUS` is set on admin's copy.
   - Contacts: create an addressbook, PUT a vCard, `addressbook-query` text-match
     finds it, `?export` downloads a merged vCard.
5. Cross-check anything ambiguous against `calendsd-admin` (same DB) and the
   `curl` patterns in the existing tests. Caveat: calendar color/description/
   timezone set via `PROPPATCH` land in dead-property storage (the
   propertystorage table), not the `calendarinstances` columns — they round-trip
   fine through PROPFIND, but the admin panel (which reads the columns) may not
   show them; that's expected, not a bug.
6. `go vet ./...` and `go build ./cmd/calclient` stay clean; run `gofmt`.

## Notes / risks

- **Storage backend capability gaps** (found during implementation): the
  Postgres `storage.CalDAV` backend implements calendars, objects, sync, and
  scheduling, but not `SharingSupport`, `NotificationsBackend`,
  `SubscriptionsBackend`, or `ReplySupport`. The Sharing and Subscriptions
  panels therefore surface 403/501/404 against the dev server (the panels
  carry a hint saying so); the endpoints themselves work against
  mock-backend test servers.
- **Server XML fragment quirk**: MKCALENDAR re-parses the `resourcetype` and
  property fragments standalone, so namespace prefixes declared on the
  request root are lost. The client declares `xmlns` inline on the elements
  inside `resourcetype`/`source` (e.g. `<subscribed xmlns="…calendarserver…"/>`).

- **jCal round-trip** and **iCal parsing** are the fiddliest client-side bits;
  `ical.js` stays intentionally minimal (only the fields the UI edits) and leans
  on the server for validation — malformed bodies surface as `415` in the drawer,
  which is itself a useful demonstration.
- **Digest** is not implemented client-side; the client uses **Basic** (server
  supports both). Fine for a same-origin demo over loopback; document that the
  proxy forwards credentials in the clear unless fronted by TLS.
- No new third-party dependencies (Go stdlib only; no JS framework/CDN).
