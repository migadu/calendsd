# Improvements — Plan

Source: a follow-up look (2026-10-23) after the whole-codebase review in
[review-fixes.md](review-fixes.md). Only items that review did not cover are
listed. Sabre is no longer a rollback target, so sabre-compatible storage is
not a constraint: new state may live in calendsd-only tables, and sabre's
tables may be altered.

Items are ordered by priority. Each one lists the evidence, the change, and
how it is tested. One commit per item (or per bullet where an item has
several), with a regression test.

| # | Item | Why now | Size |
|---|------|---------|------|
| 1 | Schedule-Tag (RFC 6638) | spurious conflicts between organizer and attendee changes | M–L |
| 2 | Change-log retention | **done** (4747c16) | S |
| 3 | Resource-size and per-user limits | resource size **done** (614d905); per-user limits open | M |
| 4 | Retire sabre from CI and the docs | **done** (b7ec7da) | S |
| 5 | Migration runner, base schema, schema cleanup | runner and baseline **done** (08a6f5c); cleanup open | M, then L |
| 6 | Fuzz tests and CI gates | iCalendar/RRULE targets **done** (ec253ba), found an open RRULE DoS; rest open | M |
| 7 | Smaller items | login throttling, streaming export | S each |
| 8 | `/health` endpoint | **done** (see below) | S |

## 1. Schedule-Tag

**Evidence.** No `Schedule-Tag` header and no `If-Schedule-Tag-Match` handling
anywhere in `internal/` (grep for `schedule-tag` is empty). Clients that
implement RFC 6638 (Apple Calendar, Thunderbird) fall back to ETags. When the
server applies an attendee REPLY to the organizer's copy, the ETag changes, so
the organizer client's next `If-Match` PUT gets a 412 and has to refetch and
retry (often shown as a conflict). A client that sends no precondition
overwrites the REPLY and loses the attendee's PARTSTAT.

**Change.**
- Store a schedule tag per calendar object in a calendsd-only table
  (`calendarobject_schedule_tags (calendarobjectid, tag)`, cascading delete),
  or as a column once item 5 lands.
- Bump rules, for both sides of a scheduling exchange:
  - Organizer copy: bump on a write by the organizer. Do **not** bump when the
    server applies an attendee REPLY (only PARTSTAT/SCHEDULE-STATUS change).
  - Attendee copy (local attendees): bump when the server delivers an
    organizer update to it. Do **not** bump when the attendee changes only
    their own participation.
- Return `Schedule-Tag` on GET/PUT of scheduling object resources, and the
  `{CALDAV:}schedule-tag` property in PROPFIND/REPORT.
- Honour `If-Schedule-Tag-Match` on PUT/DELETE: 412 on mismatch. When it
  matches but the ETag is stale, accept the write and merge into it the
  attendee state the client has not seen: per ATTENDEE, `PARTSTAT` and
  `SCHEDULE-STATUS`, on every instance, including overridden instances that
  exist only because a REPLY added them. The merge is most of this item's size.
- Advertise nothing new in the DAV header (it is part of `calendar-auto-schedule`).
- Check the RFC section references against the text before citing them
  (the headers are defined in §8).

**Tests.** Unit: tag unchanged across a REPLY applied to the organizer copy,
bumped on an organizer PUT; attendee copy bumped on an organizer update,
unchanged on the attendee's own PARTSTAT change; 412 on a stale tag; organizer
PUT with a matching tag after a REPLY changed the ETag succeeds and keeps the
attendee's PARTSTAT, including on an override instance. CalDAVTester: the
`schedule-tag` cases in `implicit*.xml` should move from fail to ok — record
the counts.

## 2. Change-log retention — done

Built as planned, with these details settled in the code:

- `[retention] change_log_days` (default 90; negative keeps everything),
  pruned hourly by a background loop in calendsd
  (`storage.RunChangeLogPruner`).
- Migration 0003 adds `created` to both change tables (existing rows get the
  migration time) and `synctoken_floor` to `calendars` / `addressbooks`.
- Floor semantics follow this code's tokens, where a client token T asks for
  rows with `synctoken >= T`: the floor F means "rows below F may be gone",
  and a token is valid iff T ≥ F. (The plan's "highest pruned token P, valid
  iff T ≥ P" was off by one for these semantics.)
- The REPORT reads the floor after the rows, so a prune committing in between
  is caught. `calctl stats` and the dashboard show change-row totals, not a
  per-collection breakdown.

## 3. Resource-size and per-user limits

**Evidence.** `{CALDAV:}max-resource-size` is advertised as 10 MB but not
enforced (`internal/caldav/plugin.go:83`, "matching sabre"); CardDAV does
enforce its limit. `quota-used-bytes` / `quota-available-bytes` are in the
allprop set (`internal/dav/propfind.go:44`) but nothing serves them. There is
no cap on calendars, objects or bytes per user.

**Change.**
- Enforce `max-resource-size` on CalDAV PUT/POST (add-member, outbox) with 403
  and the `{CALDAV:}max-resource-size` precondition, sharing the CardDAV code
  path. Request bodies are already capped with `MaxBytesReader`
  (`internal/dav/server.go:214`), so this is a size check on the read body,
  not a new read path.
- `[limits]`: `max_collections`, `max_objects_per_collection`,
  `max_bytes_per_user` (0 = unlimited, the default, so nothing changes until
  configured). Check in the write interceptor. Over a byte quota: 507 with
  `{DAV:}quota-not-exceeded` (RFC 4331). Over a count limit there is no
  standard precondition: use the same 507 + `{DAV:}quota-not-exceeded`, and
  say which limit was hit in the error body's text.
- Serve RFC 4331 quota properties on homes when `max_bytes_per_user` is set;
  compute usage with one aggregate query (sum of `size`), cached per request.
- Metric `calserver_quota_rejections_total{kind}`.

Status: `max-resource-size` is enforced on PUT and Add-Member (614d905). The
outbox POST is left to the 100 MiB request cap: it carries iTIP and free-busy
requests, not stored resources. The `[limits]` and quota bullets are open.

**Tests.** Oversize PUT → 403 with the precondition; over-quota PUT → 507
with `quota-not-exceeded`; over the collection/object count → 507; quota
properties match the stored sizes.

## 4. Retire sabre from CI and the docs — done

**Evidence.** CI still installs PHP/Composer and gates on the live parity test
(`.github/workflows/ci.yml`); `scripts/sabre-server.php` and
`docs/strangler-fig.md` still describe sabre as a rollback target. The parity
test also fails locally when PHP is not set up.

**Change.**
1. Drop the parity step from CI; keep `TestParity` behind `PARITY=1` for one
   release, then delete it with `scripts/sabre-server.php`.
2. Mark `docs/strangler-fig.md` historical (or delete it); update README and
   docs/design/PLAN.md.
3. Keep `testdata/corpus/`. It is derived from sabre but not tied to PHP, and
   it still catches regressions. Stop regenerating it and freeze the outputs of
   `scripts/*-golden.php`.

Dropping the `dav/` checkout itself waits for item 5: the sabre base schema
lives only there.

Status: done. CI no longer runs the parity test (opt-in with `PARITY=1`), and
the unit-test step now starts the dev Postgres, so the storage and migration
tests run in CI instead of skipping (previously only the parity step started
it). Still to do after one release: delete `TestParity` and
`scripts/sabre-server.php`.

## 5. Migration runner, base schema, schema cleanup

**Evidence.** `scripts/schema/pgsql.calserver.sql` is safe to re-run but says it is
"not a migration runner": it only refuses to run over missing columns. It is
also only the extension schema (`scripts/schema/schema.go` embeds just that
file). The sabre tables (`calendarobjects`, `calendarchanges`, `cards`, …) are
defined only in `dav/examples/sql/pgsql.*.sql`: the deploy never creates them
(they exist from the sabre era), and ten storage test files glob that path to
build their databases. A fresh install has no supported way to create them.

**Change.**
- Numbered migrations embedded in the binary
  (`internal/schema/migrations/NNNN_name.sql`), a `schema_migrations` table,
  each applied in its own transaction. `calctl schema apply` runs pending ones
  (the deploy already calls it), under the advisory lock and `--lock-timeout`
  it already has.
- `0001_baseline.sql` = the sabre DDL from `dav/examples/sql/pgsql.*.sql`
  (with `IF NOT EXISTS`) followed by the current extension file. Both are safe
  to re-run, so 0001 simply runs on an existing database too; no "mark applied
  if the tables exist" detection.
- Switch the storage tests to the embedded migrations instead of globbing
  `dav/`, then drop the `dav/` checkout once nothing reads it.
- The health schema check compares the latest applied version with the
  binary's: older DB → fail (schema not applied); newer DB (binary rolled
  back) → warn, not fail.
- Then the schema cleanup that sabre forced, one migration each: occurrence
  bounds as `bigint`/`timestamptz` instead of int4 epochs with the 2038 cap; a
  non-null `etag` (drop the md5 fallback once backfilled). The `SabreDAV`
  realm **stays**: it is baked into every `users.digesta1`.

Status: the runner and baseline are done (08a6f5c), as `0001_sabre` (the
sabre DDL, made conditional, without sabre's admin/admin seed) and
`0002_extension` (the former `pgsql.calserver.sql`, now frozen). The runner
lives in `internal/schema` (`schema.Apply`), so test helpers can use it without
an import cycle. Tests build databases through `internal/storage/storagetest`;
only `TestParity` still reads `dav/`, so `dav/` can go with it. The schema
cleanup is open.

**Tests.** Fresh DB → all applied and the sabre tables exist; existing DB →
0001 runs without changes, then only the new ones; concurrent `apply` → one
runs, the other waits; DB newer than the binary → health warns.

## 6. Fuzz tests and CI gates

**Evidence.** No `func Fuzz` anywhere. RRULE expansion depends on
`github.com/teambition/rrule-go`, which sees little maintenance, yet
time-range queries, `expand`, free-busy and occurrence bounds all depend on it.
CI runs only `go vet` and `go test`; litmus is `continue-on-error`;
CalDAVTester and the JS/Playwright suites are not in CI.

**Change.**
- Fuzz targets: iCalendar parse → denormalize (no panic; first ≤ last
  occurrence), RRULE expansion (bounded instance count, instances inside the
  window, monotonic), vCard parse/validate, DAV XML request bodies
  (PROPFIND/REPORT/MKCALENDAR), the config TOML parser. Seed from
  `testdata/corpus/` and `internal/caldav/ical/testdata/`. `go test` only
  replays the seeds; actual fuzzing runs in a scheduled job, one `-fuzz`
  target per invocation with a time budget each.
- Status: the iCalendar and RRULE targets exist (`internal/caldav/ical/fuzz_test.go`).
  They found and fixed three client-triggerable panics (go-ical decoder on
  `X;P=`, rrule-go on out-of-range BYDAY ordinals, rrule-go on a year-0
  DTSTART). **Open:** a sub-daily rule that never matches
  (`FREQ=HOURLY;BYSETPOS=2`) makes rrule-go walk every period to year 9999
  (`MAXYEAR` is a constant; UNTIL is checked only on produced dates): about
  5 s of CPU per expansion, on every PUT and time-range query touching it.
  Fixing it needs a bounded iterator, i.e. a patched or replaced rrule
  library. Until then `FuzzRecurrence` keeps stopping on these inputs.
- A differential test of expansion against libical (via `icalrecur` in the
  Nix shell) over a generated rule set, in the same nightly job.
- CI: add `staticcheck` and `govulncheck`; run `cmd/calclient` unit tests
  (`npm test`) and the Playwright e2e suite; run the curated CalDAVTester set
  and litmus with their current pass counts pinned (curated: 137 ok) — fail on
  a drop, not on the absolute number.

## 7. Smaller items

- **Login throttling.** `internal/auth` logs failed logins but does not slow
  repeated attempts. First check whether rcptd throttles them in rcptd mode;
  if not, add token buckets in front of the backend for both Basic and Digest
  (429 with `Retry-After`), with a metric. Key them on (username, IP) and on
  IP alone, never on the username alone: that would let anyone lock out a
  known account. Behind the reverse proxy, take the IP from a trusted
  `X-Forwarded-For` only.
- **Streaming export.** `GET ?export` builds the whole calendar in memory and
  writes it at once (`internal/caldav/export.go:108`) under a 60 s
  `WriteTimeout`. Stream objects as they are read, and extend the write
  deadline per chunk with `http.ResponseController`. The deduplicated
  `VTIMEZONE`s go before the first component, so collect the TZIDs in a first
  pass over the (small) metadata, or emit them from the calendar's
  timezone set; jCal is one JSON array, so stream its elements with the
  enclosing brackets written separately. Once the 200 is sent, an error can
  only truncate the body: log it and abort the connection so the client sees
  an incomplete download rather than a valid-looking short file.
- **`rows.Err()`** in the job-depth collector — done (f1d4aa4).
- **Stale deployment comment** about the schema step — rewritten for the
  migrations, which also report `changed` now; outside this repo.

## 8. `/health` endpoint — done

An unauthenticated `GET`/`HEAD /health` on the DAV listener, at the root
whatever the base URI (`internal/server/health.go`):

- `200 {"status":"ok","database":"ok"}` when a DB ping answers within 2 s;
  `503 {"status":"unavailable","database":"unreachable"}` otherwise. Without a
  database (in-memory dev mode) it answers `200 {"status":"ok"}`.
- The DB error is logged, never returned (the endpoint is public).
  `Cache-Control: no-store`; other methods get 405.
- Wrapped outside the access/request logs (no log flood from monitors) but
  inside metrics (`route="health"`).

Possible follow-up: switch the `calendsd` row of `calctl health` from an
`OPTIONS` probe to `/health`, once every deployed version serves it.
