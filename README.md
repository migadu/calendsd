# calendsd

A Go CalDAV server ported from [sabre/dav](https://sabre.io/), designed to run
**alongside a live sabre-dav instance against the same Postgres database**.
See [PLAN.md](PLAN.md) for the full porting plan; `dav/` is the reference
sabre/dav checkout the port is derived from.

## Dev environment

Everything is provided by the Nix flake; with [direnv](https://direnv.net/)
installed, `direnv allow` once and the toolchain (Go, PHP+pdo_pgsql, composer,
Postgres 16, sqlc) is on PATH.

## Co-existence harness

```sh
scripts/dev-db.sh start          # local Postgres (unix socket under .dev/), schema migrated, dev accounts seeded
php -S 127.0.0.1:8081 scripts/sabre-server.php   # reference sabre instance
go run ./cmd/calendsd -db "postgres:///calserver?host=$PGHOST"  # Go server (default :8080)
```

Credentials: `admin` / `admin` (Basic or Digest, realm `SabreDAV` — the realm
is baked into `users.digesta1`, see PLAN.md §5).

The **live parity test** (the §5 acceptance gate) spins up a fresh database,
a real sabre instance, and the Go server, then cross-reads/writes through
both:

```sh
PARITY=1 go test ./internal/storage/ -run TestParity -v
```

## Admin panel

`calctl panel` serves a debugging UI over the same shared database
(see [ADMIN-PLAN.md](ADMIN-PLAN.md)): principal/calendar/object inspectors, a
privilege simulator that answers "what can user X do on path Y" through the
production plugin chain, a scheduling/sync debugger, a consistency doctor,
and (behind `-readonly=false`) gated setup actions — every page doubles as a
JSON endpoint under `/api/`.

```sh
htpasswd -cB admin-auth.htpasswd root   # bcrypt admin credentials
go run ./cmd/calctl panel -db "postgres:///calserver?host=$PGHOST" \
  -admin-auth admin-auth.htpasswd       # binds 127.0.0.1:8092
```

Run it with the **same config file / Calendar-Server flags as calendsd**
(both processes resolve them through the same code and log an identical
`effective config` line — the dashboard echoes it so drift is a string
diff). Basic auth resends the password on every request: keep the listener
on loopback/LAN or behind TLS. Write actions are off by default, need the
`X-Admin-Request: 1` header, and audit-log every mutation.

## CLI

`calctl` is the management CLI (same shape as `mqctl`/`rcptctl` in the
sibling services). It reads the server's `config.toml` (falling back to
`/etc/calendsd/config.toml`, `/usr/local/etc/calendsd/config.toml`, the
pre-rename `/etc/calserver` and `/usr/local/etc/calserver` paths, then
`./config.toml`) and exposes the admin panel's read paths from the command
line:

```sh
calctl health                 # every dependency, probed concurrently (see below)
calctl panel                  # serve the admin panel (127.0.0.1:8092)
calctl stats                  # dashboard row counts + recent change-log activity
calctl doctor                 # §3.7 consistency checks; exit 1 on any finding
calctl principal list         # principals with email/displayname/login capability
calctl principal add bob --email bob@example.org --password-stdin < pw
calctl principal edit bob --name "Robert"        # --email "" clears; --password resets
calctl principal remove bob --confirm            # refuses while collections exist
calctl principal auth bob --password-stdin < pw  # verify against digesta1; exit 0 on match
calctl health --json          # every command takes --json for scripts/monitoring
calctl health --url http://127.0.0.1:8080/ --db postgres://localhost/calserver
```

`health` reports one row per component, all derived from `config.toml`:

| Component | Probe | Not healthy when |
|---|---|---|
| `calendsd` | unauthenticated `OPTIONS` on the DAV listener | no HTTP answer |
| `contacts-api` | unauthenticated `GET /api/v1/health` on `api_addr` (401 is healthy) | no answer, or no auth backend |
| `database` | a pool opened the way calendsd opens it, plus the schema check | unreachable, schema missing or behind the binary; degraded when no DSN or the schema is newer |
| `database[host:port]` | each host of a multi-host DSN on its own (only when there are several) | degraded when some nodes fail, unhealthy when all do |
| `db-pool` | the running daemon's `go_sql_*` gauges from `metrics_addr` | degraded when in-use reaches the cap |
| `jobs` | due, stuck-executing and discarded rows in the job queue | degraded when due work is >10m old or a claim is >15m old |
| `rcptd` | `/auth` with a probe address (404 is healthy) | token rejected or no answer; degraded on 503 |

The per-node rows exist because calendsd tries a multi-host DSN in order:
a node that silently drops traffic costs each new connection a full
`connect_timeout`, which the `database` row (some host answered) hides.

`health` exits 1 on an unhealthy component (degraded only warns), `doctor` on
any finding, so both slot straight into monitoring. The `principal` writes go through the same
`storage.AdminWrites` carve-out as the admin panel (digesta1 computed under
the configured realm; override with `--realm`).

## /health

The DAV listener answers an unauthenticated `GET`/`HEAD /health` (at the
root, whatever the base URI) for load balancers and uptime monitors:
`200 {"status":"ok","database":"ok"}` while the database answers a ping
within 2s, `503 {"status":"unavailable","database":"unreachable"}` otherwise.
The database error is logged, never returned. Probes are counted in metrics
(`route="health"`) but kept out of the access and request logs.

```sh
curl -fsS http://127.0.0.1:8080/health
```

## Metrics

Set `-metrics-addr` (or `[server].metrics_addr`) to serve Prometheus metrics
and Go pprof on a second plain-HTTP listener (METRICS-PLAN.md has the full
design and metric inventory):

```sh
calendsd -config config.toml -metrics-addr 127.0.0.1:9464
curl -s http://127.0.0.1:9464/metrics | grep calserver_
go tool pprof http://127.0.0.1:9464/debug/pprof/profile
```

The listener has no TLS and no auth — bind loopback or a firewalled
interface, never the public one. Empty (the default) disables it entirely.
All series are named `calserver_*`; every label comes from a closed set
(normalized method, route pattern, typed auth outcome, …), never from
request data, so cardinality stays bounded. Sample scrape config:

```yaml
scrape_configs:
  - job_name: calendsd
    static_configs:
      - targets: ["127.0.0.1:9464"]
```

## Test corpus

The port is driven red→green against fixtures extracted from sabre's own test
suite into `testdata/corpus/` (see PLAN.md §7). Regenerate after bumping the
sabre checkout:

```sh
cd dav && composer install
CORPUS_DIR=../testdata/corpus vendor/bin/phpunit \
  --configuration tests/phpunit.xml tests/Sabre/CalDAV tests/Sabre/DAV
```

The Go corpus runner (`go test ./internal/corpus/ -v -run TestCorpus`) replays
each record and reports coverage. Porting a feature moves its records into the
`passing` set in `internal/corpus/runner_test.go`.

Parity of the PUT-time denormalization (etag, occurrence bounds) is separately
pinned against sabre via `internal/caldav/ical/testdata/golden.json`,
regenerated with `scripts/denormalize-golden.php`.
