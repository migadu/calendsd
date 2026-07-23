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
go run ./cmd/calserver           # Go server (default :8080)
```

Credentials: `admin` / `admin` (Digest, realm `SabreDAV` — the realm is baked
into `users.digesta1`, see PLAN.md §5).

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
