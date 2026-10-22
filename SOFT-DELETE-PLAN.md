# Plan: Account Soft-Delete with Retention

## 0. Purpose

Make account deletion reversible for a configurable grace period. Today
deletion is immediate and irreversible: `calctl principal remove` (guarded)
and `--cascade` (hard) both drop rows the moment they run. This adds a
*schedule now, purge later* path so an accidental or disputed deletion can be
undone within N days, and a compliance "hold then wipe" window is expressible.

The mechanism composes with what already exists: `AdminWrites.DeleteUserCascade`
(the hard purge from the account-deletion commit) becomes the **purge
primitive**, and soft-delete is a thin scheduling layer on top of it — a
tombstone row, an auth gate that disables the account immediately, and a
periodic purge job that calls the cascade once the window expires.

**Scope: login-only disable.** A soft-deleted account cannot authenticate, so
the user loses all access instantly, but its data stays in the DB until purge.
Full invisibility (hiding the principal from other users' shares, free-busy,
and scheduling delivery) is a **non-goal** here — see §10.

## 1. Hard constraints

- **No sabre-schema divergence.** The new state lives in a table calendsd
  owns (`scripts/schema/pgsql.calserver.sql`), not in new columns on `users`
  or `principals`. Those tables stay byte-for-byte sabre-compatible, matching
  the `AdminWrites` carve-out rationale.
- **Covers both auth backends.** The disable is enforced in `auth.Plugin`,
  *above* the backends, so it applies equally to the digest/basic path (against
  `users.digesta1`) and the rcptd/Dovecot path — neither of which would notice
  a `users`-column flag.
- **Reversible with zero data touch.** Soft-delete inserts one row and touches
  no calendar/card/principal data. Restore deletes that row. The `digesta1`
  hash is never destroyed, so the account works again immediately on restore.
- **Purge stays atomic and reuses the cascade.** Purge is exactly
  `DeleteUserCascade` + tombstone cleanup; no second copy of the delete SQL.
- **Nil-safe.** A server with no retention wiring (tests, `calclient` users of
  internal packages) must behave exactly as today — a nil auth gate is a no-op.
- **No new dependency.** Postgres computes the window (`now() + interval`) and
  the expiry check (`purge_after <= now()`); no clock plumbing in Go.

## 2. Data model — the tombstone table

Add to `scripts/schema/pgsql.calserver.sql` (additive extension schema, loaded
after sabre core; no existing table altered):

```sql
CREATE TABLE deleted_principals (
    principal    VARCHAR(200) PRIMARY KEY,   -- "principals/alice"
    username     VARCHAR(50)  NOT NULL,       -- bare username (for the purge cascade)
    deleted_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    purge_after  TIMESTAMPTZ  NOT NULL,       -- deleted_at + retention window
    reason       TEXT
);
CREATE INDEX deleted_principals_purge_after_ix ON deleted_principals (purge_after);
```

Rationale:
- Keyed by `principal` so the auth gate is a single PK lookup.
- `username` is stored so the purge job can call `DeleteUserCascade(username)`
  without re-deriving it (and it survives even if someone renames things).
- `purge_after` is materialized at schedule time (not computed from
  `deleted_at + config` at read time) so changing the config later does **not**
  retroactively move already-scheduled purges — the window a user was promised
  is the window they get.
- No FK to `principals`: the row must outlive nothing, but must also not block
  if the principal is manually removed; the purge job tolerates a missing
  principal (cascade reports `ErrNoPrincipal` — see §4 — which the job treats
  as done and still clears the stale tombstone).

## 3. Config — retention period

Add a `[retention]` section. Files:

- `internal/config/config.go` (struct at lines 16-62): add
  ```go
  Retention struct {
      DeletedDays int
  }
  ```
  and parse `retention.deleted_days` in the TOML subset parser (lines 125-224).
- `config.toml.example`: document
  ```toml
  [retention]
  # Days a soft-deleted account is kept before permanent purge. 0 = use the
  # built-in default (30). Only applies when scheduling a deletion; already
  # scheduled purges keep the window they were given.
  deleted_days = 30
  ```
- `internal/server/server.go` `Config` (lines 27-75): add `RetentionDays int`
  with a doc comment. Only the surfaces that *schedule* a deletion read it; the
  auth gate and purge job do not need it.
- `internal/config/shared.go` `resolve()` (lines 52-113): map
  `cfg.Retention.DeletedDays` → `Server.RetentionDays`, applying the default:
  `if n <= 0 { n = 30 }`. Expose one helper `retentionDays(cfg) int` so
  calendsd-admin and calctl share the same defaulting.

`calctl` reads `globalConfig.Retention.DeletedDays` directly (it already holds
a `config.Config`); the admin app reads `App.Config.RetentionDays`.

## 4. Storage layer — `AdminWrites` (internal/storage/adminwrites.go)

New methods next to `DeleteUser`/`DeleteUserCascade`, plus one small type:

```go
type DeletedPrincipal struct {
    Principal, Username, Reason string
    DeletedAt, PurgeAfter       time.Time
}

// ScheduleDelete tombstones a live account: it verifies a users row (or at
// least a principal) exists, then inserts a deleted_principals row with
// purge_after = now() + retainDays. Re-scheduling an already-tombstoned
// account updates its window and reason. Touches no calendar/card data.
func (w *AdminWrites) ScheduleDelete(username string, retainDays int, reason string) error

// CancelDelete removes the tombstone (restore). Errors if none exists.
func (w *AdminWrites) CancelDelete(username string) error

// ExpiredDeletions lists tombstones whose purge_after <= now().
func (w *AdminWrites) ExpiredDeletions() ([]DeletedPrincipal, error)

// PurgeExpired runs the retention sweep. For each expired tombstone it
// **claims the row first** — `DELETE FROM deleted_principals WHERE principal =
// $1 AND purge_after <= now() RETURNING username` — and only cascades if that
// delete returned a row. If it returned none, a concurrent CancelDelete
// (restore) won the race, so the account is skipped and its data left intact.
// Then it calls DeleteUserCascade. A cascade ErrNoPrincipal (already gone) is
// treated as success; the tombstone is already cleared by the claim. Returns
// what it purged.
//
// The claim-before-cascade order is the invariant that closes the
// purge/restore TOCTOU window: ExpiredDeletions may be stale by the time the
// sweep acts, so ownership of each account is decided by the claiming DELETE,
// not by the earlier read.
func (w *AdminWrites) PurgeExpired() ([]DeletedPrincipal, error)

// Deletions lists all tombstones (for `principal list` / admin display).
func (w *AdminWrites) Deletions() ([]DeletedPrincipal, error)
```

And a read helper for the auth gate — cheap, on the query side:

```go
// IsDeleted reports whether a principal has a pending-deletion tombstone.
// Single PK lookup; safe to call per authenticated request.
func (u *Users) IsDeleted(principal string) (bool, error)
```

`ScheduleDelete` validation mirrors `DeleteUser`'s username guard
(`strings.ContainsAny(username, "/\n")`). `INSERT ... ON CONFLICT (principal)
DO UPDATE SET purge_after = ..., reason = ...` keeps it idempotent.

**Sentinel error for the missing-principal case.** `PurgeExpired`'s "already
gone → success" branch must not string-match. Today `deletePrincipalRows`
signals a missing principal with `fmt.Errorf("no principal %q", principal)` (a
formatted string, no sentinel — adminwrites.go:218). Add
`var ErrNoPrincipal = errors.New("no principal")` in `adminwrites.go`, wrap it
(`fmt.Errorf("no principal %q: %w", principal, ErrNoPrincipal)`), and have the
sweep test `errors.Is(err, ErrNoPrincipal)`. This is a prerequisite for the §2
"tolerates a missing principal" behavior; both `DeleteUser` and
`DeleteUserCascade` inherit the wrap for free.

Edge to note: `deletePrincipalRows` deletes the `principals` rows and returns
`ErrNoPrincipal` *before* deleting the `users` row, so a tombstone whose
principal was manually removed while its `users` row survived would purge as
"done" yet leak the `users` row. Rare (manual mutation only); the doctor's
sweep flags it. Acceptable for v1, but the purge log line should record it.

## 5. Auth gate — immediate disable (internal/auth/auth.go)

Add one nil-safe callback to `Plugin` (lines 29-53):

```go
// Tombstoned reports whether an otherwise-valid principal is scheduled for
// deletion; such requests are rejected with 403 even though the credentials
// are correct. Nil disables the check (parity with the pre-soft-delete server).
Tombstoned func(principal string) bool
```

Gate insertion in `BeforeMethod` (lines 89-97), immediately after a backend
`Check()` succeeds and **before** `SetPrincipal`:

```go
if ok {
    if p.Tombstoned != nil && p.Tombstoned(principal) {
        p.logger().Warn("auth blocked: account scheduled for deletion",
            "principal", principal, "method", r.Method, "remote", remoteAddr(r))
        p.Metrics.AuthAttempt("tombstoned", "blocked") // nil Set is a no-op
        // valid creds, but the account is disabled → 403, not a 401 re-challenge
        return false, dav.ErrForbidden("this account is scheduled for deletion")
    }
    dav.SetPrincipal(r.Context(), principal)
    ...
}
```

- Return **403**, not 401: the credentials are valid, so re-challenging would
  loop. 403 tells the client to stop. `dav.ErrForbidden` **already exists**
  (`internal/dav/errors.go:44`, → `StatusForbidden`) — use it directly; no new
  error needed.
- **Metric for parity.** This codebase instruments auth outcomes heavily
  (`Plugin.Metrics *metrics.Set`, `AuthAttempt(backend, outcome)`); a blocked
  login should be counted, not only logged. A nil `Metrics` records nothing, so
  it stays nil-safe.
- Applies to `fixedPrincipal`? No — that path (`NewLoggedInPlugin`) returns
  early at the top of `BeforeMethod`, test/loopback only; leave it ungated.
- **Stateless auth ⇒ genuinely immediate.** DAV re-authenticates every request
  (no sessions), so a tombstone takes effect on the very next request with no
  session-invalidation work — this is why login-only disable is instant.

Wiring in `internal/server/server.go` `compose()` (lines 141-199). Note there
are **three** auth branches, not two — `authOverride`, `rcptd`, and the
`default` digest/basic path. Gate the `rcptd` and `default` branches by
reusing the `users := &storage.Users{DB: db}` already in scope:
`ap.Tombstoned = func(principal string) bool { ok, _ := users.IsDeleted(principal); return ok }`.
**Leave the `authOverride` branch ungated on purpose** — it's the injected-plugin
/ test path (same rationale as `fixedPrincipal`); state this explicitly so it
doesn't read as an oversight.
On query error, fail **open** for availability (log it) — a DB blip must not
lock every user out; the purge job is the backstop. The `ok, _ := ...` discard
already yields `false` (fail open) on error. (§12 records this decision.)

Cost: one indexed PK lookup per authenticated request, on a tiny table. Auth
already hits the DB (`HashA1`). If this ever matters, add a short-TTL cache
later — not in v1.

## 6. calctl surface (cmd/calctl/principal.go)

New verbs in the `handlePrincipal` dispatch (lines 23-44), keeping the existing
`remove` as the **immediate hard delete** it is today:

- `principal delete <user> [--in <days>] [--reason <text>] --confirm`
  Soft-delete: schedule. `--in` defaults to `globalConfig.Retention.DeletedDays`
  (then the built-in 30). Prints the computed purge date.
- `principal restore <user>` — cancel a pending deletion (`CancelDelete`).
- `principal purge-expired [--dry-run]` — run the retention sweep
  (`ExpiredDeletions` for `--dry-run`, else `PurgeExpired`); prints each account
  purged. **This is the command cron/systemd runs** (§8).
- `principal remove <user> --confirm [--cascade]` — unchanged; the escape hatch
  for immediate, non-retained deletion.

Also extend `principal list` (lines 134-185) to show a `DELETION` column
(`purge_after` when tombstoned, `-` otherwise), sourced from `Deletions()`.

All use the existing `openAdminQueries(dsn).DB` → `&storage.AdminWrites{DB: db}`
pattern and `takeUsername`/`--confirm` conventions.

**Naming decision to confirm:** `delete` (soft) vs `remove` (hard) reads
slightly subtle. Alternative: fold into `remove` with `--in <days>` for soft
and `--now` for hard. I recommend the separate verbs (clearer, and `remove`
keeps its current meaning), but flag it for sign-off.

## 7. Admin HTTP API + panel

- **API** (`internal/admin/writes.go`): the `Writes` interface gains
  `ScheduleDelete`, `CancelDelete` (and the app can reuse `DeleteUserCascade`
  already there for the immediate path). New endpoints, both `writeGate`-d
  (CSRF + `-readonly=false`) with an audit line:
  - `POST /api/schedule-delete` — form `username`, optional `days`
    (default `App.Config.RetentionDays`), optional `reason`.
  - `POST /api/restore-user` — form `username`.
  Register in `internal/admin/admin.go` next to `/api/delete-user`.
- **Panel** (`internal/admin/templates/principal.html`): in the danger zone,
  add "Schedule deletion (in N days)" beside the existing immediate "Delete
  user" form; when the principal is already tombstoned, show the purge date and
  a "Restore" button instead. Data comes from a `Deletions()` lookup added to
  the principal-page handler.
- Update the fake in `internal/admin/phasec_test.go` (`fakeWrites`) with the
  new interface methods, same as the delete-user work.

## 8. Purge scheduling (deployment)

No in-process scheduler — the purge is a `calctl` invocation, matching the
FreeBSD + ansible deployment. Add to the ansible role a daily job:

```cron
# /usr/local/etc/cron.d/calendsd-purge  (or a periodic(8) script)
17 3 * * *  calendsd  /usr/local/bin/calctl principal purge-expired >> /var/log/calendsd/purge.log 2>&1
```

- Idempotent and safe to run often; only tombstones past `purge_after` act.
- Logs each account purged, and logs an already-gone principal (`ErrNoPrincipal`)
  distinctly — that's the rare manual-removal case where a stale `users` row may
  linger for the doctor to sweep (see §4).
- `--dry-run` for an operator to preview before trusting the timer.
- Document in README/ops notes that disabling the timer means soft-deletes
  never actually purge (data grows) — the timer is required, not optional.

## 9. Testing

- **Storage integration** (`internal/storage/adminqueries_test.go`, the
  Postgres-backed `TestAdminQueries`; extend, as with the cascade work): seed
  the new table via the schema glob (already loads `pgsql.*.sql`; ensure the
  extension file is in the loaded set), then assert:
  - `ScheduleDelete` inserts a tombstone with `purge_after ≈ now()+days`,
    touches no calendar/card rows, and is idempotent (re-schedule updates).
  - `IsDeleted` true for a tombstoned principal, false otherwise.
  - `ExpiredDeletions` returns only rows with `purge_after <= now()` (seed one
    past, one future by backdating `deleted_at`/`purge_after`).
  - `PurgeExpired` cascades exactly the expired accounts (reusing the cascade
    assertions), clears their tombstones, and leaves future ones intact.
  - `PurgeExpired` claim-before-cascade: a tombstone whose `principals` rows
    were removed out from under it purges as success (`errors.Is(err,
    ErrNoPrincipal)`) and still clears the tombstone; and a tombstone deleted
    (restored) before the sweep acts is **not** cascaded — assert the account's
    data survives.
  - `CancelDelete` removes the tombstone and errors when none exists.
- **Auth gate** (`internal/auth/*_test.go`): a request with valid creds but a
  `Tombstoned` callback returning true gets 403 (not 401), records no
  `SetPrincipal`, and counts one `AuthAttempt("tombstoned","blocked")`; nil
  callback and false-returning callback behave exactly as today. Table-driven
  across the digest and basic backends.
- **Admin handlers** (`internal/admin/phasec_test.go`): `/api/schedule-delete`
  and `/api/restore-user` route to the right `Writes` calls, honor `writeGate`,
  default the window from `Config.RetentionDays`, and validate inputs; principal
  page renders the schedule form / restore button.
- **Config** (`internal/config/*_test.go`): `[retention] deleted_days` parses;
  0/absent yields the default via `shared.go`.
- TDD, tests-first, same as the account-deletion commit.

## 10. Non-goals / future — full invisibility

Login-only disable leaves a soft-deleted principal *visible* to others until
purge: existing shares still resolve, free-busy still answers, and inbound
scheduling (iMIP/iTIP) can still target it. For the recover-within-N-days and
hold-then-wipe use cases that's acceptable — the account is inert and its owner
is locked out. If true invisibility is later required, layer it on:

- `acl` principal backend: hide tombstoned principals from principal-collection
  listings and principal-search.
- `caldav` free-busy + scheduling paths: treat tombstoned principals as absent.

These touch more surfaces and DAV semantics; keep them out of v1.

## 11. Files touched (summary)

| Concern | File | Change |
|---|---|---|
| Schema | `scripts/schema/pgsql.calserver.sql` | new `deleted_principals` table + index |
| Config struct/parse | `internal/config/config.go` | `Retention.DeletedDays` + TOML key |
| Config example | `config.toml.example` | `[retention]` section |
| Config → server | `internal/server/server.go` | `Config.RetentionDays` field |
| Config resolve/default | `internal/config/shared.go` | map + default (30) |
| Storage | `internal/storage/adminwrites.go` | `ScheduleDelete`, `CancelDelete`, `ExpiredDeletions`, `PurgeExpired` (claim-before-cascade), `Deletions`, `DeletedPrincipal`; `ErrNoPrincipal` sentinel wrapping the existing "no principal" error |
| Storage (read) | `internal/storage/postgres.go` (`Users`) | `IsDeleted` |
| Auth gate | `internal/auth/auth.go` | `Plugin.Tombstoned` + gate in `BeforeMethod` (403 via existing `dav.ErrForbidden`; `AuthAttempt("tombstoned","blocked")`) |
| Auth wiring | `internal/server/server.go` `compose()` | set `Tombstoned` closure over the in-scope `users` in the `rcptd` + `default` branches; `authOverride` left ungated |
| CLI | `cmd/calctl/principal.go` | `delete`, `restore`, `purge-expired`; `list` column |
| Admin API | `internal/admin/writes.go`, `admin.go` | `schedule-delete`, `restore-user` + interface + routes |
| Admin UI | `internal/admin/templates/principal.html` | schedule/restore forms |
| Deployment | ansible role (out of this repo) | daily `calctl principal purge-expired` |
| Tests | storage / auth / admin / config `*_test.go` | per §9 |

## 12. Decisions (resolved)

1. **CLI naming**: **separate verbs** — new `principal delete` (soft) /
   `restore` / `purge-expired`; `principal remove --confirm [--cascade]` stays
   as the immediate hard delete. (§6)
2. **Auth-gate DB error**: **fail open** — on a tombstone-lookup error, allow
   login and log it; the purge job is the backstop. (§5)
3. **Default retention**: **30 days** when `deleted_days` is 0/unset. (§3)
4. **Scope**: **login-only disable** this pass; full invisibility deferred to
   §10.
