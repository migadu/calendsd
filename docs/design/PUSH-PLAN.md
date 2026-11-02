# WebDAV-Push implementation plan

Status: **implemented** (milestones 1–4). Server: `internal/push/` (plugin,
store, worker, sender), pg_notify trigger in `internal/storage/{sync,carddav}.go`,
config in `[push]`, schema `push_subscriptions` in
`scripts/schema/pgsql.calserver.sql`, wired in `internal/server/server.go` +
`cmd/calendsd/main.go`. Web client: `cmd/calclient/web/shared/push.js` +
`push-sw.js`, a Push section in the explorer's sync panel, and an "Instant
updates" toggle in the calweb settings. Unit + Postgres integration tests in
`internal/push/`. Milestone 5 (metrics, self-change suppression, VAPID
rotation) remains as follow-up.
Goal: let clients learn of calendar & address-book changes in **seconds** instead
of waiting out their poll interval, by adding server-initiated push per the
[WebDAV-Push draft](https://bitfireat.github.io/webdav-push/draft-bitfire-webdav-push-00.html)
(`draft-bitfire-webdav-push-00`, also filed as `draft-gajda-dav-push-00`).

---

## 1. Why, and what this does / doesn't buy us

Today the server correctly implements RFC 6578 incremental sync (`sync-collection`
REPORT + `getctag`/`sync-token`), but there is **no server→client notification** of
any kind. Change latency is therefore entirely the *reading* client's poll
interval — 30 s for the web client (`cmd/calclient/web/shared/store/session.js:93`),
and 15 min+ for an iPhone falling back to its Fetch schedule.

WebDAV-Push closes that gap by delivering an encrypted Web Push message the moment a
collection changes; the client then does its normal `sync-collection` REPORT.

**Scope / reality check on clients:**

- ✅ **DAVx⁵ ≥ 4.4.2 (Android)** — the only shipping client that speaks WebDAV-Push
  today. This is the primary beneficiary.
- ✅ **Our own web client** — we can implement a Service Worker + Web Push
  subscription and get near-instant updates instead of the 30 s poll.
- ❌ **iOS Calendar / macOS Calendar** — do **not** support WebDAV-Push. They only
  honor Apple's proprietary APNS `push-transports`, which self-hosted servers can't
  obtain. **This plan does not help the iPhone.** For iPhone the only lever remains
  lowering the Fetch interval. Call this out to stakeholders so expectations are set.

Because the near-term real-world win is DAVx⁵ + our web client, this is worth doing
but should be scoped as such, not sold as "fixes iPhone sync."

---

## 2. Protocol summary (what we must implement)

Namespace: `https://bitfire.at/webdav-push` (call it `NSPush`).

**a) Advertise support**
- `DAV:` response header on every request includes the token `webdav-push`.
- On each calendar / address-book collection, PROPFIND returns:
  - `{NSPush}transports` — the offered transports and our VAPID public key:
    ```xml
    <P:transports xmlns:P="https://bitfire.at/webdav-push">
      <P:web-push>
        <P:vapid-public-key type="p256ecdsa">BASE64URL…</P:vapid-public-key>
      </P:web-push>
    </P:transports>
    ```
  - `{NSPush}topic` — a **server-wide unique, opaque, stable** id for the collection
    (e.g. `O7M1nQ7cKkKTKsoS_j6Z3w`).
  - `{NSPush}supported-triggers` — e.g. `content-update` (depth 1) for collections.

**b) Register a subscription** — client sends `POST` to the *collection URL* with a
`{NSPush}push-register` body carrying its Web Push endpoint + keys:
```xml
<P:push-register xmlns:P="https://bitfire.at/webdav-push" xmlns:D="DAV:">
  <P:subscription>
    <P:web-push-subscription>
      <P:push-resource>https://up.example.net/yohd4yai5Phiz1wi</P:push-resource>
      <P:subscription-public-key type="p256dh">BASE64URL…</P:subscription-public-key>
      <P:auth-secret>BASE64URL…</P:auth-secret>
    </P:web-push-subscription>
  </P:subscription>
  <P:trigger><P:content-update><D:depth>infinity</D:depth></P:content-update></P:trigger>
  <P:expires>Wed, 20 Dec 2023 10:03:31 GMT</P:expires>
</P:push-register>
```
Response: `201`/`204` with `Location:` (absolute URL of the subscription resource)
and `Expires:` (server-chosen, IMF-fixdate; spec recommends ≥ 3 days). Re-POSTing the
same `push-resource` **updates** the existing subscription (idempotent).

**c) Deliver a notification** — on change, server sends a Web Push `POST` to each
subscriber's `push-resource`, encrypted per **RFC 8291** (`aes128gcm`) with a
**VAPID** `Authorization` header per **RFC 8292**. Body:
```xml
<P:push-message xmlns:P="https://bitfire.at/webdav-push" xmlns:D="DAV:">
  <P:topic>O7M1nQ7cKkKTKsoS_j6Z3w</P:topic>
  <P:content-update><D:sync-token>http://sabre.io/ns/sync/42</D:sync-token></P:content-update>
</P:push-message>
```
Optional `Topic:` request header (coalesces undelivered messages) and `Urgency:`.

**d) Unsubscribe / expiry** — `DELETE` the subscription resource URL → `204`/`404`.
Server MUST NOT push to expired subscriptions.

**e) Self-change suppression (optional)** — a client may send `Push-Dont-Notify` on
writes to avoid being notified about its own changes. Nice-to-have, phase 3.

Spec references: RFC 8030 (Web Push), RFC 8291 (message encryption),
RFC 8292 (VAPID). We should **not** hand-roll this crypto — see §5.7.

---

## 3. Architecture / data flow

```
  WRITE PATH (unchanged choke point)          PUSH PATH (new)
  ─────────────────────────────────          ──────────────────────────────────
  PUT/DELETE object                           ┌── dispatch worker (goroutine) ──┐
    → storage.Create/Update/Delete            │  pgx LISTEN "dav_push"           │
      → addChange(tx, calID, uri, op)  ──┐    │  → debounce/coalesce per topic   │
          • INSERT calendarchanges       │    │  → load subscriptions for topic  │
          • UPDATE calendars.synctoken   │    │  → build push-message XML        │
          • pg_notify('dav_push', topic) ├───►│  → webpush.Send (RFC8291+VAPID)  │
      (all in one tx; fires on COMMIT)   │    │  → 404/410 ⇒ prune subscription  │
                                         ┘    └──────────────────────────────────┘
```

**Key design choice — trigger via Postgres `LISTEN/NOTIFY`:**

`addChange` (`internal/storage/sync.go`) and `abAddChange`
(`internal/storage/carddav.go`) are the *single* choke points every content change
already flows through (create=1/modify=2/delete=3, for both calendars and address
books). We add one line inside that same transaction:

```go
tx.Exec(`SELECT pg_notify('dav_push', $1)`, topicFor(collectionKind, collectionID))
```

`pg_notify` delivers **only on commit**, so notifications inherit the write's
transactional correctness (rollback ⇒ no push) for free — no lost or phantom
notifications, no outbox scanning, no added write latency. A dedicated worker holds a
`LISTEN dav_push` connection and reacts within milliseconds.

Why not the alternatives:
- *`AfterWriteHook` at the DAV layer* (`internal/dav/plugin.go`) fires after PUT but
  is not guaranteed for every DELETE/MKCOL path — `addChange` is the truer choke
  point and covers cal + card uniformly.
- *Polling an outbox table* works and is restart-durable, but trades latency for
  simplicity — the whole point here is low latency.
- *DB trigger on `calendars.synctoken`* is equivalent to the in-code `pg_notify` but
  hides logic in SQL; keep it in Go where it's greppable.

Restart durability: the worker on startup reconciles by comparing each
subscription's `last_synctoken` against the collection's current `synctoken` and
sends a catch-up notification where they differ, so a change during downtime isn't
lost.

---

## 4. New package: `internal/push`

A single new plugin package keeps this self-contained:

```
internal/push/
  plugin.go     // PushPlugin: FeatureProvider + PropFindHook + MethodProvider
  register.go   // POST push-register / DELETE subscription handlers
  topic.go      // stateless topic derivation (HMAC)
  worker.go     // LISTEN/NOTIFY dispatch loop + graceful shutdown
  sender.go     // Web Push (RFC 8291/8292) wrapper
  store.go      // push_subscriptions CRUD (pgx, mirrors storage/*.go)
  xml.go        // NSPush element (un)marshaling
```

---

## 5. Component breakdown (mapped to real extension points)

### 5.1 Config — `internal/config/config.go` + `config.toml.example`
Add a section (follows the existing typed-struct pattern; `config.go:16-88`):
```toml
[push]
enabled          = false   # master switch; when false the plugin isn't registered
vapid_subject    = "mailto:admin@example.com"   # RFC 8292 'sub' claim
vapid_public_key = ""      # base64url P-256; generated once, see §7
vapid_private_key = ""     # SECRET — base64url; keep out of VCS
topic_secret     = ""      # SECRET — HMAC key for opaque topic derivation
# base_url reuses [server].base_uri for absolute subscription Location URLs
```
Validation reuses the existing wrong-type rejection (commit `c09aa65`). If `enabled`
but keys are blank → fail closed at startup with a clear error (mirrors the
"fail closed when the ACL privilege checker is missing" stance, `625a377`).

### 5.2 Schema — `scripts/schema/pgsql.calserver.sql`
```sql
CREATE TABLE push_subscriptions (
    id               BIGSERIAL PRIMARY KEY,
    collection_kind  TEXT NOT NULL,          -- 'calendar' | 'addressbook'
    collection_id    BIGINT NOT NULL,
    principal_uri    TEXT NOT NULL,          -- who registered (for ACL re-checks)
    push_resource    TEXT NOT NULL,          -- Web Push endpoint
    p256dh           TEXT NOT NULL,          -- subscription public key
    auth_secret      TEXT NOT NULL,
    last_synctoken   BIGINT NOT NULL DEFAULT 0,
    expires          TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (collection_kind, collection_id, push_resource)   -- idempotent re-register
);
CREATE INDEX push_subscriptions_lookup
    ON push_subscriptions (collection_kind, collection_id) WHERE expires > now();
```
The `UNIQUE` gives us upsert-on-re-register (spec §b). A periodic
`DELETE … WHERE expires < now()` prunes dead rows (worker housekeeping tick).

⚠️ `last_synctoken` defaults to `0`, but the register handler (§5.5) MUST seed it to
the collection's *current* `synctoken` at insert time (and leave it untouched on an
idempotent re-register). If it stays `0`, the startup-reconcile sweep (§5.8) — which
notifies wherever `last_synctoken < synctoken` — will fire a spurious catch-up push at
the first worker tick after every fresh subscription. The `DEFAULT 0` is only a
fallback for rows created outside the handler.

### 5.3 Opaque topic — `internal/push/topic.go`
Stateless, stable, non-enumerable — no schema change to `calendars`/`addressbooks`.
Two consistency points with the rest of the plan:
- **IDs are strings on the write path.** `addChange`/`abAddChange` carry
  `calendarID`/`addressBookID` as `string` (the numeric PK, stringified — see
  `storage/sync.go:120`, `carddav.go:330`), and §5.6's `pg_notify` payload is
  `fmt.Sprintf("calendar:%s", calendarID)`. Keep `Topic` on the same type so the
  advertised topic and the worker's derivation can't drift: take a `string` id (or a
  single `"kind:id"` key), not `int64`.
- **The secret is injected, not a package global.** `topic.go` stays a pure function;
  the HMAC key comes in as a parameter (or the plugin binds it via a closure/method
  receiver at construction from `cfg.TopicSecret`). No hidden `cfg` reference.
```go
// derive binds the configured secret once (e.g. as a PushPlugin method):
func (p *PushPlugin) Topic(kind, id string) string {
    mac := hmac.New(sha256.New, p.topicSecret)
    fmt.Fprintf(mac, "%s:%s", kind, id)
    return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
```
The reverse map (topic → collection) needed by the worker is recovered by indexing
subscriptions on the topic, or by carrying the collection key in the `pg_notify`
payload directly (simpler — see §5.6).

### 5.4 Advertisement — `internal/push/plugin.go`
Register the plugin in `compose()` (`internal/server/server.go:163-297`) **only when
`push.enabled`**. ⚠️ Registration order is load-bearing for the POST handler, not
just cosmetic: the plugin must be added **before `caldav.SharingPlugin`** (currently
`server.go:259`), i.e. right after `srv.AddPlugin(&caldav.Plugin{})` at `server.go:244`.
See §5.5 for why.
- Implement `FeatureProvider` (`dav/plugin.go:23`) → add `"webdav-push"` to the `DAV:`
  header.
- Implement `PropFindHook` (`dav/plugin.go:41`): in `PropFind(pf, node)`, when
  `node` is `*caldav.Calendar` or `*carddav.AddressBook`, `pf.Handle(...)` the three
  properties from §2a. This mirrors exactly how `caldav/plugin.go:59-98` adds
  `supported-calendar-data` etc. The VAPID public key and computed `topic` come from
  config + `topic.go`.

### 5.5 Registration & unsubscribe — `internal/push/register.go`
Implement `MethodProvider` (`dav/plugin.go:30-37`), dispatched by the loop in
`dav/server.go:245-258` (each `MethodProvider` is offered the request in registration
order until one returns `handled=true`).

**POST on a collection — body handling is the subtle part.** Peek the request-body
root element; if it is `{NSPush}push-register`, handle it and return `handled=true`,
otherwise return `handled=false` so the existing collection-POST handlers still run.
Two facts about the current code make the naive version wrong:
- **POST has no shared body buffer.** Unlike REPORT — where `dav/core.go:85` reads the
  body once into `[]byte` and passes the same slice to every `ReportHandler` — each
  `MethodProvider.HandleMethod` reads `r.Body` itself. There is nothing to "extend."
- **`readBody` (`caldav/sharing.go:418`) consumes *and closes* `r.Body` with no
  rewind.** `caldav.SharingPlugin` is the other plugin that peeks the root element of a
  collection POST (`share`, `invite-reply`, …). It gets away with not restoring the
  body only because it is effectively the terminal XML-POST handler today.

So the design is:
- Register the push plugin **before `SharingPlugin`** (§5.4). For a bare-collection
  POST the order is then `core (POST→false, no read) → caldav (add-member;
  `plugin_writes.go:105` returns false *without* reading the body for non-`;add-member`
  paths) → push → schedule → sharing`.
- The push handler reads the body once (mirroring `readBody`), and **on pass-through
  restores it** so `SharingPlugin` (and any later handler) still sees an intact stream:
  ```go
  body, _ := io.ReadAll(r.Body)
  r.Body.Close()
  if rootElementName(body) != pushRegister {
      r.Body = io.NopCloser(bytes.NewReader(body)) // hand the untouched body onward
      return false, nil
  }
  ```
- Upsert into `push_subscriptions` (unique key = kind+id+push_resource), choose
  `expires` (e.g. `now()+7d`, ≥ spec's 3d floor), respond `201` + `Location:` +
  `Expires:`. `Location` = `<base_uri>/push/subscriptions/<id>`.

**DELETE of a subscription — do NOT model it as a bare `Methods(path)` claim.**
`Methods()` only populates the `Allow` header; it has **no effect on dispatch order.**
`CorePlugin` is registered first (`dav/server.go:79`) and its `HandleMethod` claims
DELETE unconditionally (`core.go:56`), calling `Tree.Delete(path)` and returning
`handled=true` before any later plugin runs. A plain `DELETE /push/subscriptions/<id>`
would therefore be intercepted by core, hit `Tree.Delete` on a non-existent node, and
404 — the push plugin never sees it. `BeforeUnbindHook` can't rescue this: it fires
*inside* `core.delete`, can only return an error (not "handled"), and core still
proceeds to `Tree.Delete`.

Make subscription resources **real tree nodes** instead, so unsubscribe flows through
the existing DELETE path: expose a small read-only collection at `/push/subscriptions/`
whose children resolve to subscription rows, and prune the row from an
`AfterUnbindHook`/`BeforeUnbindHook` (`dav/plugin.go:63,101`) that matches the
`/push/subscriptions/` prefix. `Tree.Delete` then succeeds, core returns `204`
naturally, and a missing/expired id yields the normal `404`. (Alternative if a tree
node is too heavy: carve the `/push/subscriptions/` prefix out *ahead of* `CorePlugin`
via a `BeforeMethodHook` that handles the DELETE and stops processing — but the
tree-node route fits the existing model and is preferred.)

**ACL gate — two call sites, only one of which has an HTTP request.** A subscription
is bound to `principal_uri`, and both registration and every later dispatch must
confirm that principal can still read the collection (privacy invariant, cf.
`7b6d9cd`). The shared primitive is `RequirePrivilegeChecker(srv)` →
`CheckPrivileges(ctx, path, "{DAV:}read")` (the checker that `625a377` made
fail-closed, `caldav/plugin_writes.go:122`) — but it reads the *authenticated
principal off `ctx`*, so the two call sites differ:
- **At registration** (`POST`, §5.5): call it with the live `r.Context()`, exactly as
  `handleAddMember` does for `{DAV:}bind`. The request's authenticated principal is
  already in context; this is a drop-in reuse. Do **not** reach for the ACL plugin's
  `BeforeMethod` here — POST isn't in its precondition table (that's precisely why
  `handleAddMember` enforces privileges inline), so the push handler must call the
  checker itself.
- **At worker dispatch** (§5.8): there is no request and no `ctx` principal. The worker
  must synthesize a context that carries `principal_uri` as the authenticated principal
  (the same state auth installs on a real request via `withPrincipalState`, see
  `dav/server.go:218`) before calling `CheckPrivileges`, then skip/prune the
  subscription on denial. This is a small helper, not a drop-in — call it out as work.

### 5.6 Trigger — one line in `internal/storage/sync.go` & `carddav.go`
Inside `addChange(tx, calendarID, objectURI, op)` (`sync.go:120-134`), after the
`calendarchanges` INSERT and `synctoken` UPDATE, within the same tx:
```go
_, err = tx.Exec(`SELECT pg_notify('dav_push', $1)`,
    fmt.Sprintf("calendar:%s", calendarID))
```
Mirror in `abAddChange` (`carddav.go:330-343`) with `addressbook:%s`. Payload carries
the collection key directly so the worker needn't reverse a topic. This is the only
change outside the new package, and it's additive + transaction-scoped.

### 5.7 Sender — `internal/push/sender.go`
Wrap a vetted library rather than implementing RFC 8291/8292 by hand. Candidate:
`github.com/SherClockHolmes/webpush-go` (handles aes128gcm encryption + VAPID ES256
JWT). Add to `go.mod`. `Send(sub, plaintextXML)` sets `Content-Type:
application/xml`, `Urgency`, optional `Topic` header (= our topic, for coalescing).
Handle responses:
- `201/202` → ok; bump `last_synctoken`.
- `404/410` (endpoint gone) → **prune** the subscription.
- `429`/`5xx` → honor `Retry-After`, bounded retry with backoff.

### 5.8 Worker & lifecycle — `internal/push/worker.go` + `cmd/calendsd/main.go`
- Uses a **dedicated `pgx.Conn`** (not the `database/sql` pool from
  `storage/postgres.go`) because `LISTEN` needs a pinned connection; loop on
  `conn.WaitForNotification(ctx)`.
- **Coalesce**: buffer notifications per topic over a short window (e.g. 300–500 ms)
  so a bulk import sends one push per collection, not thousands.
- On each coalesced topic: load current `synctoken`, load live subscriptions, fan out
  via a small bounded worker pool (reuse the connection-pool-bounding instinct from
  `a1e952a`), send, update `last_synctoken`, prune dead endpoints.
- **Startup reconcile**: sweep subscriptions where `last_synctoken < collection
  synctoken` and notify (covers changes during downtime).
- **Housekeeping tick**: periodic `DELETE FROM push_subscriptions WHERE expires <
  now()`.
- Spawn as `go pushWorker.Run(ctx)` in `main()` next to `serveMetrics`
  (`cmd/calendsd/main.go:121-141`). Add a `context.WithCancel` + signal handler for
  graceful shutdown — currently `main.go` has none, so introduce a minimal one.

---

## 6. Reliability, security & privacy

- **Encryption/authenticity**: RFC 8291 encrypts message bodies to the subscriber's
  key; RFC 8292 VAPID proves *we* are the sender. Push service never sees plaintext.
- **Minimize payload**: send only topic + sync-token. A client that misses/decodes
  nothing still recovers via a normal sync (push is a hint, not a source of truth).
- **ACL on register AND on send**: only notify a principal that can still read the
  collection. Re-check at dispatch, prune on loss. Aligns with the ACL-before-
  preconditions privacy invariant (`7b6d9cd`).
- **Secrets**: `vapid_private_key` + `topic_secret` are secrets — document keeping
  them out of VCS (env/`config.toml` perms), like existing tokens in config.
- **Abuse bounds**: cap subscriptions per principal/collection; TTL + pruning stop
  unbounded growth; SSRF note — `push_resource` is attacker-influenced, so the sender
  should restrict schemes to https and (optionally) disallow internal IP ranges.
- **VAPID key rotation**: spec supports a `property-update` push telling clients to
  re-register; low priority, note for later.

---

## 7. Testing

- **Unit**: topic determinism/opacity; push-register XML parse/upsert; expiry &
  prune logic; POST dispatch correctly *passes through* non-push POST bodies.
- **Integration (DB)**: write an object → assert a `dav_push` NOTIFY fires on commit
  and not on rollback; worker turns it into exactly one send per collection under
  burst (coalescing).
- **Sender**: mock Web Push endpoint; assert encryption headers, VAPID JWT, and
  404/410 pruning.
- **End-to-end**: DAVx⁵ ≥ 4.4.2 against a dev instance (its WebDAV-Push flow is the
  conformance target). The vendored sabre reference has a `Davx5Test.php` documenting
  DAVx⁵'s discovery/sync shape — useful for cross-checking property output.
- **Web client**: Service Worker registration + a visible "instant update" path,
  validated with the existing puppeteer harness (see memory:
  `calweb-ui-testing-via-puppeteer`).

Generate a VAPID keypair once for dev (e.g. `webpush-go`'s `GenerateVAPIDKeys`,
or `web-push generate-vapid-keys`) and put it in `config.toml`.

---

## 8. Milestones

1. **Advertise (no delivery).** Config + `PushPlugin` FeatureProvider/PropFindHook +
   topic derivation. Clients discover support; nothing is sent yet. Fully testable
   via PROPFIND.
2. **Register/unsubscribe.** `push_subscriptions` schema + POST/DELETE handlers +
   ACL gate. Subscriptions persist; still no delivery.
3. **Deliver.** `pg_notify` trigger + worker + sender. End-to-end DAVx⁵ push.
4. **Web client.** Service Worker + subscription UI in calclient; replace/augment the
   30 s poll.
5. **Polish.** Coalescing tuning, self-change suppression (`Push-Dont-Notify`),
   VAPID rotation, metrics (`internal/metrics`: subscriptions gauge, sends/failures
   counters, dispatch latency histogram).

Milestones 1–3 are the core (DAVx⁵ works after 3). Estimated ~2–4 focused days for
1–3 given the choke points are already clean, plus the web-client work in 4.

---

## 9. Open decisions

- **Library vs. hand-rolled crypto** for RFC 8291/8292 — recommend the library.
- **Coalescing window** length (latency vs. burst amplification) — start 300 ms.
- **Subscription TTL** — 7 days (≥ 3-day floor), refreshed on re-register.
- **Web-client transport** — real Web Push via a public push service, or a simpler
  in-app SSE/WebSocket for same-origin only? WebDAV-Push assumes Web Push; SSE would
  be a separate, non-standard path just for our web UI.
- Whether to also emit `property-update` triggers (display-name/ACL changes on
  principals) or ship `content-update` only first (recommended: content-update only).

## 10. Explicit non-goals

- iPhone / macOS Calendar push (unsupported by those clients; unchanged by this work).
- Replacing polling entirely — push is a low-latency hint; `sync-collection` remains
  the source of truth and the fallback.
