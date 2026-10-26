# Slow queries — Plan

Source: pg_stat_statements on production (2026-10-26). Two statements
dominate:

| Statement | Rows/call | Mean | Max |
|---|---|---|---|
| `SELECT uri, email, displayname FROM principals …` | ~178k | 2 s | 22 s |
| `SELECT <objectColumns> FROM calendarobjects WHERE calendarid = $1` | per calendar | 180 ms | 13 s |

The principals statement only looked like it had no WHERE clause. The
WHERE is on the second line of the query in
`internal/storage/principals.go:33`, and the stats view showed only the
first line. The WHERE (`uri LIKE 'principals/%' AND uri NOT LIKE
'principals/%/%'`) only drops proxy sub-principals, so it still matches
every user.

pg_stat_statements times each statement on its own, so the 22 s max is
the scan itself (I/O, lock contention, or a slow consumer: the timer runs
until the last row is sent). The per-child point queries that item 2
describes sit in their own row, `SELECT uri, email, displayname FROM
principals WHERE uri = $1`. Its `calls` count is the direct evidence for
the N+1; pull it from the same snapshot and record it here.

Items are ordered by priority. Each one lists the evidence, the change and
how it is tested. Make one commit per item, each with a regression test.

| # | Item | Why | Size |
|---|------|-----|------|
| 1 | Find the callers, count queries in tests | confirm which clients and endpoints are responsible before changing behaviour; items 2–4 are tested by query count and nothing counts queries today | M |
| 2 | Scoped root listings | a PROPFIND Depth:1 on a root costs 1 full scan plus ~178k point queries | M |
| 3 | Metadata-only calendar listing | Depth:1 on a calendar reads every `calendardata` | M |
| 4 | Batched object fetch | `QueryObjects` runs N+1 queries | S |
| 5 | Paginate admin and calctl lists | operator tools also list every principal | S |

## 1. Find the callers, count queries in tests

**Evidence.** `PrincipalsByPrefix` has five callers:

- `acl.PrincipalCollection.Children` (`internal/acl/principal.go:196`):
  PROPFIND Depth:1 on `/principals/`
- `caldav.CalendarRoot.Children` (`internal/caldav/nodes.go:50`): Depth:1 on
  `/calendars/`
- `carddav.AddressBookRoot.Children` (`internal/carddav/nodes.go:42`):
  Depth:1 on `/addressbooks/`
- the admin principals page (`internal/admin/pages_principals.go:90`)
- `calctl principal list` (`cmd/calctl/principal.go:92`)

`principalForAddress` (`internal/caldav/schedule_principals.go:100`) also
has a fallback that lists all principals. It is unreachable in production
because `storage.Principals` implements `AddressResolver`
(`internal/storage/lookups.go`).

`CalendarObjects` has three callers: `Calendar.Children`
(`internal/caldav/nodes.go:275`), `export.go:140` and the `ObjectByUID`
fallback (`nodes.go:329`). The fallback is unreachable because storage
implements `UIDLookup`.

**Change.**
- Add `DBOpTimer("principals_by_prefix")` and
  `DBOpTimer("calendar_objects")` around the two queries.
  `storage.Principals` has no `Metrics` field (`principals.go:19`), only
  `CalDAV` and the CardDAV backend do; add one and wire it at every
  construction site, including the inline `&storage.Principals{DB: q.DB}`
  in `cmd/calctl/principal.go:92`. A nil set records nothing, as for
  `CalDAV`.
- In the PROPFIND handler, log path, depth, principal, User-Agent, child
  count and duration at info level whenever the path is one of the three
  roots, or whenever the listing returns more than 1000 children.
- Deploy, collect logs for a day and note the client mix here. That
  decides whether item 2 can be strict (see the open question there).
- Test harness, as its own commit: a `database/sql` driver wrapper for
  tests that records every statement text, so a test can count queries
  and assert that a column was never selected. Items 2, 3 and 4 all test
  that way, and nothing in the repo counts queries today.

**Tests.** Metrics test covers the two new op labels. The harness tests
itself: a known sequence of queries produces the expected log. No
behaviour change.

## 2. Scoped root listings

**Evidence.** `Server.propfind` (`internal/dav/server.go:352`) calls
`VisibleChildren`, which loads every child and then runs
`acl.Plugin.FilterChildren` (`internal/acl/privileges.go:249`). That hook
keeps the requester, plus every principal in the requester's email domain
(admins keep everything). It computes each child's domain with
`principalEmailDomain`, and that function goes through `Tree.NodeForPath`.
For a `/calendars/` listing, a single request therefore costs:

1. one `PrincipalsByPrefix` scan (178k rows), then
2. one `PrincipalByPath` query per child to read the owner's email
   (~178k round trips).

The 178k round trips don't show in the scan's 22 s max (see the note on
pg_stat_statements above); they are the `calls` count on the
`WHERE uri = $1` row, and the request as a whole pays both. The harness
from item 1 confirms the count.

**Change.**
- Add a storage query that returns the principals the domain filter would
  keep, filtered in SQL: `PrincipalsByPrefixInDomain(ctx, prefix, domain)`.
  It uses the same `uri LIKE` filter as `PrincipalsByPrefix` plus
  `email LIKE '%@_%' AND lower(split_part(email, '@', -1)) = $3`. The
  `-1` (PostgreSQL 14+) takes the text after the *last* `@` and the guard
  rejects a missing or trailing `@`, which is what `principalEmailDomain`
  does (`internal/acl/privileges.go:318`); `split_part(…, 2)` would split
  at the first `@` instead.
- Migration 0005 (0004 is item 3's, which ships first): `CREATE INDEX
  principals_email_domain ON principals (lower(split_part(email, '@',
  -1)))`. The existing `principals_lower_email` index cannot serve a
  suffix match.
- Add an optional interface in `internal/dav`:

  ```go
  // ScopedLister is a collection that can list only the children the
  // requester may see, instead of listing all of them for ChildFilterHook
  // to prune.
  type ScopedLister interface {
      ChildrenScoped(ctx context.Context, scope ChildScope) ([]Node, error)
  }
  ```

  `VisibleChildren` (`internal/dav/server.go:486`) knows nothing about the
  ACL plugin, so `ChildFilterHook` gets a second method,
  `ChildScope(ctx, path) (ChildScope, bool)`, that returns the requester,
  the requester's domain and whether the requester is an admin, and
  `false` for paths it does not filter. `VisibleChildren` uses
  `ChildrenScoped` when the collection implements it and a hook returns a
  scope. Otherwise it keeps the existing path. Every other filter hook
  still runs on the scoped result, as `Hooks` iterates all of them. The
  scoping hook then skips children that were already scoped, so no
  per-child `NodeForPath` lookup runs.
- `PrincipalCollection`, `CalendarRoot` and `AddressBookRoot` implement it
  through the new storage query. The requester is always included, even
  with no email domain, which matches what `FilterChildren` does today.
- Admin requests keep the full listing. That is acceptable for now
  because it is rare, and item 5 covers the operator tools.
- `FilterChildren`'s per-child `principalEmailDomain` stays as it is: after
  this item every root in production implements `ScopedLister`, so the
  per-child path only runs for a collection that opts out, and none does.

**Open question.** A large domain could still list tens of thousands of
users. If item 1 shows that no real client relies on the root listing
(autodiscovery normally follows `current-user-principal` and
`calendar-home-set`), consider listing only the requester and their
proxy/delegate grantors for non-admins. That is an observable behaviour
change, so it is decided after item 1 and not built into this item.

**Tests.**
- Unit: `ChildrenScoped` returns exactly what `Children` followed by
  `FilterChildren` returns, for a same-domain user, a user with no email,
  and an admin. Write it as a table test that compares the two paths. The
  corpus includes a principal with two `@` in its email, one with a
  trailing `@`, and one whose domain differs only in case, so the SQL and
  Go domain rules are compared on the inputs where they could diverge.
- Storage: counting queries for a Depth:1 PROPFIND on `/calendars/` with
  1000 principals across 3 domains gives at most 3 queries (it is ~1001
  today).
- `EXPLAIN` the domain query on the seeded corpus and assert it uses the
  new index.
- Run CalDAVTester principal and propfind suites and record ok/fail counts
  before and after.

## 3. Metadata-only calendar listing

**Evidence.** `CalDAV.CalendarObjects` (`internal/storage/objects.go:26`)
selects `objectColumns`, which includes `calendardata` and
`coalesce(etag, md5(calendardata))`. Its hot caller is `Calendar.Children`,
which serves PROPFIND Depth:1 on a calendar. Clients that don't use
sync-collection issue that request to compare `getetag`, and it needs only
uri, etag, size, lastmodified and componenttype. Upstream sabre's
`getCalendarObjects` leaves out `calendardata` for this reason. An index
won't help here: the cost is reading the TOASTed data.

**Change.**
- Backfill once in migration 0004: `UPDATE calendarobjects SET etag =
  md5(calendardata) WHERE etag IS NULL`. Count the NULL rows on
  production first; if there are more than a few thousand, run the update
  in batches by `id` range outside the migration and leave the migration
  as the final check. After that, a metadata query needs no
  `md5(calendardata)` fallback. Writes already store an etag; add a check
  in `internal/schema/check.sql` that none are NULL, and widen that file's
  header, which currently says it audits 0002 only.
- Add `CalendarObjectsMeta(ctx, id) ([]caldav.ObjectInfo, error)` to the
  backend, with `CalendarData` left nil. `Calendar.Children` uses it.
- Make `CalendarObject` load lazily. When `obj.CalendarData` is nil, the
  first `Get`, `viewData` or calendar-data property read fetches it with
  `CalendarObject(ctx, id, uri)` and memoizes it, as `view` is memoized
  today. If the row is gone by then (deleted between the listing and the
  fetch), `Get` returns not-found and the property read reports the
  property as missing; neither serves an empty body.
- Size on read-only shares: `viewData` redacts confidential and private
  events there, so `getcontentlength` comes from the redacted body. Keep
  that correct by loading lazily on read-only views. Owner and read-write
  views use the stored `size`. That column is new to the read path:
  today `Size()` is `len(viewData())` for every view
  (`internal/caldav/nodes.go:649`). It is written as `len(data)`, so it
  should match; the seeded-corpus check asserts `size =
  octet_length(calendardata)`.
- The ETag is unchanged: the served etag is the stored one, even for a
  redacted view, as today.
- `export.go` keeps the full query. It could stream rows instead of
  building a slice, but that is out of scope here.

**Tests.**
- Storage: `CalendarObjectsMeta` returns the same uri, etag and size as
  `CalendarObjects`, with nil data.
- DAV: Depth:1 PROPFIND for `getetag`/`getcontentlength` on a 500-object
  calendar runs one query and never selects `calendardata` (assert on the
  query log).
- Read-only share with a CLASS:PRIVATE event: `getcontentlength` equals the
  redacted length.
- A calendar-multiget on objects listed through `Children` still returns
  the full data.

## 4. Batched object fetch

**Evidence.** `Calendar.QueryObjects` (`internal/caldav/nodes.go:313`) gets
URIs from `CalendarQuery` and then calls `CalendarObject` once per URI. A
broad calendar-query, such as a time-range query with no bounds, makes N+1
round trips.

calendar-multiget is worse: `calendarMultigetReport`
(`internal/caldav/plugin_reports.go:277`) builds one `objectRef` per href
and resolves each path separately, which walks the tree per href
(principal, home, calendar, object).

**Change.** Add `CalendarObjectsByURI(ctx, id, uris []string)`, which
selects `objectColumns` with `uri = ANY($2)` and is served by
`calendarobjects_ukey`. Better still, have `CalendarQuery` return the rows
directly and drop the second query. For multiget, group the hrefs by
parent calendar path, resolve each calendar once and fetch its objects
with the same `uri = ANY` query; hrefs that resolve to nothing still get
their 404 entry.

**Tests.** A query over 200 matching objects runs 1 or 2 queries instead
of 201. Results match the per-URI path, including order. A multiget of
200 hrefs in one calendar resolves the calendar once and fetches with one
query; a missing href is still reported 404.

## 5. Paginate admin and calctl lists

**Evidence.** The admin principals page and `calctl principal list` load
all 178k principals and build rows in memory.

**Change.** Add keyset pagination (`uri > $after ORDER BY uri LIMIT $n`)
to a new `PrincipalsPage` storage method. The admin page gets a search box
(prefix on uri/email) and next/previous links. `calctl principal list`
gets `--limit` (default 100) and `--after`, and `--all` streams rows.

**Tests.** Pagination boundaries (empty, exact page, last partial page);
`calctl` flags; the admin page renders with a search term.

## Rollout

Items 1, 3 and 4 have no client-visible behaviour change and can ship
together. Item 2 keeps today's visibility semantics exactly, which the
table test enforces, so it ships next; item 5 follows. Once all have run
in production, compare pg_stat_statements: `principals_by_prefix` calls
should drop to admin PROPFINDs and `calctl --all`, the `WHERE uri = $1`
row's calls should drop by the per-child lookups, and the
`calendarobjects WHERE calendarid` mean should fall roughly in proportion
to the payload size.

## Status (2026-10-26)

All five items are implemented, one commit each, in the order 1, 3, 4, 2,
5 (so migration 0004 is item 3's backfill and 0005 is item 2's index).
Measured with the query log from item 1, on the test corpora:

| Request | Queries before | Queries after |
|---|---|---|
| Depth:1 PROPFIND on `/calendars/`, 1000 principals in 3 domains | ~1001 | 2 on `principals`, 1 `propertystorage` read per listed home |
| Depth:1 PROPFIND on a calendar for getetag/getcontentlength | 1 full-payload read | 1 metadata read, no `calendardata` |
| calendar-multiget, 200 hrefs in one calendar | ~800 (4 per href) | 1 `calendarobjects` read + 1 `propertystorage` read per object |
| calendar-query matching 200 objects | ~800 | 2 `calendarobjects` reads + 1 `propertystorage` read per object |

Two things came out of the counting that were not in the plan:

- The ACL gate on every node of a response resolved the requester's
  principal and group membership again for each node: two queries per
  object of any multi-object response. The request's principal holder now
  memoizes the set (`dav.RequestPrincipals`), its own commit after item 4.
- One `propertystorage` read per node remains in every PROPFIND and
  report, from the property-storage plugin's late hook, which is per node
  as in sabre. It is the one per-object query left; batching it (one
  `path = ANY($1)` per response) is the next item if the listings are
  still slow.

CalDAVTester (`scripts/caldavtester/run.sh`), the propfind and
current-user-principal files for CalDAV and CardDAV, before (d483756) and
after: identical, 32 ok / 16 fail / 3 ignored in total, with the same
per-file counts (CalDAV/propfind 14/12/1, CardDAV/propfind 12/4/0, the two
current-user-principal files 3/0/1). The 16 failures predate this work.
