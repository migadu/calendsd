# Plan: Admin Panel for calendsd

## 0. Purpose

A debugging and setup-assistance tool for admins: answer "why does user X's
calendar look wrong / not sync / not share" without psql or packet captures.
Full read overview of principals, calendars, address books, objects,
properties, ACLs, delegation, scheduling, and sync state; later, gated write
operations to fix common setup problems.

**Non-goals**: tenant-facing self-service, multi-tenant management, anything
that risks sabre co-existence.

## 1. Hard constraints

- **Shared schema**: the Postgres schema is shared live with sabre/dav.
  The admin panel introduces **no new tables**. Writes to anything with a
  sync token, change log, or ETag go through the existing backend interfaces
  (`caldav.Backend`, `carddav.Backend`, `acl.Backend`, `dav.PropStorageBackend`)
  so both servers stay correct. **One carve-out**: `users` and `principals`
  have no backend write path anywhere in calendsd (auth.Backend is
  read-only) and carry no sync state, so user/principal creation and
  password reset go through a new, narrowly scoped `storage.AdminWrites`
  type (§4) — plain SQL in sabre-parity style, nothing else touches it.
  The same carve-out covers deleting doctor-found orphans: their parent
  collection is gone, so no backend can reach them either.
- **Stdlib style**: plain `net/http`, `html/template`, `embed.FS`. No web
  framework, no JS toolchain. Single binary.

## 2. Architecture

**Standalone binary: `cmd/calendsd-admin`**, with the logic in a new
`internal/admin` package.

Key insight: calendsd is stateless — all state lives in Postgres. The admin
app imports the same internal packages, calls the same `server.New(db, config)`
composition against the same DSN, and can therefore run simulations through
the *identical* production code paths. Separation costs almost nothing and
buys:

- Crash isolation both ways (an admin query that OOMs cannot take DAV down).
- Independent deploy/restart cycles.
- Separate resource limits and firewalling.

Accepted costs (documented, mitigated):

1. **Config drift** — the privilege simulator is only truthful if the admin
   app runs with the same flags as the server (realm, `-per-user-props`,
   `-principals-readable`, …; admin principals are hardcoded in
   `server.New`, so they can't drift as long as the admin app composes via
   `server.New` rather than building its own plugin set). "Run both with
   the same flags" is a convention, not a mechanism, so make drift
   checkable: both binaries log the same canonical one-line effective-config
   string at startup, and the dashboard (§3.1) shows the admin app's copy —
   comparing the two is a string diff.
2. **No in-process visibility** — in-flight iMip sends and any future caches
   live in the server process; the admin app sees only DB and logs.
   Negligible today.
3. **No mock mode** — the admin app requires a Postgres DSN. Acceptable:
   debugging real setups is the point.

Structure `internal/admin` so an embedded `-admin-addr` mode in the main
server remains a five-line addition later, but standalone is the default.

### 2.1 Auth

- Admin logins are **decoupled from the DAV `users` table** (avoids the
  digesta1/realm coupling and doesn't force admins to be DAV principals).
- HTTP Basic auth against an **htpasswd-style file** (`user:$2y$…` bcrypt
  lines, `htpasswd -B` compatible), pointed at by an `-admin-auth` flag.
- Listener binds `127.0.0.1:8081` by default (`-addr` to override). Basic
  auth sends the password each request → loopback/LAN or TLS-terminating
  reverse proxy only; state this in README.
- CSRF tokens on any mutating HTML form.

### 2.2 UI + API

Server-rendered HTML (`html/template` + `embed.FS`, minimal vanilla JS).
Every page is backed by a JSON endpoint under `/admin/api/…`; HTML is a layer
on top. Keeps everything testable with `httptest` and scriptable with
curl/jq. Phase A may ship JSON-only if useful earlier.

## 3. Feature areas

### 3.1 Dashboard
- Counts: principals, users, calendars/instances, objects, address books,
  cards.
- Recent activity: last N rows from `calendarchanges`/`addressbookchanges`
  (principal, collection, operation), ordered by synctoken — the change
  tables have **no timestamp column**; a time can be joined in from
  `calendarobjects.lastmodified` only for objects that still exist
  (deletes show without one).
- Effective config echo: the canonical config string from §2 (realm, iMip
  status, enabled Calendar-Server flags) — doubles as the config-drift
  check against the server's startup log line.

### 3.2 Principal & user inspector ("everything about user X")
- Identity: `principals` row (uri, email, displayname) + matching `users` row.
- **Auth diagnostics**: principals without credentials, credentials without
  principals, malformed `digesta1` hashes. A realm mismatch (classic silent
  login failure) is **not detectable offline** — `digesta1` is an opaque
  md5 — so the inspector carries an interactive credential check instead:
  the admin enters a username/password, the app computes the digest under
  the configured realm and compares; a known-good password that fails a
  well-formed hash means the hash was minted under a different realm.
- Group membership and **delegation**, both directions of `groupmembers`,
  rendered in proxy terms: who can act for this user
  (`calendar-proxy-read`/`-write` members) and whose calendars this user sees.
- Links into their calendars, address books, scheduling inbox, notifications.

### 3.3 Calendar & address book inspector
- All `calendarinstances` for a calendar side by side — sharing lives here:
  each principal's view with `access` level (owner/read/read-write) and
  `share_href`.
- **Properties in three layers**: live column-backed props (displayname,
  color, timezone, order, transparent), dead props from `propertystorage`,
  and per-user overlays (the `path + "/\n" + principal` synthetic rows built
  in `internal/dav/peruserprops.go`), shown per principal — "delegate sees a
  different color than owner" made visible. Flag `valuetype=3`
  (PHP-serialized) rows as sabre-only.
- Supported components, sync token, object count, total size.

### 3.4 Object browser
- List/search `calendarobjects` and `cards` by UID, URI, summary, date range.
  UID and URI hit indexed columns; summary/date-range search must parse
  `calendardata` row by row, so it is **scoped to one collection and
  hard-limited/paginated** — the browser must not be the query that OOMs.
- Raw iCal/vCard next to a parsed summary (component type, UID,
  organizer/attendees, RRULE).
- **Stored-vs-derived checks**: recompute `firstoccurence`/`lastoccurence`
  (sabre's single-"r" spelling, kept in the schema)/`size`/`etag` from
  `calendardata` and diff against stored columns — explains "event missing
  from time-range REPORT".

### 3.5 ACL & privilege simulator (the big debugging win)
- Effective ACL viewer: walk the real node tree for any path, render
  `ACL() []ACE` (privilege, principal, protected).
- **"As user X on path Y"**: run `acl.Plugin.CurrentUserPrivileges` — and
  optionally a full synthetic PROPFIND through the plugin chain with the
  principal forced (the `auth.NewLoggedInPlugin` test pattern) — and show
  the result. Answers "why 403" by exercising the exact production code path.

### 3.6 Scheduling & sync debugger
- Scheduling: per-principal inbox (`schedulingobjects`) with parsed iTIP
  method/status; pending share invites — the shared schema stores **no
  notification objects** (sabre's PDO backend lacks notification support),
  so `calendarinstances.share_invitestatus = 1` is the source of truth;
  iMip configuration status.
- Sync: change-log timeline per collection; "what would `sync-collection`
  return for token T" preview via `ChangesForCalendar` — debugs "my phone
  stopped syncing".

### 3.7 Doctor (consistency checks)
Read-only invariant checks, each finding linked to its inspector:
- Orphaned `calendarinstances` / objects.
- Unparseable `calendardata` / `carddata`.
- Dangling `groupmembers` rows.
- Principal/user mismatches and realm mismatches (§3.2).
- Stale denormalized occurrence columns (§3.4).
- `propertystorage` rows pointing at nonexistent paths.

### 3.8 Setup actions (writes — gated)
Behind `-readonly` defaulting to **true**:
- Create user + principal (compute `digesta1` from the configured realm);
  reset password. These are the one class of write with **no existing
  backend path** — they go through `storage.AdminWrites` (§1 carve-out, §4).
- Edit delegation via `SetGroupMemberSet`.
- Fix collection properties via the backend `PropPatch` path.
- Provision default calendar/address book (reuse auto-provision code).
- Delete orphans found by the doctor.

Every write with sync state goes through existing backends — raw SQL only
inside `AdminWrites` for the sync-state-free `users`/`principals` tables —
and emits a structured audit log line (no audit table, per §1).

## 4. Data access

Backend interfaces are per-principal shaped (`CalendarsForUser`, …) — right
for inspectors, wrong for cross-user aggregates. Add a small **read-only**
`storage.AdminQueries{DB}` beside the existing storage types for the
dashboard/doctor joins and counts, in the same sabre-parity SQL style.
For Phase C, a sibling `storage.AdminWrites{DB}` holds the §1 carve-out:
`users`/`principals` inserts and password (`digesta1`) updates — the only
raw mutation SQL in the admin app, kept in one file so the carve-out stays
auditable. Everything else reuses existing types (`acl.Principal`,
`caldav.CalendarInfo`, `caldav.ObjectInfo`, …).

## 5. Phases

| Phase | Scope | Risk |
|-------|-------|------|
| **A** | Standalone binary, auth, dashboard, principal inspector, calendar/addressbook inspector, object browser | Pure reads; zero parity risk; most of the debugging value — **implemented** (`cmd/calendsd-admin`, `internal/admin`, `storage.AdminQueries`) |
| **B** | Privilege simulator, scheduling/sync debugger, doctor | Still read-only; simulated requests through the plugin chain — **implemented** (`server.NewLoggedIn`, `internal/admin/{simulate,debug}.go`, `storage` doctor) |
| **C** | Gated write actions (§3.8) + parity tests proving sabre sees admin-made changes correctly (extend the `PARITY=1` harness) | Writes; guarded by backends (plus the audited `AdminWrites` carve-out) + parity tests — **implemented** (`storage.AdminWrites`, `internal/admin/writes.go`, `TestParity/AdminMadeUserWorksInSabre`) |

## 6. Testing

- Handler tests follow the established pattern: mock backends
  (`caldav.MockBackend`, `acl.MemoryBackend`) + `httptest` against the admin
  mux, as in `internal/server/discovery_test.go`.
- Doctor checks get fixture-based tests (seed a mock/DB with each broken
  invariant, assert the finding).
- Phase C: parity tests on a live dev DB (`make db-start`), asserting sabre
  and calendsd both observe admin mutations with correct sync tokens.
