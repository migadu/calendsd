# calserver

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
scripts/dev-db.sh start          # local Postgres (unix socket under .dev/), sabre schema loaded
php -S 127.0.0.1:8081 scripts/sabre-server.php   # reference sabre instance
go run ./cmd/calserver -db "postgres:///calserver?host=$PGHOST"  # Go server (default :8080)
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

`calserver-admin` is a standalone debugging UI over the same shared database
(see [ADMIN-PLAN.md](ADMIN-PLAN.md)): principal/calendar/object inspectors, a
privilege simulator that answers "what can user X do on path Y" through the
production plugin chain, a scheduling/sync debugger, a consistency doctor,
and (behind `-readonly=false`) gated setup actions — every page doubles as a
JSON endpoint under `/api/`.

```sh
htpasswd -cB admin-auth.htpasswd root   # bcrypt admin credentials
go run ./cmd/calserver-admin -db "postgres:///calserver?host=$PGHOST" \
  -admin-auth admin-auth.htpasswd       # binds 127.0.0.1:8081
```

Run it with the **same config file / Calendar-Server flags as calserver**
(both binaries resolve them through the same code and log an identical
`effective config` line — the dashboard echoes it so drift is a string
diff). Basic auth resends the password on every request: keep the listener
on loopback/LAN or behind TLS. Write actions are off by default, need the
`X-Admin-Request: 1` header, and audit-log every mutation.

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
