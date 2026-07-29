# Plan: Prometheus Metrics for calserver

## 0. Purpose

Answer "is the service healthy, and if not, where is it degrading" from a
scrape instead of log spelunking: request rates and latency, auth failures,
rcptd availability, Postgres pool saturation, sync behavior, and iMIP
delivery. These map to the ways calserver actually fails in practice:

- rcptd down or slow → every login stalls or errors
- auth failure spikes → brute force or a misconfigured client
- DB pool wait climbing → saturation before user-visible latency
- sync responses truncating constantly → a client re-syncing pathologically
- iMIP sends failing silently → "invitations don't arrive"

**Non-goals**: tracing, per-user/per-calendar breakdowns (cardinality;
that's the admin panel's job), metrics for `calserver-admin` itself (low
traffic, basic-auth'd, logs suffice), pushgateway/remote-write, dashboards
(out of tree — flatmail ansible owns Prometheus/Grafana config).

## 1. Hard constraints

- **Bounded label cardinality.** DAV paths embed principal, calendar, and
  object URIs. Raw paths must never become label values. Everything below
  labels by a *closed* set: DAV verb, normalized route pattern (§4), report
  type, auth outcome, HTTP status class. No label derives from user input.
- **Not on the public listener.** The DAV port is internet-facing; `/metrics`
  leaks operational detail. Metrics get their own listener, default-off,
  documented as loopback/firewalled (same posture as the admin port).
- **One new dependency**: `github.com/prometheus/client_golang` (plus its
  transitive protobuf deps). Everything else stays stdlib, per house style.
- **No behavior changes.** Instrumentation wraps existing code paths;
  a nil/disabled metrics setup must be a no-op so tests and `calclient`
  users of internal packages don't need a registry.
- **Own registry**, not the global default: `prometheus.NewRegistry()` with
  Go runtime + process collectors registered explicitly. Avoids surprise
  double-registration if another package ever pulls in client_golang.

## 2. Architecture

New package **`internal/metrics`**:

- Defines every collector in one file, all named `calserver_*`, registered
  on a `*metrics.Set` struct that owns the registry. One place to audit
  names, labels, and buckets.
- `metrics.New()` returns a populated Set; `(*Set).Handler()` returns the
  `promhttp` handler. A nil `*Set` is valid everywhere — every method
  checks nil and no-ops — so plumbing is optional at all call sites.
- The Set (or the specific instruments) is passed down the same way `db`
  and config are today: explicit arguments through `server.New`, the auth
  plugin constructor, and the storage backends. No globals.

**Listener** in `cmd/calserver/main.go`: when `server.metrics_addr` is
set, start a second plain-HTTP `http.Server` on that address serving
`GET /metrics` plus `/debug/pprof/*` (net/http/pprof — same privacy
posture, zero cardinality, and it answers the CPU questions a histogram
can't). Default: empty string = disabled. Key mirrors the existing
`server.addr` / `-addr` pair (`-metrics-addr` flag); it must also be added
to the strict-keys allowlist in `internal/config/config.go:106` or config
loading rejects it. The main listener blocks in `ListenAndServe` today, so
the metrics server runs in a goroutine — and **exits the process on bind
failure** rather than log-and-continue: a monitoring endpoint that
silently fails to come up is the one failure monitoring must not have
(the reqlog "log and disable" posture is for debug aids, not this). No
TLS, no auth on this listener — the deployment contract is "bind loopback
or a firewalled interface", stated in the example config comment.

## 3. HTTP middleware

Hook point: `server.New` (`internal/server/server.go:102-108`). Add a
`WrapMetrics(next, set)` sibling of `WrapRequestLog`, wrapped **outside**
`WrapWellKnown` so redirects are counted (matching the route table's
`well-known` row); order relative to `WrapRequestLog` is immaterial
(reqlog is a debug aid, off in production). It needs its own small status
recorder — reqlog's `responseRecorder` only exists when reqlog is enabled.

Captured per request: normalized route (§4), method, status, duration,
in-flight count. Method label is a closed DAV verb set: the core methods
from `internal/dav/core.go:37-45` (OPTIONS, GET, HEAD, DELETE, PROPFIND,
PUT, PROPPATCH, COPY, MOVE, REPORT, MKCOL), plus MKCALENDAR (the caldav
plugin's `Methods()`) and **POST** — add-member (RFC 5995) and the
scheduling outbox (free-busy, iTIP) arrive as POST, some of the most
interesting traffic on the server — plus `other` for anything else. Never
the raw request method string.

## 4. Route normalization

A small classifier in `internal/metrics` (pure function, unit-tested) maps
request paths to a fixed pattern set:

```
/                                → root
/.well-known/*                   → well-known
/principals/{user}               → principal
/calendars/{user}                → calendar-home
/calendars/{user}/{cal}          → calendar
/calendars/{user}/{cal}/{obj}    → calendar-object
/addressbooks/{user}             → addressbook-home
/addressbooks/{user}/{book}      → addressbook
/addressbooks/{user}/{book}/{obj}→ addressbook-object
(anything else)                  → other
```

Classification is by path-segment count under the known roots — the same
structure the DAV tree already implies — not regexes over user data. The
`;add-member` suffix (RFC 5995, `internal/caldav/plugin.go:145`) folds into
its collection's pattern. Proxy principals
(`/principals/{user}/calendar-proxy-read|write`, exposed because
`server.go:146` sets `Proxies = true`) are depth 3 under principals and
classify as `other` — deliberate, and pinned in the classifier's test
table so it stays a decision rather than an accident.

REPORT is a catch-all verb, so additionally count by report type at the
dispatch in `internal/dav/core.go:93-98`: `calendar-query`,
`calendar-multiget`, `free-busy-query`, `sync-collection`,
`addressbook-query`, `addressbook-multiget`, `other`. This is the "what are
clients actually doing" metric.

## 5. Metric inventory

### HTTP (internal/server middleware)

| Metric | Type | Labels |
|---|---|---|
| `calserver_http_requests_total` | counter | `method`, `route`, `code` |
| `calserver_http_request_duration_seconds` | histogram | `method`, `route` |
| `calserver_http_requests_in_flight` | gauge | — |
| `calserver_report_requests_total` | counter | `type` |

Duration buckets: `{0.005, 0.025, 0.1, 0.25, 1, 2.5, 10, 30}` — sync of a
large calendar legitimately takes seconds; default buckets top out too low.
`code` is the numeric status (bounded in practice; collapse to class only
if series count ever matters).

### Auth (internal/auth)

| Metric | Type | Labels |
|---|---|---|
| `calserver_auth_attempts_total` | counter | `backend` (basic/digest/rcptd), `outcome` |
| `calserver_auth_check_duration_seconds` | histogram | `backend` |
| `calserver_rcptd_requests_total` | counter | `code` (200/404/401/503/other/error) |
| `calserver_rcptd_request_duration_seconds` | histogram | — |
| `calserver_auth_cache_events_total` | counter | `result` (hit/miss) |

`outcome` ∈ {ok, bad_credentials, unavailable, no_credentials,
provision_error, error} — **typed at the source, not parsed from reason
strings** (those are human-facing and will be reworded). An Authorization
header neither backend can parse (Bearer, mangled base64) counts as
{backend="none", outcome="error"} at the plugin — a rejection must never
vanish from the series or read as a credential-less probe. Each backend
increments its own counter (the backend gains an optional `*metrics.Set`),
so classification lives next to the code that knows the answer:
`RcptdBackend.verify`'s status switch, and the fail-closed provisioning
branch at `rcptd.go:110-114` → `provision_error` — a first-login DB-write
failure is operationally nothing like `bad_credentials` and must not be
lumped with it. In the digest+basic default stack a failed attempt
produces reasons from *both* backends; the `backend` label follows the
credential type the client actually presented (`attemptedLogin` at
`auth.go:122` already distinguishes Basic vs Digest headers), not the
backend loop.

`auth_check_duration_seconds` wraps the whole `verify()` path. The plan's
own premise is "rcptd down *or slow* → every login stalls", but the rcptd
roundtrip (instrumented at `rcptd.go:179-188`) is only half the cache-miss
cost — the local bcrypt verify in `passwd.VerifyAny` (`rcptd.go:152`) is
often the dominant term by design. Roundtrip vs. check duration
disambiguates "rcptd is slow" from "bcrypt is eating the CPU". Cache
hit/miss counted where the HMAC cache is consulted (`rcptd.go:129`,
`cacheHit` at `rcptd.go:207-219`). This complements the authlog work: logs
answer *who*, metrics answer *how much and when*.

### Storage (internal/storage)

| Metric | Type | Labels |
|---|---|---|
| `collectors.NewDBStatsCollector(db, "calserver")` | gauges | — |
| `calserver_db_op_duration_seconds` | histogram | `op` |
| `calserver_sync_responses_truncated_total` | counter | `resource` (calendar/addressbook) |

The DBStats collector is free and gives pool in-use/idle/wait — wait
duration rising is the earliest saturation signal. `op` is a closed set of
the *named expensive operations only*, not every query: `calendar_query`
(postgres.go:342), `changes_for_calendar` (postgres.go:507),
`create_object`, `update_object`, `scheduling_objects`,
`changes_for_addressbook` (carddav.go:250).

Truncation counter hooks the single choke point in
`internal/dav/sync.go:211`, where `SyncPlugin` consumes
`changes.Truncated` for **both** backends — one instrument instead of two
storage-level hooks (postgres.go:545 and carddav.go:318), with the
resource type free from context.

Deliberately skipped: timing the `SELECT FOR UPDATE` synctoken lock
(postgres.go:566) — `db_op_duration_seconds{op="create_object"|"update_object"}`
brackets the whole transaction including that lock, and pool wait covers
contention too; add a dedicated timer only if it shows up. Also skipped: a
denormalize/recurrence-expansion histogram — that's profiling data, not
monitoring (nothing to alert on, no action from a scrape); the write-path
cost already lands in `create_object`/`update_object` durations and PUT
route latency, and the CPU question is what `/debug/pprof` on the metrics
listener (§2) is for.

### Scheduling / iMIP (internal/caldav)

| Metric | Type | Labels |
|---|---|---|
| `calserver_imip_deliveries_total` | counter | `outcome` |
| `calserver_imip_delivery_duration_seconds` | histogram | — |

Around `DeliverExternal()` in `internal/caldav/imip.go:47-103`. `outcome`
∈ {sent, invalid_recipient, no_mailer, smtp_error, skipped, unsupported},
mapping the schedule-status branches directly (1.1, 5.3, 5.2, 5.1, the
not-significant early return, and the non-mailto sender/recipient abort —
every exit of the function is counted, so no delivery can vanish). A flat ok/error split would make the §8
alert page on a user inviting a typo'd address — only `smtp_error` is
alert-worthy; `invalid_recipient` is user data, `no_mailer` is a
misconfiguration you'd catch at deploy. Duration histogram observes the
`Mailer.Send` call only (the SMTP roundtrip is the variable part).

### Runtime

Go collector + process collector on the registry. Free heap, GC, goroutine,
FD visibility.

`calserver_build_info{version="..."}` gauge (constant 1) — one series, and
it's how a latency regression gets correlated with a deploy on a dashboard.
Version stamped via `-ldflags "-X main.version=..."`; `dev` when unset.

## 6. Implementation order

1. **`internal/metrics` package** — Set, registry, all instruments, route
   classifier + tests. No wiring yet; `go vet`/tests green.
2. **Listener + config** — `server.metrics_addr` key in `internal/config`
   (flag `-metrics-addr`, strict-keys allowlist entry), second server in a
   goroutine in `cmd/calserver/main.go` serving `/metrics` +
   `/debug/pprof/*`, exit on bind failure, `config.toml.example` entry
   with the firewalling note, `build_info` version stamp. Scrape works,
   shows only runtime metrics.
3. **HTTP middleware** — `WrapMetrics` in `internal/server` (outside
   `WrapWellKnown`), REPORT-type counter at the core.go dispatch. This
   alone delivers most of the value.
4. **Auth** — typed outcome counters and check-duration histogram inside
   the backends (each gains an optional `*metrics.Set`), rcptd roundtrip +
   cache instruments.
5. **Storage + scheduling** — DBStats collector, op histograms, truncation
   counter in `SyncPlugin`, iMIP counters.
6. **Docs** — README section: enabling, sample scrape config, the
   loopback-only expectation.

Each step is independently shippable; nil-Set no-op keeps every
intermediate state working.

## 7. Testing

- Route classifier: table-driven unit tests including hostile paths
  (traversal attempts, absurd depth, `;add-member`, proxy principals →
  `other`).
- Middleware: `httptest` request through the wrapped chain, then read the
  registry via `testutil.CollectAndCount` / `CollectAndCompare` to assert
  series and labels; assert no unbounded label leaked.
- Auth/storage instruments: extend the existing package tests to pass a
  real Set and assert counters move on success and failure paths.
- Cardinality guard: a test that scrapes after a spread of synthetic
  requests and asserts total series stays under a fixed budget.

## 8. Alerting sketch (for the ansible side, not this repo)

- `rcptd_requests_total{code!~"200|404"}` rate > 0 for 5m — auth backend sick
- `auth_attempts_total{outcome="bad_credentials"}` sudden rate spike;
  `outcome="provision_error"` rate > 0 — first logins failing
- 5xx ratio and p99 `http_request_duration_seconds` per route
- `go_sql_wait_duration_seconds_total` (DBStats) rate climbing — pool
  saturation
- `imip_deliveries_total{outcome="smtp_error"}` rate > 0 — invitations
  failing (not `invalid_recipient`: that's a user typo, not an incident)
