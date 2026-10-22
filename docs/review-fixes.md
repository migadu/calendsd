# Code Review Fixes — Plan

Source: a whole-codebase review (2026-10-22) for bugs, PHP-isms carried over
from sabre/dav, and simplifications. About 90 findings. Each was checked
against callers before it was listed; the top three security findings
(MOVE/COPY destination delete, `ShareAccess` never set, mock root) were
re-checked by hand.

Sabre compatibility is no longer a constraint (see PLAN.md / strangler-fig.md),
so sabre quirks are removed rather than preserved.

## Execution

The work is split into **workstreams with disjoint file ownership**, so they
can proceed in parallel in one tree without stepping on each other. Two
cross-cutting refactors (storage `context` threading, hook dispatch) touch
many packages and run **after** phase 1, sequentially.

Rules for every workstream:

- Each bug fix gets a regression test where the package already has a test
  harness for that area (mock backend, corpus, httptest). Storage fixes that
  need a live DB get a test behind the existing dev-DB gate.
- `go vet ./... && go test ./...` must pass at the end of each phase.
- One commit per logical fix group (roughly one per S-row or bullet below),
  not one commit per workstream.
- Each workstream lands its S-fixes as their own commits **before** any of its
  cleanup commits, live findings (see the Prod column) first.
- Cite files by full path from the module root.
- No behaviour change beyond the finding, except where the finding is a
  sabre quirk explicitly marked for removal.

## Phase 1 — security and data loss (land first, own commits)

**Status: S1–S10 done** (branch `review-fixes`). S2 grew while fixing it:
`carddav.Card` had no ACL, so the sabre default (`{DAV:}all` for
authenticated) let any user GET/PUT another user's contacts. Cards now carry
the owner ACL and the default ACL is `{DAV:}read`. S7 changed
`auth.Backend.Challenge` to take the request (for `stale=true`).

Prod column: **live** = reachable with the ansible role's defaults
(`roles/calserver/defaults/main.yml`: rcptd mode, so Basic-only; browser on;
push off; temp-file filter not templated, off by default). **latent** = only
reachable with a non-default config. Fix both, live first.

| # | Prod | Finding | Location | Fix |
|---|------|---------|----------|-----|
| S1 | live | MOVE/COPY with a missing source skips the destination ACL check, then deletes the destination before noticing; MOVE also skips `BeforeUnbindHook` on the overwritten destination (COPY runs it) | `internal/acl/privileges.go:414`, `internal/dav/core.go:714-770` | Check destination bind/unbind regardless of source existence; resolve the source in `copyMoveInfo` before any delete (404 first); run `BeforeUnbindHook` for the destination in MOVE too. |
| S2 | live | Production root is a `mock.Collection`: no ACL of its own, so `defaultACL` (`{DAV:}all` for authenticated) applies; writable, unlocked `children` | `internal/server/server.go:386`, `internal/dav/mock/mock.go`, `internal/acl/privileges.go:80` | New read-only root type in `dav` (403 on create/delete/rename); `mock` stays test-only. |
| S3 | live | Every sharee gets `{DAV:}share` because storage never fills `ShareAccess` (only `corpus/seed.go` sets it) | `internal/caldav/nodes.go:521`, `internal/storage/postgres.go:107-170` | Populate `ShareAccess` from `access`; grant share only for owner / not-shared using the `Access*` constants. |
| S4 | live | `GET ?export` bypasses CONFIDENTIAL/PRIVATE redaction (`exportBlobs` returns raw `CalendarData`) | `internal/caldav/export.go:139,170` | Serve through the same `viewData` path (`RedactRestricted` when read-only). |
| S5 | latent | Push-resource SSRF (any URL, redirects followed, 404/410 oracle) | `internal/push/xml.go:35`, `internal/push/plugin.go:147`, `internal/push/sender.go` | Require https; reject loopback/private/link-local after resolve (also at dial time via a custom `DialContext` on `webpush.Options.HTTPClient`); no redirects. |
| S6 | latent | Temp-file filter bypasses ACL, unbounded | `internal/dav/tempfilefilter.go:68` | Key by principal + path, check bind on the parent, cap total size and count. |
| S7 | latent | Digest: `uri` not checked, nonce never verified → replay | `internal/auth/digest.go:56-86` | Compare `uri` with `r.URL.RequestURI()`; HMAC-signed timestamped nonces with a short expiry (stale=true on expiry); track the highest `nc` per nonce in memory and reject non-increasing values, since a signed nonce alone still replays until it expires. |
| S8 | live | `%2F` inside a segment passes the traversal guard | `internal/dav/core.go:963` | Reject decoded segments containing `/`. |
| S9 | live | Browser listing ignores `ChildFilterHook` (cross-domain principal leak); hrefs are HTML-escaped but not URL-path-encoded, so names with `?`/`#` break | `internal/dav/browser.go:32` | One `Server` method for the filtered child listing, used by PROPFIND and browser; path-encode hrefs (`encodePath`) before HTML-escaping. |
| S10 | live | Read-only sharee can raise their own access via `cs:share` | `internal/caldav/sharing.go:219` | The privilege check there is correct; the hole is S3. Covered by S3; add an explicit test. |

## Phase 1 — workstreams (parallel)

### A. `internal/dav`, `dav/xml`, `dav/mock`, `acl`, `pathutil`
Owns S1, S2 (incl. the one-line root swap in `server/server.go`), S6, S8, S9, plus:
- `acl/proxy.go:150` regex href parsing → `encoding/xml` + `Server.ResolveHref`;
  same for `acl/plugin.go:439,559,562`.
- `acl/plugin.go:87` `ACLForNode` → return `nodeACL(node)`.
- Remove the `Bare` multistatus quirk (`xml/multistatus.go`, `acl/plugin.go:582,589`).
- Preconditions: If-None-Match 304 for HEAD; If-(Un)Modified-Since only where
  RFC 7232 applies, no 404 for not-yet-existing targets.
- `writePropStatusResponse`: deterministic order; share one propstat helper with PROPPATCH.
- One `Server.WriteMultiStatus`; one XML escaper (`xml.EscapeText`); no sprintf XML in `WriteError`, supported-method-set, principal-search-property-set.
- GET via `http.ServeContent` when the body is seekable.
- Dead code: `splitPath` → `pathutil.Split`; drop the `Path` wrapper if trivial (keep `Server.ResolveHref`, which the href-parsing bullet above relies on), the `xmlname` alias, `PropPatch.String`; move `xml/canonical.go` to a test helper. Dropping `Path` touches callers in `internal/caldav` (B), `internal/carddav` and `internal/push` (F): A makes those one-line call-site edits, last, after B and F have landed their S-fixes.

### B. `internal/caldav` (non-ical)
Owns S3 (incl. the `ShareAccess` fill in `storage/postgres.go`), S4, S10, plus:
- `plugin_mkcalendar.go`: replace the `innerxml` parser with `davxml`'s namespace-aware prop parsing.
- `imip.go:649`: build headers with `mail.Address` + `mime.QEncoding` (as `share_notify.go` does); add `Date` and `Message-ID`.
- `sharing.go:241`: send notifications from the final `changes`; handle `AddNotification` errors.
- `sharing.go:331`: drop `X-Test-Now`; inject a clock.
- One shared `principalForAddress` backed by an indexed storage lookup. B writes the storage method in a new file under `internal/storage`; any index it needs goes into `scripts/schema/pgsql.calserver.sql`, which D owns, so B asks D for it.
- UID lookup / conflict check / free-busy: use `CalendarQuery` / a UID index instead of loading every object; drop `existingPath`. Same schema rule as above.
- `export.go`: hoist timezone and filter out of the loop.
- `writeback_lock.go`: bounded lock wait; discard the connection when unlock fails.
- `schedule_freebusy.go:261`: serialize via go-ical, not `formatICalProp`.
- Replace the hand-rolled XML escapers / string-built XML with `encoding/xml`.
- `submission.go`: delete `plainAuth`, use `smtp.PlainAuth`.
- `plugin_reports.go`: per-href timezone in multigets; 404 entries for unresolvable hrefs; `errors.As`.
- `readBody` → `io.ReadAll`; sharing POST only reads the body for sharing roots; fix the stale doc comment in `schedule.go:359`; lock the mock backend consistently.

### C. `internal/caldav/ical`
- Outlook TZIDs: map to IANA and keep the wall clock (reuse `availability.go`'s `ruleComponent`/`place`) instead of flattening to UTC; test recurring + EXDATE + override.
- `expand.go:148`: count only in-window instances; `denormalize.go:184`: past the cap, fall back to UNTIL / maxDate.
- `itip.go:136`: STATUS from the master only.
- REPLY application: key instances by time, create the missing override, never copy master PARTSTAT onto overrides; drop the `"master"` sentinel.
- EXDATE REPLY / `addExdate`: keep TZID and VALUE=DATE; merge into an existing EXDATE using its own params.
- All-day end: `AddDate` instead of `Add(24h)`.
- Free-busy: honour overrides (moved / cancelled / transparent); resolve non-IANA TZIDs.
- Recurrence detection: handle RDATE-only events.
- VTODO alarms: use DUE for RELATED=END; work without DTSTART.
- Fold lines on rune boundaries; RFC 6868 parameter encoding.
- jCal: follow RFC 7265 in both directions (multi-value, cal-address, utc-offset, period, typed recur parts, `until` conversion).
- Dedup `eventEnd`/`nonRecurringEnd`, `AlarmInTimeRange`, `setUTCDateTime`/`setDateValue`, the override loops, `cloneComponent`.
- `sequence` as an int; trim `attendeeInfo`; `slices.Contains` / `slices.SortFunc`; hoist the `vtzgen` fingerprint behind the cache check.

### D. `internal/storage`, `pgdial`
- `adminwrites.go:363` PurgeExpired: claim the tombstone inside the cascade transaction.
- `objects.go:46`, `adminqueries.go:486`: clamp to the int4 range (or cast to `bigint`).
- Change rows: one synctoken per row (`DeleteAllCards`, `diffCards` batch), so a `limit` never splits a token.
- Check `RowsAffected` on update/delete; return not-found and write no change row.
- `contactimport.go:247`: `FOR UPDATE` on the first synctoken read.
- Doctor / admin scans: `LIMIT cap+1` in SQL.
- `DeleteUserCascade`: also delete `notifications`.
- `HashA1` → `(string, error)` (`internal/storage/users.go`); D also updates the `auth.DigestBackend.HashA1` field type (F's file) and the wiring in `internal/server/server.go:439` (E's file), in the same commit; `flagStaleIMipPending` checks errors and `rows.Err()`, ignoring only 42P01.
- Merge `sync.go` / `carddav.go` change tracking into one `changesSince` / `recordChange` (`UPDATE … RETURNING`).
- `PrincipalsByPrefix`: filter in SQL.
- Native pgx arrays instead of `array_to_json` / hand-built literals.
- `moveInTx` / `DeleteOrphans`: single set-based statements, in a transaction.
- NULL etag: `coalesce(etag, md5(data))` in the column lists; `QueryRow` + `ErrNoRows` for single-row reads.

### E. `cmd/*`, `admin`, `server`, `config`, `health`, `metrics`, `logging`, `corpus`
(Does not touch shutdown in `cmd/calendsd/main.go`, which belongs to F.)
- `cmd/calclient/main.go:107`: `http.Server` with `ReadHeaderTimeout`/`IdleTimeout`.
- `internal/health/health.go:96`: set the TLS `ServerName` for the local probe.
- `admin/auth.go:53`: lazy `dummyHash` (`sync.OnceValue`).
- calctl global flags via a `flag.FlagSet`; export `config.FindDefaultConfig`.
- `cmd/calctl/jobs.go:313` `strconv.ParseInt`; `cmd/calctl/jobs.go:289` name the organizer; `truncate` by rune.
- `server.go:471`: config error instead of a panic for push keys.
- Export `admin.CheckCredentials` / `WellFormedDigest` / principal rows and reuse them in calctl.
- calctl helpers: `fatalf`, `printJSON`, `dbFlag`, one `dash`.
- `server.go`: `compose` returns `(srv, backend)`; drop `newDAVServer`, `New`, `NewWithBackground`.
- One `normalizeBase`; `const DefaultRealm = "SabreDAV"` — the value must not change: `users.digesta1` bakes in the realm and Basic verifies against it too (`internal/server/server.go:36,432-436`), so a new realm breaks every login.
- `config`: track consumed keys in the accessors instead of the `known` map; handle escapes in the TOML parser (or switch to a real TOML library).
- `cmd/calctl/contacts.go:456`: typed JSON summary; check NDJSON write/close errors.
- Minor: one status-recorder ResponseWriter with `Unwrap()`; `logging.ParseLevel` → `slog.Level.UnmarshalText`; `ndjsonScanner` uses `bytes.TrimSpace(s.Bytes())`; admin `propfind` reuses `davRequest`; `propPatch`/`provision` reuse `writeGate`.

### F. `carddav`, `contactsapi`, `auth`, `jobs`, `push` (+ shutdown in `cmd/calendsd/main.go`)
Owns S5, S7, plus:
- Graceful shutdown: `main` waits for `Shutdown` to finish; `jobs.Queue` gets `Wait()` (WaitGroup); also wait for the push worker.
- Job settle: `WHERE id=$1 AND state='executing' AND attempt=$2`; refuse to requeue `executing`.
- Push: HTTP client timeout / per-send context; `reconcile` after every LISTEN and on the housekeeping tick; retry 429/5xx; join the reader goroutine before `conn.Close`; `sync.OnceValues` for `privilegeChecker`.
- CardDAV PUT: one shared validator (single card, UID required, size cap) used by both DAV and the contacts API.
- `auth.Check` → `(string, error)` with `ErrUnavailable` / `ErrBadCredentials`; no DB error text in the 401 body; update all callers.
- Jobs: shared enqueue helpers; one `reconcileSibling`; no iMIP-specific `ON CONFLICT` in the generic `enqueue`; `len(cfg.Backoff)==0`; `clear`; `errors.Is`; `errors.New`.
- CardDAV query: validate `test` (400 on unknown values); drop the always-nil error from `match`.
- Cleanups: one `prefix()` / `Challenge`; drop `push/xml.go`'s escaper; fix the "drain" comment. (The dead `QuotedETag` fallback depends on D's coalesce, so it moves to phase 2.)

Cross-workstream touch points (keep edits minimal and local):
- `internal/server/server.go`: A swaps the root; D updates the `HashA1` wiring; E owns the rest.
- `internal/storage/postgres.go`: B fills `ShareAccess`; D owns the rest.
- `internal/storage/` new file: B adds the address/UID lookup; schema changes go through D.
- `internal/auth/digest.go`: D changes the `HashA1` field type; F owns the rest.
- `cmd/calendsd/main.go`: F owns shutdown; E owns the rest.
- `auth.Check` callers outside F's packages are updated by F.
- `Server.Path` callers in B's and F's packages are updated by A, last.

**Status: workstreams A–F merged** into `review-fixes`; their leftovers were
done in phase 2 except the two kept below.

## Phase 2 — cross-cutting refactors (sequential, after phase 1 is green)

**Status: done.** Notes against the original items:
- `ctx` also had to go through the `dav` node and tree interfaces (`Child`,
  `Children`, `Get`, `Put`, `Delete`, `Create*`, `Props`, `PropPatch`,
  `Changes`) and the hooks that reach storage; without that the nodes had no
  request context to pass on. A locked-row test pins the contacts API write
  timeout.
- Hook dispatch uses a range-over-func iterator, `dav.Hooks[T]`, rather than a
  callback `eachHook`.
- The `QuotedETag` fallback is not dead: the in-memory backends, fixtures and
  the admin panel's derived-etag check rely on it. Kept, documented.

1. **`context.Context` through storage.** `ctx` is the first parameter of every
   CalDAV / CardDAV / Principals / PropertyStorage / Users method; `withTx`
   takes `ctx`; delete the `XxxContext` wrapper pairs; thread the request ctx
   from handlers (caldav, carddav, contactsapi, admin, calctl). This makes
   contacts API writes honour `RequestTimeout`.
2. **Hook dispatch.** Replace the per-hook type-assertion loops with one
   generic `eachHook[T]`; call the single-implementer hooks the same way, but
   log the errors from `fireAfterUnbind` / `fireAfterMove` instead of dropping them.
3. **Dead `QuotedETag` fallback** (`internal/carddav/backend.go:35`), now that
   D's `coalesce(etag, md5(data))` guarantees an etag.

## Verification

- `go vet ./...`, `go test ./...` after each phase. (`./...` also matches
  `cmd/calclient/node_modules/flatted/golang/pkg/flatted`; a failure there is
  not ours.)
- `make corpus`, plus `make caldavtester` against the dev DB, before the final commit.
- Manual checks:
  - S1: MOVE / COPY onto another user's calendar → 403, also with a missing source (and the destination survives).
  - S2: `PUT /dav/x` → 403.
  - S3/S10: a read-only sharee's `cs:share` → 403.
  - S4: a read-only sharee's `GET ?export` of a calendar with a CONFIDENTIAL event → redacted.
  - S8: a path with `a%2F..%2Fb` in a segment → 403.
  - S9: browser listing of `/principals/` shows only same-domain principals; a child named `a?b` links correctly.

## Out of scope / deliberately kept

- The 5-minute positive credential cache, the fail-open tombstone check and the
  per-keystroke card scan, which are documented trade-offs.
- `internal/corpus` stays (test fixture code); only its unused exports are trimmed.
- `jobs.enqueue`'s iMIP `ON CONFLICT`: making it opt-in needs a migration of
  the `jobs_coalesce_ix` partial index for no behaviour change; rows without
  the iMIP keys never collide.
- `Server.Path` stays: it pairs `EscapedPath` with base stripping and
  decoding for ~30 callers.
