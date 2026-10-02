# CalDAVTester conformance harness

[CalDAVTester](https://github.com/apple/ccs-caldavtester) is Apple's
CalDAV/CardDAV protocol conformance suite — the same one sabre validates
against. This directory runs a curated subset of it against a local
`calserver`.

```sh
make caldavtester                                      # curated list (tests.txt)
make caldavtester-full                                 # the entire suite, categorized

# or call the scripts directly:
direnv exec . scripts/caldavtester/run.sh              # curated list
direnv exec . scripts/caldavtester/run.sh CalDAV/put.xml   # a single file
VERBOSE=1 direnv exec . scripts/caldavtester/run.sh CalDAV/put.xml  # per-test results and failure reasons
direnv exec . scripts/caldavtester/run-full.sh         # full sweep (fresh server per file)
```

The first run takes a while: it compiles a Python 2.7 interpreter (CalDAVTester
is Python 2 only, and Python 2 was dropped from the pinned nixpkgs) and clones
the suite + `pycalendar` under `.dev/caldavtester/`. Subsequent runs reuse both.

## What the pieces are

| File | Purpose |
|------|---------|
| `run.sh` | builds py2, checks out the suite, tailors serverinfo, seeds the DB, starts calserver, replays each file, prints a pass/fail tally |
| `serverinfo-overrides.py` | rewrites the upstream `serverinfo.xml` for calserver: host/port, a trimmed `<features>` list, and substitutions that map CalDAVTester's `__uids__`/GUID paths onto sabre's `/calendars/<user>/` layout |
| `seed.sql` | idempotent seed of `user01`/`user02` (principals, digest auth, `calendar`+`tasks` calendars, `addressbook`) |
| `tests.txt` | the curated file list (standards-based files; CS-only extensions excluded) |

## Reading the results

The suite targets Apple Calendar Server, so many upstream tests assume
behaviours calserver deliberately does not have. Expect three categories of
non-passing results that are **not** calserver bugs:

- **CS provisioning assumptions.** The suite expects a default
  calendar/addressbook that regenerates after `DELETEALL`. calserver (like
  sabre) never auto-recreates collections; `run.sh` re-seeds before each file
  to compensate, but any test that deletes then re-reads a *default* collection
  mid-file will still fail.
- **CS-specific expected values.** Exact `DAV:` header contents, the home
  collection's child set (CS includes `freebusy`/`notifications`), absolute
  `https://…/` well-known redirects, and the `sync-report-home` expansions.
- **Harness state contamination.** Destructive files delete shared
  collections; per-file-in-isolation numbers are the trustworthy ones.

### Known failures in `CalDAV/availability.xml`

Two of its seven tests fail, by design, not because of a calserver bug
(VAVAILABILITY-PLAN.md §7):

- `Capabilities/2` expects VTIMEZONE and VFREEBUSY in a calendar's
  `supported-calendar-component-set` (CalendarServer's default). calserver's
  is `VEVENT,VTODO`.
- `free-busy reports/1` matches CalendarServer's exact text:
  `FREEBUSY;FBTYPE=BUSY:…T170000Z/PT1H` and the two BUSY-UNAVAILABLE spans
  comma-joined on one line. calserver writes free-busy in sabre's format, so
  the same periods come back as `FREEBUSY:…T170000Z/…T180000Z` and one
  BUSY-UNAVAILABLE line per span.
  `TestGenerateFreeBusyCalDAVTesterAvailability` replays this test's data and
  pins those periods.

RFC-level behaviour (sync-collection, calendar-query, multiget, ACL reports) is
verified more directly by the sabre-extracted corpus in `internal/corpus/` and
the live parity test in `internal/storage/`. Treat CalDAVTester as an
additional, adversarial cross-check — its malformed-input tests in particular
catch things the well-formed corpus cannot.
