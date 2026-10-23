# Plan: Porting sabre/dav to a Go CalDAV Server

## 0. What we're starting from

The `dav/` directory is a full checkout of **sabre/dav** (~34k LOC of PHP):

| Module   | Files | LOC    | Role |
|----------|-------|--------|------|
| `DAV`    | 109   | 14,428 | WebDAV core: `Server`, `Tree`, node interfaces, property system, XML, auth, locks, sync |
| `CalDAV` | 66    | 11,365 | Calendars, calendar objects, `calendar-query`/`calendar-multiget` REPORTs, scheduling (iTIP), sharing, subscriptions |
| `CardDAV`| 20    | 3,371  | Address books (out of scope for a *cal* server) |
| `DAVACL` | 30    | 4,589  | Principals, privileges, ACL enforcement |

It also depends on three sibling libraries **not** in this checkout that we must also replace:
- `sabre/http` — PSR-7-ish request/response, routing, auth negotiation
- `sabre/vobject` — iCalendar/vCard parsing, recurrence expansion, iTIP
- `sabre/event` — event emitter (the plugin hook mechanism)
- `sabre/xml` — streaming XML reader/writer used for all WebDAV bodies

## 1. Scope decision (do this first)

Define the target so we don't port all 34k lines. Recommended MVP scope for a **cal server**:

**In scope**
- WebDAV core methods: `OPTIONS`, `PROPFIND`, `PROPPATCH`, `GET`, `PUT`, `DELETE`, `MKCOL`, `MKCALENDAR`, `REPORT`, `HEAD`
- CalDAV: calendar home, calendars, calendar objects (VEVENT/VTODO), `calendar-query`, `calendar-multiget`, `free-busy-query`, time-range filtering, `expand` of recurrences
- Principals + a working ACL model (even if single-user to start)
- Auth: HTTP Basic + Digest, pluggable
- Sync: `sync-collection` REPORT + CTag/ETag/sync-token
- A storage backend (Postgres or SQLite) mirroring sabre's `Backend\PDO` schema

**Defer / optional**
- CalDAV **scheduling** (iTIP/iMIP, `schedule-outbox`, free-busy of others) — large, do as phase 5
- Sharing & subscriptions
- WebDAV locks (`LOCK`/`UNLOCK`) — clients rarely need them for calendars
- CardDAV — drop unless we want a combined server later

**Out of scope**: the sabre PHP `Client`, `Browser` plugin (nice-to-have debug UI), `TemporaryFileFilterPlugin` (Mac `._` files — actually worth keeping for macOS clients).

## 2. Target architecture in Go

Keep sabre's proven object model; make it idiomatic Go. The core abstraction is a **tree of nodes** addressed by URL path, plus a **plugin/middleware chain** on the HTTP handler.

```
cmd/calendsd/main.go        wiring, config, listen
internal/dav/                WebDAV core (port of lib/DAV)
  server.go                  http.Handler; method dispatch; PROPFIND/PROPPATCH engine
  tree.go                    path -> node resolution, MOVE/COPY/DELETE
  node.go                    interfaces (see below)
  propfind.go, proppatch.go  property request/response objects
  xml/                       element (un)marshalers, {DAV:} namespace registry
  plugin.go                  Plugin interface + hook points
internal/caldav/             port of lib/CalDAV
  plugin.go                  advertises calendar features, wires REPORTs
  calendar.go, object.go     node implementations
  query.go                   calendar-query validator + time-range filter
  ical/                      iCalendar model (replaces sabre/vobject subset we need)
internal/acl/                principals + privilege checks (port of DAVACL)
internal/auth/               Basic/Digest backends
internal/storage/            Backend interface + postgres/sqlite impl
  schema.sql                 mirrors sabre PDO tables
```

### Node interfaces (direct port of sabre's `INode`/`ICollection`/`IFile`)

```go
type Node interface {
    Name() string
    SetName(string) error
    LastModified() time.Time
}
type Collection interface {
    Node
    Child(name string) (Node, error)       // GetChild
    Children() ([]Node, error)
    ChildExists(name string) bool
    CreateFile(name string, data io.Reader) (etag string, err error)
    CreateDirectory(name string) error
    Delete() error
}
type File interface {
    Node
    Get() (io.ReadCloser, error)
    Put(data io.Reader) (etag string, err error)
    ContentType() string
    Size() int64
    ETag() string
}
type Properties interface {                 // IProperties
    Props(names []string) (map[string]any, error)
    PatchProps(*PropPatch)
}
```

CalDAV then adds `Calendar` (a `Collection` carrying calendar metadata + supported components) and `CalendarObject` (a `File` whose payload is iCalendar), exactly like `lib/CalDAV/ICalendar.php` and `ICalendarObject.php`.

### Plugin/hook model (replaces sabre/event)

sabre's `Server` fires events (`beforeMethod`, `propFind`, `report`, `beforeUnbind`, etc.) and plugins subscribe. In Go, model this as an ordered list of plugins implementing optional hook interfaces:

```go
type Plugin interface{ Name() string; Init(*Server) }
type MethodHandler interface{ HandleMethod(ctx, method, path) (handled bool, err error) }
type PropFindHook interface{ PropFind(*PropFind, Node) }
type ReportHandler interface{ Report(name string, body xml.Node, path string) (bool, error) }
```

The core WebDAV behavior is itself a plugin (`CorePlugin`), matching sabre. CalDAV, ACL, Sync, Auth are each plugins registered on the server.

## 3. Go libraries

No mature all-in-one CalDAV framework exists in Go, so the WebDAV/CalDAV protocol layer is a hand port. We pull in focused libraries only for the two hard domains (iCalendar and recurrence) and lean on the stdlib for everything else.

| Concern | Library | Why / caveat |
|---|---|---|
| HTTP server (replaces `sabre/http`) | stdlib `net/http` | Passes non-standard methods (`PROPFIND`, `REPORT`, `MKCALENDAR`) through fine. Port only sabre's auth negotiation and `#` URL-encoding helpers. |
| iCalendar parse/serialize (replaces `sabre/vobject`) | **`github.com/emersion/go-ical`** | Closest thing to a vobject subset; round-trips VCALENDAR/VEVENT/VTODO + params. From the go-webdav/go-vcard maintainer — the ecosystem's center of gravity. Will need extending for edge cases. |
| Recurrence (RRULE) expansion | **`github.com/teambition/rrule-go`** | go-ical does **not** expand recurrences. This piece makes `calendar-query time-range` and `expand` correct: parse with go-ical, feed RRULE/RDATE/EXDATE into rrule-go. |
| WebDAV/CalDAV reference | **`github.com/emersion/go-webdav`** (+ its `caldav` subpkg) | Won't drop in as the server (client-oriented, thinner than sabre), but the best reference for `{DAV:}`/`{CALDAV:}` XML element types — vendor pieces rather than writing from zero. |
| XML (replaces `sabre/xml`) | stdlib `encoding/xml` + namespace registry | Handles clark-notation `{DAV:}response` via `xml.Name{Space,Local}`. The element registry (name → Go type) is custom — see §4. |
| Postgres driver | **`github.com/jackc/pgx/v5`** | Native driver; use with `database/sql` or pgx-native. |
| SQL access | **`sqlc`** (generate typed Go from hand-written SQL) | Critical: **you write the SQL**, transcribed from sabre's `Backend\PDO.php`, so behavior matches by construction. **Avoid a schema-owning ORM (GORM)** — it fights the existing schema. |
| Timezones | stdlib `time` + embedded `time/tzdata` + custom TZID mapping | tzdata gives IANA zones but does **not** parse VTIMEZONE blocks, and real clients send nonstandard TZIDs (Outlook "Customized Time Zone", Windows zone names). Need TZID→IANA mapping with a fallback that interprets the VTIMEZONE definition itself. Part of the phase-0 spike. |
| Auth | stdlib for Basic; hand-rolled Digest (reference: `github.com/abbot/go-http-auth`, archived) | Most modern clients use Basic-over-TLS; Digest for parity. The binding constraint is the shared `users.digesta1` convention (§5) — realm must match sabre's — not the library. |
| Event/plugin hooks (replaces `sabre/event`) | in-repo plugin interfaces | §2 |
| Logging (replaces `psr/log`) | stdlib `log/slog` | |
| Conformance | Apple **CalDAVTester**, **litmus** (external, run against the running server) | Same suites sabre validates against — see §7. Caveat: `ccs-caldavtester` is archived and Python-2-era — still a useful oracle, but budget real setup time. Use the maintained litmus (`notroj/litmus`), not stale forks. |

Honest summary: **go-ical + rrule-go + net/http do the heavy domain lifting; the protocol layer is a hand port.**

**Key risk up front**: vobject is the deepest dependency. Do a spike (phase 0) to confirm go-ical + rrule-go can round-trip real calendar data and expand recurrences over a time range, since `calendar-query` correctness hinges on it.

## 4. The XML problem (highest-effort core piece)

WebDAV request/response bodies are namespaced XML with polymorphic content (`{DAV:}propfind`, `{urn:ietf:params:xml:ns:caldav}calendar-query`, arbitrary dead properties). sabre solves this with a service that maps element names to (de)serializer callbacks.

Plan:
1. Build a `Registry` mapping clark-notation names (`{DAV:}response`) to Go types + marshal/unmarshal funcs.
2. Represent unknown/dead properties generically (raw XML fragment) so PROPPATCH can round-trip them.
3. Port the concrete element types incrementally, driven by what each REPORT needs: `{DAV:}propfind`, `multistatus`, `prop`, `href`, `{CALDAV:}calendar-query`, `filter`, `comp-filter`, `time-range`, `calendar-data`.
4. Golden-file tests: capture real request bodies from Apple Calendar / Thunderbird / DAVx5 and assert byte-level round-trips.

This subsystem gates everything; budget real time for it.

## 5. Storage backend — Postgres, shared live with sabre-dav

**Hard constraint: the Go server runs alongside a running sabre-dav instance against the *same* Postgres database.** Both processes read and write the same tables concurrently from day one. This is the strongest form of "keep the DB as-is" and drives every decision below:
- Keep the schema **byte-identical** to sabre's `examples/sql/pgsql.*.sql` — no `ALTER TABLE`, no renamed columns, no migration that sabre wouldn't understand. (A separate `schema_version`-type table is fine only if sabre never touches it; prefer adding nothing.)
- Concurrency is handled by the DB: rely on the existing unique indexes (`calendarobjects_ukey` on `(calendarid, uri)`, etc.) and transactions. No app-level coordination between the two servers.
- Every value the Go server writes must be **readable by sabre**, and vice versa — this is a correctness requirement, not just a test.

**Postgres only.** Drop the MySQL/SQLite variants; use `examples/sql/pgsql.*.sql` unchanged. The schema *is* the compatibility contract — plain relational SQL with no PHP-specific features, so the tables carry over verbatim.

Define a Go `Backend` interface matching `BackendInterface.php` (`CreateCalendar`, `GetCalendarObjects`, `GetCalendarObjectByUID`, `CalendarQuery`, `GetChangesForCalendar`, …), implemented with `sqlc` queries transcribed from `Backend\PDO.php`.

**Tables reused unchanged:** `calendars`, `calendarobjects`, `calendarinstances`, `calendarchanges`, `principals`, `groupmembers`, `users`. The semantics to replicate exactly (conventions the schema doesn't enforce, so the Go code must):
- **`calendars` vs `calendarinstances` split** — shared calendar state (`id`, `synctoken`, `components`) in `calendars`; each principal's view (`displayname`, `uri`, `calendarcolor`, `access`) in `calendarinstances`. Objects hang off `calendars.id`. This split is how sharing works — preserve it even single-user.
- **`calendarobjects.calendardata`** is raw iCalendar bytes in `BYTEA`. `etag` = `md5(calendardata)` in quotes — keep the exact algorithm or every client re-syncs.
- **`lastmodified`, `firstoccurence`, `lastoccurence`** are Unix-epoch integers, not SQL timestamps. Compute first/last occurrence on PUT (via rrule-go) so `time-range` queries prefilter in SQL, then refine in memory — this is how sabre keeps queries fast.
- **`synctoken`** is a monotonic integer per calendar; each change appends to `calendarchanges` with `operation` (1=add, 2=modify, 3=delete). Client-facing token is `http://sabre.io/ns/sync/<n>` (`Sync\Plugin::SYNCTOKEN_PREFIX` — the older `sabredav.org` prefix is from pre-3.0 releases). Match it exactly: sabre *rejects* tokens that don't start with its precise prefix, so a mismatch forces every client into a full re-sync.
- **Write atomicity — do better than sabre, compatibly.** sabre's `addChange()` is two unwrapped statements (INSERT change row at the *current* synctoken, then `UPDATE … synctoken + 1`), so concurrent writers can record duplicate synctoken values — a race that exists even between two sabre processes today; `sync-collection` semantics tolerate it. The Go server wraps object-write + change-row + token-increment in a single transaction — strictly safer and fully compatible. The parity test must compare sync *behavior*, not assume identical interleaving.
- **PUT denormalization must replicate `getDenormalizedData()` exactly:** only VEVENT gets `firstoccurence`/`lastoccurence`; component type = first non-VTIMEZONE component (400 if none); all-day events without DTEND get DTSTART+1 day; no RRULE → DTEND, else DURATION, else DTSTART; negative timestamps clamp to 0; and recurrence expansion caps at **`MAX_DATE = 2038-01-01`** — infinite RRULEs store that timestamp, finite ones expand until it. Diverge on any of these and time-range prefilters (and the parity test) disagree between the two servers.
- **Backend addressing:** sabre's calendar backend addresses objects by a `[calendarId, instanceId]` pair (it throws if given a scalar). The Go `Backend` interface takes that tuple from day one — retrofitting it after building on a scalar ID is painful.

**`users` table — reused unchanged, and it constrains auth.** sabre's auth backend reads `users (username, digesta1)` where `digesta1 = md5(user:realm:password)`. The **realm is baked into the hash**, so the Go server must use the same realm string as the sabre instance forever — for Digest *and* Basic (Basic verification = compute the same A1 from the cleartext and compare). This matters more than which Digest library we pick.

**`propertystorage` — simplified but sabre-compatible.** Since sabre may read what we write, we can't invent our own encoding. sabre tags each value with a `valuetype`: **1 = string**, **2 = XML** (a sabre/xml-serialized fragment — language-neutral), **3 = PHP-serialized object** (language-*specific*). The simplification: **only read and write valuetype 1 (string) and valuetype 2 (XML).** Both are cross-readable; skip type 3 entirely (never write it, and treat any type-3 row we encounter as opaque/read-through — sabre owns those). Most properties clients care about (`displayname`, `calendar-color`, …) are real columns anyway, so `propertystorage` carries only the long tail and can be deferred past the MVP without breaking co-existence.

**Parity test (the real proof of co-existence):** point the reference PHP sabre instance and the Go server at the *same* Postgres DB; create/modify/delete via one, read via the other; assert identical ETags, sync-tokens, and REPORT output — now including `propertystorage` for value types 1 and 2. This test is the acceptance gate for "runs alongside sabre-dav."

## 6. Phased delivery

**Phase 0 — Spikes, scaffolding & test corpus (2–3 wks)**
- **Extract the test corpus first (§7):** instrument the reference sabre checkout, dump its request/response tests to `testdata/corpus/`, and build the Go corpus runner + XML canonicalizer. Everything below is developed against it.
- iCalendar spike: prove go-ical + rrule-go round-trip and expand recurrences. Acceptance criteria include timezone handling: nonstandard TZIDs resolve via the TZID→IANA mapping, and a raw VTIMEZONE definition can be interpreted as a fallback.
- XML spike: PROPFIND request → multistatus response round-trip.
- Repo skeleton, CI (reports corpus coverage), `http.Handler` that answers `OPTIONS` with DAV headers.

**Phase 1 — WebDAV core (2–4 wks)**
- Tree + node interfaces, path resolution.
- `PROPFIND` (allprop/propname/named), `PROPPATCH`, `GET`, `PUT`, `DELETE`, `MKCOL`, `MOVE`, `COPY`.
- In-memory + filesystem backend for testing.
- Pass the [`litmus`](https://github.com/notroj/litmus) WebDAV test suite basic/props classes.

**Phase 2 — Auth + Principals + minimal ACL (1–2 wks)**
- Basic/Digest auth plugin, verifying against the shared `users.digesta1` column with sabre's realm (§5).
- Principal collection, `current-user-principal`, `principal-URL`.
- Enough ACL to gate calendars to their owner.

**Phase 3 — CalDAV read path (2–3 wks)**
- Calendar home / calendar / calendar-object nodes.
- `MKCALENDAR`, calendar properties (`supported-calendar-component-set`, `calendar-color`, CTag).
- `calendar-multiget`, `calendar-query` with `comp-filter`/`prop-filter`/`time-range`, `calendar-data` with `expand`.
- Storage-backed by the shared Postgres DB (§5); run against a live sabre instance on the same DB and pass the parity test.

**Phase 4 — Sync + interop hardening (1–2 wks)**
- `sync-collection` REPORT, sync-tokens, ETag/CTag correctness.
- Validate against real clients: **Apple Calendar (macOS/iOS), Thunderbird, DAVx5**.
- Run **[CalDAVTester](https://github.com/apple/ccs-caldavtester)** suites (archived, Python 2 — budget setup time; treat as an oracle, not a turnkey gate).

**Phase 5 — Scheduling (optional, 3–5 wks)**
- iTIP: `schedule-outbox`, `schedule-inbox`, auto-processing of invites, free-busy of attendees.
- This is the largest remaining chunk (`CalDAV/Schedule/*`) — port only if invitations are a requirement.

**Phase 6 — Sharing/subscriptions (optional)**

**Known gaps — Calendar-Server infrastructure we do not provide (surfaced by the full CalDAVTester sweep).** Running the entire Apple CalDAVTester suite (`make caldavtester-full`, all ~193 files) leaves ~64 files in the **ERROR** state — they fail in their `<start>` provisioning and never run a single assertion, because their fixtures assume Calendar-Server features that calendsd (a sabre-lineage server) does not implement. These are *expected non-runners, not test failures*. To make those files runnable we would need to build, roughly in order of leverage:

- **Server-side implicit scheduling / iMIP inbox delivery** — the `CalDAV/implicit*.xml` family (auto-processing of invites into attendee inboxes, PARTSTAT changes propagating back to the organizer, schedule-tag, auto-accept). calendsd has the iTIP broker + outbox POST + inbox nodes, but not the auto-delivery semantics these tests drive (they PUT an event as the organizer and expect the server to materialize inbox items on each attendee automatically).
- **Group principals** — bookable/gatewayed `groups/` principals with membership; several `implicit*group*` and `sharing-groups*` files provision a group at start.
- **Resource & location principals** — auto-accepting `resources/`/`locations/` principals (rooms, projectors) with proxy-driven booking; `implicitauto*`/scheduling files depend on them.
- **Proxy membership management** — calendar-proxy principals *and* ACE enforcement of delegation already exist; what's missing is Calendar Server's runtime membership management (PROPPATCH/POST of `group-member-set` on the proxy sub-principals) that `sharing-proxies.xml`/`implicitproxy.xml` use to provision delegates at start.
- **`add-member` / bulk POST**, **managed attachments**, and the **Browser plugin** GET listings — smaller CS extensions a few files assume.

None are required for the standards-based CalDAV/CardDAV surface (the curated `make caldavtester` subset); they are the roadmap if fuller Calendar-Server parity is ever a goal. Phase 7 below picks up the tractable subset.

**Phase 7 — Calendar-Server parity (in progress)**
- **7a. Harness provisioning — DONE.** Seeded `admin`, `resource01..20`, `location01..10` (with calendars), `group01..40` principals in `scripts/caldavtester/seed.sql`, and remapped the resource/location/group serverinfo substitutions (`$rprincipaluri`, `$rcalendarpath`, `$rinboxpath`, `$rcuaddrurn`, …) from CS `__uids__/GUID` form onto the sabre layout. Unblocks the `<start>` of most ERROR files.
- **7b. Auto-accept scheduling — DONE.** `SchedulePlugin.AutoSchedule` (enabled via `-auto-schedule-prefixes resource,location`; off by default — sabre has no such feature): REQUESTs to matching principals are written to their default calendar with PARTSTAT=ACCEPTED, or DECLINED on a free-busy conflict (declined bookings never land on the calendar); no inbox drop; CANCELs delete the copy; the iTIP REPLY to the organizer is queued during the organizer's own PUT and flushed by the new `AfterWrite` hook once the object is persisted. Also fixed a real broker port divergence found on the way: `ProcessMessage` REPLY now sets `SCHEDULE-STATUS` from the reply's REQUEST-STATUS (default 2.0) and clears RSVP, like sabre's `processMessageReply`, instead of deleting it. A minimal opt-in `-browser` plugin (HTML index for GET on collections) unblocks the `<start>` GETs. `CalDAV/implicitauto1.xml`: ERROR → 23 ok / 13 failed (the 13 are CS's `EMAIL=`-param/urn address canonicalization in exact-match tests).
- **Phase 7 impact so far (full sweeps, 2026-07-25):** after 7a+7b: ERROR 64 → 26, ok cases 684 → 928. After 7c+auto-provision: **ERROR 24, FAIL 22, ok cases 1237** (baseline 684 → +81%), CRASH 0. Curated run: 117 → 137 ok. The 26 remaining ERRORs need: CS sharing provisioning flows (`sharing-*`, ~10), trash (3), managed-attachments POST (2), CS auto-schedule-mode PROPPATCHes and client-leniency starts (`implicitautomodes`/`badclients`/…, ~7), and the DELETE-default-collection reprovisioning gap (`CardDAV/sync-report`, `mkcalendar` et al.).
- **7c. Proxy membership management — DONE.** The PROPPATCH plumbing already existed; what was missing: (1) `principals` rows for the `calendar-proxy-read/-write` sub-principals (sabre's own sample SQL seeds them; `SetGroupMemberSet` requires them) — now seeded per account, plus CS's stock delegation of every resource/location to user01; (2) `{CS}record-type` (`read/write-delegate-group`) on proxy nodes; (3) `calendar-proxy` in the DAV header (a real sabre-parity gap — sabre's `getFeatures` advertises it); (4) **a real ACL-plugin parity fix**: sabre's `beforeMethod` does not gate PROPFIND — the propFind hook 403s each property inside a 207 instead; our port returned request-level 403 (REPORT stays gated, stricter than sabre, so the calendar-query prefilter can't leak hrefs); (5) opt-in `-principals-readable` (authenticated users may read principal resources, CS behavior). `calendaruserproxy.xml`: 17 → **67 ok** / 30 failed; the 30 need CS per-user properties (peruser displayname/color per delegate) — see 7f.
- **7c½. Auto-provisioning — DONE.** Opt-in `-auto-provision`: when a principal's last calendar/address book is deleted, the default `calendar`/`addressbook` is recreated on the next home access, like Calendar Server. Removes the harness's structural "DELETEALL kills the default collection" failure class (`CardDAV/put.xml` 0/18 → 15/18). Off by default (sabre never auto-creates).
- **7d. Group attendee expansion — DONE** (opt-in `-expand-groups`). `SchedulePlugin.expandGroupAttendees` rewrites a group ATTENDEE (a principal with a non-empty `{DAV:}group-member-set`) into a display-only `CUTYPE=X-SERVER-GROUP` placeholder (`SCHEDULE-STATUS=2.7`, no PARTSTAT/RSVP) plus one real ATTENDEE per member carrying `MEMBER="<group>"`; the iTIP broker skips X-SERVER-GROUP attendees so only members are scheduled. Re-encode is gated on whether expansion changed anything, so plain PUTs stay byte-exact (parity). Advertises `calendarserver-group-attendee`. Exact-match GET tests in `implicitgroup.xml` stay blocked on CS's `urn:x-uid` address canonicalization (a directory-GUID concept we don't model) — the behavior itself is verified (`TestExpandGroupAttendees`).
- **7e. Add-Member (RFC 5995) — DONE.** The `{DAV:}add-member` property is advertised on every calendar (`<collection>/;add-member`), and a POST there stores the body under a server-assigned name and returns 201 + Location, running the same write-interceptor chain (validation + scheduling) a PUT would so it is not a validation bypass. `CalDAV/add-member.xml`: ERROR → **4/4**. Test `TestAddMember`.
- **7g. Bulk POST + managed attachments** — the two remaining Calendar-Server extensions, both large and *proprietary* (not standards): bulk POST is CS's multi-resource POST format (`bulk.xml`), and managed attachments (`managed-attachments*.xml`) need a whole binary-attachment store (POST/GET/PUT/DELETE attachments, `ATTACH;MANAGED-ID`, dropbox, quota). Deferred — low priority, no standards value, and neither is needed by any real CalDAV client. This closes the tractable/standards-based Calendar-Server parity work; what's left is CS-proprietary only.
- **7f. Per-user properties — DONE** (opt-in `-per-user-props`). Decoded CS's actual model from the tests: per-user treatment applies to a *whitelist* of personal presentation properties (`calendar-color`, `calendar-order`) — everything else a delegate writes is shared, which is why CS read-delegates hold `{DAV:}write-properties`. Implementation: `dav.PerUserPropsPlugin` — a new `PrePropPatchHook` (fires before node handlers) claims whitelisted writes by non-owners into the property-storage backend under a synthetic child-look-alike path `<path>/\n<principal>` (never collides with real paths, invisible to a co-existing sabre, and swept/renamed by the existing AfterUnbind/AfterMove prefix queries so a recreated collection cannot resurrect old values); a very-late PropFind hook (registered after global property storage) force-overlays the reader's own values. Admins exempt (harness provisioning must stay global). Plus one more **sabre-parity fix**: `CalendarHome::getACL` grants the calendar-proxy groups access (write proxies read+write, read proxies read) — our port was owner-only. `calendaruserproxy.xml`: 67 → **80 ok**; residual 17 = nested-group membership semantics, CS tracking reports, and 2 known DURATION-strictness tests.

**Deployment note — strangler-fig migration** (historical: the cutover is done and sabre is no longer a rollback target, see docs/strangler-fig.md). Because both servers share one Postgres DB (§5), the Go server can take over incrementally instead of in a big-bang cutover: front both with a reverse proxy and route selected methods/paths to Go as each phase lands (e.g. read-only `REPORT`/`PROPFIND` first, writes later), with sabre handling everything not yet ported. The parity test gates each routing change. Two routing caveats: `MOVE`/`COPY` carry a `Destination` header, so source and destination must land on the same implementation; and auth must be indistinguishable on both sides (same realm — forced by `digesta1` anyway (§5) — and Digest nonce handling that tolerates the other server's nonces or degrades gracefully).

## 7. Test-first porting strategy

**Convert the tests before the code.** The port is driven red→green against a corpus extracted from sabre's own suite, so "does the Go server still behave like sabre" is answered continuously, not at the end. sabre ships **209 test files** (`*Test.php`; 249 PHP files under `tests/` counting fixtures/helpers); the leverage is that a large share are *black-box HTTP tests* — set up fixture state, issue a request, assert the response — which are language-neutral behavioral specs.

### What's portable vs. not

- **Portable (the corpus, ~39 request-style tests in `CalDAV`/`DAV` today):** tests that build an HTTP request and assert the response. `Issue205Test` is typical — fixture calendars + calendar objects, then a `REPORT` with a `calendar-query` body, asserting the `multistatus`. The `Expand*`, `FreeBusyReport`, `GetEventsByTimerange`, `ICSExport`, and most `Issue*` regression tests are all this shape. These become the golden corpus.
- **Not portable (leave behind):** unit tests of internal PHP classes whose API won't exist in Go (`CalendarQueryValidatorTest`, `JCalTransformTest`, serialization internals). Their *intent* is re-expressed as Go unit tests where relevant, but they aren't part of the shared corpus.

### Extraction: capture, don't hand-transcribe

Rather than manually copy request/response strings out of ~39 PHP files (error-prone, and expected responses are often computed), **instrument sabre once** and let it emit the corpus:
1. Add a thin PHPUnit listener / `Sapi` shim to the reference sabre checkout that, for every request-style test, dumps a fixture record: `{ setup: {calendars, objects, principals}, request: {method, uri, headers, body}, response: {status, headers, body} }` to `testdata/corpus/<TestName>.json` (or YAML).
2. Run the sabre suite once → authoritative corpus, generated from the real implementation. Re-runnable when we bump the sabre version.
3. Commit the corpus into the Go repo as `testdata/`.

### Running the corpus in Go

A single table-driven Go test loads each record, seeds the shared Postgres DB (§5) with the fixture state, replays the request against the Go `http.Handler`, and compares the response. Comparison must be **semantic, not byte-exact**:
- XML bodies compared after **canonicalization** — namespace-*prefix*-insensitive (`D:` vs `d:`), attribute-order-insensitive, and order-insensitive across `{DAV:}response` elements in a `multistatus`. Build this canonicalizer once; it's also what the golden XML round-trip tests (§4) use.
- Volatile headers (`Date`, server token) masked; `ETag`/sync-token compared structurally.

Each ported feature flips its corpus entries from skip→pass; CI reports corpus coverage (e.g. "312/420 records green") as the port's progress metric.

### The full test pyramid

1. **Corpus (this section)** — behavioral parity with sabre, extracted from its suite. The primary driver.
2. **Go unit tests** — XML canonicalization, recurrence expansion (rrule-go), time-range filter, backend CRUD, `firstoccurence`/`lastoccurence` computation (incl. the `MAX_DATE` 2038 cap and all-day/no-DTEND edge cases, §5).
3. **Live co-existence parity test (§5)** — Go and sabre on one Postgres, cross-read/write, identical ETags/sync-tokens/REPORTs. Catches DB-level divergence the corpus can't.
4. **External conformance** — `litmus` (WebDAV), Apple `CalDAVTester` (CalDAV/scheduling). The same suites sabre uses; they define "done."
5. **Real-client interop** — Apple Calendar (macOS/iOS), DAVx5, Thunderbird, Evolution. Manual + captured-traffic replay.

## 8. Key risks & mitigations

| Risk | Mitigation |
|---|---|
| vobject depth (recurrence, iTIP) has no drop-in Go equivalent | Phase-0 spike; be ready to port sabre's recurrence/iTIP logic by hand; keep scheduling optional |
| WebDAV XML edge cases (namespaces, dead props, mixed content) | Namespace-aware registry + golden-file tests from real clients |
| Client quirks (each client bends the spec) | Test against the real client matrix early (phase 4), not just RFCs |
| ACL complexity balloons | Start single-owner; add aggregate privileges only when sharing is needed |
| Scope creep into full sabre parity | Enforce §1 scope; CardDAV/locks/sharing explicitly deferred |

## 9. Effort estimate

MVP (phases 0–4, a multi-calendar CalDAV server co-existing with sabre on one Postgres, passing the extracted corpus + CalDAVTester core + real clients): **~11–15 focused weeks** for one engineer (Phase 0 grew to include corpus extraction, which pays back across every later phase). Add ~4–5 weeks for scheduling (phase 5).

## 10. First concrete steps

1. Confirm scope in §1 (esp. scheduling in/out). Storage is decided: **Postgres, shared live with sabre-dav** (§5).
2. Stand up the co-existence harness: one Postgres from `examples/sql/pgsql.*.sql`, a running reference sabre-dav against it, and the parity test scaffold. This is the acceptance gate for everything that follows.
3. **Extract the test corpus (§7)** from that same sabre checkout and build the Go corpus runner + XML canonicalizer — the red→green driver for the whole port.
4. Phase-0 spikes: iCalendar round-trip + PROPFIND round-trip. These de-risk the two hardest subsystems before committing to the full port.
5. Stand up the repo skeleton (§2) and an `OPTIONS`-answering handler behind the strangler-fig proxy.
