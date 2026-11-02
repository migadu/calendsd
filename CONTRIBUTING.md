# Contributing to calendsd

Bug reports, fixes and features are welcome. For anything larger than a
small fix, please open an issue first so the design can be discussed before
you invest in code. Security problems go to [SECURITY.md](SECURITY.md), not
the issue tracker.

## Development environment

Everything comes from the Nix flake. With [direnv](https://direnv.net/), run
`direnv allow` once and the toolchain is on your PATH: Go, gopls,
golangci-lint, sqlc, Postgres 16, PHP with pdo_pgsql and composer (for the
reference sabre/dav checkout), and Node with Playwright (for the web client
tests). Without direnv, use `nix develop`.

Without Nix, you need Go (version in `go.mod`) and Postgres 16. That covers
the server and its tests; PHP is only needed to regenerate the sabre-derived
fixtures.

## Running it locally

```sh
make dev     # dev Postgres + calendsd (:8080) + admin panel (:8092) + web UI (:8091/cal/)
make seed    # demo data: a second user, events, an invitation, contacts
make help    # every target
```

`scripts/dev-db.sh start` runs a private Postgres on a unix socket under
`.dev/`, so nothing touches your system's database. It applies the schema
and seeds the dev accounts. `make db-reset` starts the schema over.

The dev DAV login is `admin` / `admin` (Basic or Digest, realm `SabreDAV`); the
admin panel's is `root` / `root`.

## Tests

```sh
make test    # go test ./... (start the dev DB first, or storage tests skip)
make vet
make corpus  # replay the protocol corpus extracted from sabre/dav's own tests
```

- **Go tests.** Tests that need Postgres skip when no database is reachable.
  CI starts one, so please run them against the dev database before sending
  a change to storage code.
- **Protocol corpus.** `testdata/corpus/` holds request/response records
  extracted from sabre/dav's test suite. `go test ./internal/corpus/ -run
  TestCorpus -v` replays them and reports coverage. Porting a feature moves
  its records into the `passing` set in `internal/corpus/runner_test.go`. To
  regenerate after bumping the `dav/` checkout:

  ```sh
  cd dav && composer install
  CORPUS_DIR=../testdata/corpus vendor/bin/phpunit \
    --configuration tests/phpunit.xml tests/Sabre/CalDAV tests/Sabre/DAV
  ```

- **Golden files.** The PUT-time denormalization (etag, occurrence bounds) is
  pinned against sabre in `internal/caldav/ical/testdata/golden.json`,
  regenerated with `scripts/denormalize-golden.php`. The expand and free-busy
  goldens have their own scripts in `scripts/`.
- **Conformance suites.** `make caldavtester` runs a curated CalDAVTester
  subset; CI also runs litmus (informational).
- **Web client.** `cd cmd/calclient && npm install && npm test` (unit tests)
  and `npm run lint`; Playwright end-to-end tests live in
  `cmd/calclient/web/e2e/`.
- **sabre parity.** `make parity` (or `PARITY=1 go test ./internal/storage/
  -run TestParity -v`) runs calendsd and a real sabre instance against one
  database and cross-checks reads and writes. It is opt-in and not part of CI.

## Code layout

| Path | What lives there |
| --- | --- |
| `cmd/calendsd` | The server binary. |
| `cmd/calctl` | Management CLI and admin panel. |
| `cmd/calclient` | Web UI (plain JavaScript, no build step) and its proxy. |
| `internal/dav` | WebDAV core: the node tree, PROPFIND/PROPPATCH, preconditions, the plugin chain. |
| `internal/caldav`, `internal/carddav` | CalDAV/scheduling and CardDAV on top of it. |
| `internal/storage` | Postgres backends. |
| `internal/schema/migrations` | Versioned schema migrations, applied by `calctl schema apply`. |
| `internal/auth`, `internal/passwd`, `internal/acl` | Authentication backends, hash verification, and access control. |
| `internal/push`, `internal/jobs` | WebDAV-Push and the durable job queue. |
| `dav/` | Reference sabre/dav checkout (BSD). Not built; read for behavior and used to extract fixtures. |
| `docs/design/` | Design notes; the reasoning behind most subsystems. |

## Changes

- Run `gofmt` (`make fmt`), `go vet` and `golangci-lint run`
  (configured in `.golangci.yml`). CI runs all three, plus the tests.
- **Schema changes** go in a new numbered file in
  `internal/schema/migrations/`. Never edit a migration that has shipped:
  databases record migrations by version, so an edit would never reach them.
- **Behavior changes** need a test. Protocol behavior that sabre has an
  opinion on should match sabre unless there is a written reason not to.
- **Commit messages:** a short imperative subject line, then a body that
  explains why the change was needed and what it does. The existing history
  is the best guide.
- By contributing, you agree that your contributions are licensed under the
  [MIT License](LICENSE).
