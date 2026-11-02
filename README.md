# calendsd

A CalDAV and CardDAV server in Go, backed by Postgres. It began as a port of
[sabre/dav](https://sabre.io/) and still reads and writes sabre's table
layout, so it can take over an existing sabre/dav database in place. The schema is calendsd's own now: it adds
its own tables through versioned migrations (`calctl schema apply`) and no
longer treats sabre as a rollback target.

It runs in production at [Migadu](https://www.migadu.com/), serving calendars
and contacts for every mailbox.

## Features

- **CalDAV** (RFC 4791) with `sync-collection` (RFC 6578) and `add-member`
  (RFC 5995).
- **Scheduling** (RFC 6638): free-busy, auto-scheduling between local users,
  and email invitations (iMIP) to everyone else, sent from a durable job queue
  through your SMTP submission server.
- **Availability** (RFC 7953): working hours that shape free-busy answers.
- **CardDAV** (RFC 6352).
- **Calendar sharing** and subscriptions, with optional share-invite emails.
- **WebDAV-Push**, so clients that support it learn about changes without
  polling.
- **Authentication** against the database, a Dovecot passwd-file, or an rcptd
  hash service. The last two share your mail server's passwords.
- **Operations:** a management CLI (`calctl`), a read-mostly admin panel,
  Prometheus metrics and pprof, `/health`, soft-delete with delayed purge, and
  multi-host Postgres failover.
- **A webmail contacts API** (`docs/webmail-contacts.md`), so a webmail and a
  phone share one address book.
- **calclient**, a small web calendar and contacts UI that proxies to
  calendsd.

## Binaries

| Binary | What it is |
| --- | --- |
| `calendsd` | The DAV server. |
| `calctl` | Management CLI: schema migrations, principals, health, consistency checks, the admin panel. |
| `calclient` | Web UI. It serves the embedded client and reverse-proxies DAV requests to a calendsd, so the browser talks to a single origin. |

## Quickstart

You need Go (see `go.mod` for the version) and Postgres (developed against 16).

```sh
go install github.com/migadu/calendsd/cmd/...@latest

createdb calendsd
export DSN="postgres:///calendsd?sslmode=disable"
calctl schema apply --db "$DSN"
calctl principal add alice --email alice@example.org --password-stdin --db "$DSN" <<< 'correct horse'

calendsd -db "$DSN" -auto-provision     # listens on :8080
```

Point a CalDAV/CardDAV client at `http://localhost:8080/` and log in as
`alice`. `-auto-provision` gives each user a default calendar and address
book on first login. Without `-db`, calendsd runs on an in-memory store, which is handy
for a quick look and loses everything on exit.

For anything beyond a quick look:

1. Copy [`config.toml.example`](config.toml.example) to `config.toml`. Every
   key is documented there and every one is optional. calendsd and calctl
   look for it in `/etc/calendsd/config.toml` and
   `/usr/local/etc/calendsd/config.toml`, or take `-config PATH`.
2. **Serve it over TLS**, either with `[server.tls]` or behind a reverse
   proxy. The Basic auth that the passwd-file and rcptd modes require sends
   the password with every request.
3. **Pick how users log in:**
   - **Database** (the default): users live in the `users` table and are
     managed with `calctl principal …`. Supports Basic and Digest.
   - **Dovecot passwd-file**: set `server.passwd_file` to the file your IMAP
     server already uses (`user:{SCHEME}hash` lines; bcrypt, SHA512-CRYPT,
     SHA256-CRYPT, SSHA512 and SHA512 are understood). The file is re-read
     when it changes. Pair it with `calendarserver.auto_provision = true` so a
     first login creates the user's principal, calendar and address book.
   - **rcptd**: `[server.rcptd]` fetches hashes from an rcptd service instead
     of a local file; same verification, same pairing with auto-provisioning.
4. **Email invitations** (optional): configure `[scheduling.imip]` with your
   SMTP submission server. See [docs/imip.md](docs/imip.md).

## Operations

### calctl

`calctl` reads the same `config.toml` as the server. Every command takes
`--json`.

```sh
calctl health                 # every dependency, probed concurrently (see below)
calctl panel                  # serve the admin panel (127.0.0.1:8092)
calctl stats                  # row counts + recent change-log activity
calctl doctor                 # consistency checks; exit 1 on any finding
calctl schema apply           # apply pending schema migrations
calctl principal list         # principals with email/displayname/login capability
calctl principal add bob --email bob@example.org --password-stdin < pw
calctl principal edit bob --name "Robert"        # --email "" clears; --password resets
calctl principal remove bob --confirm            # refuses while collections exist
calctl principal auth bob --password-stdin < pw  # verify a password; exit 0 on match
calctl principal purge-expired                   # purge soft-deleted principals (run daily)
```

`calctl help` lists everything else (jobs, iMIP resends, contact import).

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

`health` exits 1 on an unhealthy component (degraded only warns), and `doctor`
exits 1 on any finding, so both slot straight into monitoring.

### Admin panel

`calctl panel` serves a debugging UI over the same database: inspectors for
principals, calendars and objects; a privilege simulator that answers "what
can user X do on path Y" through the production plugin chain; a scheduling
and sync debugger; the consistency doctor; and, behind `-readonly=false`,
gated write actions. Every page doubles as a JSON endpoint under `/api/`.

```sh
htpasswd -cB admin-auth.htpasswd root   # bcrypt admin credentials
calctl panel -admin-auth admin-auth.htpasswd   # binds 127.0.0.1:8092
```

Run it with the same config as calendsd. Keep the listener on loopback or
behind TLS. Write actions are off by default, need the
`X-Admin-Request: 1` header, and audit-log every mutation.

### /health

The DAV listener answers an unauthenticated `GET`/`HEAD /health` (at the root,
whatever the base URI) for load balancers: `200 {"status":"ok","database":"ok"}`
while the database answers a ping within 2s, and
`503 {"status":"unavailable","database":"unreachable"}` otherwise.

### Metrics

Set `-metrics-addr` (or `[server].metrics_addr`) to serve Prometheus metrics
and Go pprof on a second plain-HTTP listener. It has no TLS and no auth, so
bind it to loopback or a firewalled interface.

```sh
calendsd -config config.toml -metrics-addr 127.0.0.1:9464
curl -s http://127.0.0.1:9464/metrics | grep calserver_
go tool pprof http://127.0.0.1:9464/debug/pprof/profile
```

All series are named `calserver_*`, and every label comes from a closed set,
so cardinality stays bounded. The metric inventory is in
[docs/design/METRICS-PLAN.md](docs/design/METRICS-PLAN.md).

## Documentation

- [`config.toml.example`](config.toml.example): every setting.
- [`docs/`](docs/): feature notes (iMIP, share emails, webmail contacts, DST
  handling, slow-query work, audit findings).
- [`docs/design/`](docs/design/): the design notes behind each subsystem,
  starting with the porting plan in [PLAN.md](docs/design/PLAN.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the development setup and tests,
and [SECURITY.md](SECURITY.md) for reporting vulnerabilities.

## License

MIT, see [LICENSE](LICENSE). The reference sabre/dav checkout in `dav/` (and
the test corpus extracted from it) keeps its own BSD license, see
[dav/LICENSE](dav/LICENSE). The bundled IBM Plex fonts are under the SIL Open
Font License.
