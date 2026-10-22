# Durable Job Queue (DB-backed, Oban-shaped)

> **Status: implemented.** A single durable queue backs **all** background iMIP
> sends: relay-native sends (resource replies, server-originated mail, every send
> in a submission-less deployment) carry no credential; per-user submit-as-user
> sends carry the user's submission password **encrypted at rest** (config-provided
> key), kept only while the send is unresolved (plus a TTL-bounded tail after a
> give-up, for manual resend), so the send is off the request path, **metered**,
> and survives a restart (D5/D12). Prompted by the restart-durability gap in the
> off-request-path delivery work: a transiently-failed queued send was lost on
> process restart, leaving an attendee stuck at `SCHEDULE-STATUS=1.0`.
>
> **Single tier, no RAM queue.** An earlier draft kept an in-memory first-attempt
> tier so a *successful* per-user send never touched disk with the credential;
> that tier was removed (see D5) once we saw its only benefit — fewer encrypted
> rows at rest — evaporates under the real threat model (a DB-only leak yields
> undecryptable ciphertext either way; a DB+key leak loses under both). It bought
> a whole in-memory subsystem's worth of complexity for a margin that doesn't
> exist. Enqueue happens on the request path; the send runs in a worker.
>
> **This reverses** `IMIP-SEND-AS-USER-PLAN.md`'s "credential is never persisted"
> constraint — deliberately and narrowly; see D12.

## Goal

Make queued iMIP work **survive a process restart** while keeping:

1. **Off the request path.** DELETE/PUT must not block on SMTP.
2. **Per-user outbound metering.** A user's invitations count against their own
   mizu_out/rspamd budget — the abuse control `IMIP-SEND-AS-USER-PLAN.md` exists
   for. Durable per-user retries must therefore still authenticate *as the user*,
   which is why the credential has to be recoverable after the request ends.
3. **Encrypted, time-bounded credential at rest.** The submission password is
   never stored in plaintext, never in `args`, is encrypted with an out-of-band
   key, is erased the instant the send succeeds, and is TTL-bounded otherwise
   (retained only until the purge window so a manual resend can reuse it — D12).
   This is the deliberate, scoped reversal of the old "never persisted" rule.

Secondary goal: **don't foreclose a future move to Elixir/[Oban](https://hexdocs.pm/oban)** —
shape the table/abstraction so an Oban migration is a mechanical mapping.

## Background / current state

**The in-memory queue** (`SendQueue`) retries transiently-failed iMIP sends in a
worker pool with backoff. Its terminal callback drives the PUT write-back that
patches `SCHEDULE-STATUS`. It is RAM-only: a queued closure holds the user's
submission credential. On restart, whatever is pending is dropped — a PUT
invitation is then stuck at `1.0;pending`.

**Two send identities** in `internal/caldav/imip.go`:

- `deliverAsUser` → `SubmissionMailer.SendAs(username, password, …)` — the user's
  credential, **metered**. Needs the password.
- `deliverViaRelay` → `Mailer.Send(SenderEmail, …)` — the server identity,
  **unmetered**, no per-user secret. Used for server mail, resource replies, and
  every send when `Submission` is nil.

**Why we do not just relay durable retries** (the option we rejected): a per-user
send over the relay is unmetered, and it would fire on exactly the 4xx
rate-limit rejection — delivering the send *around* the limit that rejected it.
A compromised account would trip its limit once, then relay everything free.
Metering becomes decorative in the abuse case. So durable per-user retries must
still `SendAs` the user — hence the encrypted credential (D12).

**Patterns reused:** additive extension tables (`scripts/schema/pgsql.calserver.sql`);
timestamp-scheduled sweeps (`deleted_principals.purge_after`); `LISTEN`/`NOTIFY`
+ worker (`internal/push`); read-only scans (`internal/storage/admindoctor.go`).

## Decisions

- **D1 — One general `jobs` table.** Generic `worker`+`args`+`queue`+`state` is
  more Oban-compatible than an `imip_sends` table and gives one home for future
  work. iMIP send is the first worker.

- **D2 — Design for swappability, don't adopt `oban_jobs` verbatim.** Narrow Go
  interface (`Enqueue`/`Register`); Oban-shaped columns and state names so a later
  migration is a column mapping. No Oban claim-protocol/notify/version coupling in
  Go now.

- **D3 — The table carries every iMIP send; the *credential* is a separate,
  encrypted, nullable column — never in `args`.** Relay-native jobs store no
  credential. Per-user jobs store the AEAD-encrypted submission password in
  `enc_credential` (D12), so `args` stays inspectable and secret-free (D8). A job
  is a per-user job iff `enc_credential IS NOT NULL`.

- **D4 — Persist the fully-rendered RFC822 body in `args` (base64), not the
  inputs to re-derive it.** The worker SMTP-sends the bytes; no MIME/iCal framing
  in the worker, and a future Elixir worker re-sends the same bytes. (Multi-KB
  rows; relevant to pruning.)

- **D5 — Per-user sends are durable *and* metered via a single encrypted-credential
  durable tier.** On the request path, a per-user send is enqueued with the
  submission password encrypted at rest (D12) and delivered by a worker that
  decrypts it and `SendAs` the user — off the request path, metered, restart-
  surviving. There is **no RAM tier**.
  - **Metered by construction.** The worker always `SendAs` the user, never a
    relay. A rate-limit 4xx keeps hitting the *user's own* limit each retry and
    eventually discards (respecting the limit), rather than routing around it.
  - **Per-user SMTP rejections are classified** (D11/D13): a 5xx (incl. a `535`
    from a now-stale password) discards immediately; a 4xx/greylist retries.
  - **No credential key, or no durable queue → synchronous send.** Without a key
    (`EncryptsCredentials` false) the credential can't be persisted, so the send
    runs synchronously on the request path (the pre-queue behavior: it blocks on
    SMTP but still delivers or fails terminally). The in-memory server (no DB) is
    likewise synchronous. In every real (Postgres) deployment with a key, per-user
    sends are durable and off-path.
  - **Why one tier, not two (the reversal).** An earlier draft kept a RAM first
    attempt so a *successful* send never persisted the credential, and only
    *failing* sends held one at rest. That extra tier — a whole in-memory queue,
    a cross-goroutine handoff, and the `msg`-sharing concurrency it dragged in —
    bought nothing under the actual threat model: a **DB-only leak** yields
    AEAD ciphertext the attacker can't decrypt *whether it's a few rows or many*
    (the key lives out-of-band); a **DB+key leak** loses under both designs (D12
    already concedes it). The "fewer rows at rest" margin only exists in the
    key-compromised case that's already lost. So we persist the encrypted
    credential for every in-flight per-user send (NULLed the instant it succeeds —
    D12) and delete the tier.

- **D5b — Recovery, in two tiers by whether a usable stored credential survives.**
  The stored `SCHEDULE-STATUS=1.0;pending` is always the durable record; a
  reconciliation scan (admindoctor-style) surfaces attendees stuck at `1.0`
  (metric + optional client "delivery pending" flag). How they recover turns on
  whether the job still holds a decryptable `enc_credential`:
  - **Credential retained (the common case) → `calctl imip resend`, still metered.**
    calctl runs on the server host, so it holds both the DB ciphertext *and* the
    config key: it decrypts `enc_credential` and re-attempts `SendAs` the user — a
    fully metered resend, no relay, no bypass. Mechanically it just resets the job
    to due-now (`state='available'`, `scheduled_at=now()`, and for a given-up job
    clears the attempt count) and lets the worker pick it up — an *immediate manual
    retry* of a job whose backoff hasn't fired yet or has exhausted. For this to
    reach give-up cases, `enc_credential` is **retained on `discarded` until the
    TTL purge** (D12), not NULLed the instant a job gives up.
  - **No usable credential (no key configured; credential TTL-purged; key rotated
    out) → the organizer's live session, or a logged override.** Here nothing on
    the server can send *as the user*: recovery is the organizer's client re-issuing
    the invite (their live request carries the password, so it is metered), or an
    explicit, audit-logged `calctl imip resend --relay` that sends via the server
    identity **unmetered** — a deliberate, recorded exception, never the automatic
    path. A plain re-save of an *unchanged* event won't resend (no significant
    change), so the client affordance must force a resend.

- **D6 — Ship the iMIP worker; do not big-bang migrate push/purge.** The table
  being *capable* of holding future work is the win; forcing existing subsystems
  onto it is scope creep.

- **D7 — Minimal columns first.** No priorities/cron yet. The additions this plan
  needs are the credential columns (D12) and the coalesce index (D10) — nothing else.

- **D8 — `args` hygiene is a standing invariant: no secret in `args`, ever.** The
  credential lives only in the dedicated encrypted column (D3/D12). Stated in the
  package doc so the next worker author can't reintroduce the hazard.

- **D9 — Claim-then-release + orphan-reclaim sweep.** The claim marks a row
  `executing` and commits immediately (row lock held for microseconds, not the
  send); the handler runs SMTP *outside* any open txn (never pin a pool connection
  across the mailer timeout). A periodic reclaim sweep resets `executing` rows
  whose `attempted_at` is older than `reclaim_after` back to `available` (bounded
  by `max_attempts`) — this is what makes an in-flight-at-crash job survive. Oban's
  rescue mechanism, minimal.

- **D10 — At-least-once, one job per recipient, deduplicated at enqueue.**
  - **One job = one recipient.** The broker already emits one message per attendee
    (`Send(from, []string{recipient}, …)`), so a retry re-sends only to that one
    recipient — never re-blasts a list on a partial failure. The table keeps this:
    one row per recipient.
  - **At-least-once, not exactly-once.** A worker that gets the SMTP `250` then
    dies before writing `completed` will re-send on reclaim (D9). This is inherent
    — an SMTP send and a DB commit can't be one atomic transaction. Bounded
    (`reclaim_after` > mailer timeout) and made harmless because a duplicate
    `REQUEST`/`CANCEL` with the same `UID`+`SEQUENCE` is **idempotent at the
    recipient's calendar** (it re-applies the same state — a second email, not a
    second event). A lost invite is worse than a duplicate.
  - **Coalesce per `(uid, recipient, method)` by UPSERT-supersede — not a reject
    index, and not keyed on SEQUENCE.** For a **REQUEST**, SEQUENCE is the
    *organizer client's* to increment (RFC 5545 §3.8.7.4): this server, like sabre,
    decides significance by its own property hash (`itip.go` `sigHash`) and echoes
    the client's REQUEST SEQUENCE verbatim (bumping it server-side would desync the
    stored event from what attendees hold and break REPLY matching). So a REQUEST's
    SEQUENCE is present-but-unreliable (absent, stuck at `0`, or non-monotonic across
    clients) and unsafe as a uniqueness component. For a **CANCEL** the server
    *does* now increment it (`incSequence` in `itip.go`, RFC 5546 — a cancellation
    is a significant change, and a CANCEL must beat the REQUEST it supersedes even
    for recipients that collate on SEQUENCE alone), so a server-generated CANCEL is
    monotonic — but a REQUEST still isn't, which is why we sort on `(SEQUENCE,
    DTSTAMP)` and never on SEQUENCE alone. So: a partial unique index on
    `(uid, recipient, method)` over `state='available'`, with
    `ON CONFLICT DO UPDATE`. A **newer** revision — higher `(SEQUENCE, DTSTAMP)`,
    RFC 5546's own collation order — replaces the pending job's body and reschedules
    it; an **older/equal** arrival (the re-issued PUT or double-fired
    `InterceptWrite`) is a no-op. This dedups exact re-issues *and* lets a genuine
    update supersede a stalled retry — which a reject index would get backwards,
    dropping the correct update and continuing to deliver the stale one. `SEQUENCE`
    and `DTSTAMP` ride in `args` purely as this ordering tiebreaker, never as
    identity, so correctness no longer depends on the client having bumped SEQUENCE.
    The index is scoped to `available` so a supersede that races an already-
    `executing` send simply inserts fresh: the attendee gets the stale send then the
    correction — harmless under the at-least-once/idempotency point above. (Keeping
    `method` in the key is a v1 choice; coalescing across methods on
    `(uid, recipient)` so a CANCEL supersedes a pending REQUEST is a later
    refinement.)

- **D11 — Handler distinguishes complete / reschedule / discard.** `Handler`
  returns `nil` (complete), `Discard(err)` (terminal now — permanent failure), or
  a plain error (reschedule with backoff to `max_attempts`). This preserves the
  sync path's `5.3`/`5.1` status fidelity and avoids retrying a permanent failure.

- **D12 — Credential encryption.** (New — the crux of the metering-preserving
  durability.)
  - **What:** the user's submission password, encrypted with AEAD (AES-256-GCM or
    nacl/secretbox), per-row random nonce, `key_id` for rotation.
  - **Key:** a random 256-bit key from **config** (env override), held out-of-band
    — never in Postgres, and excluded from DB backups (that exclusion is the whole
    security value against the common DB-at-rest leak). The feature is **gated on
    the key**: no key → the per-user durable path is off and D5b applies. Never
    store plaintext.
  - **Lifecycle:** written at enqueue for every per-user send (single tier, D5).
    `enc_credential` is NULLed in the settle txn that marks a job `completed`
    (success — never needed again; the common case, so most credentials live only
    the sub-second-to-seconds until the first attempt succeeds). While a job is
    still retrying it is **retained** — the send genuinely still needs it. The
    purge sweep **tracks the job's schedule, not a fixed age from insert**: because
    each transient failure pushes `scheduled_at` forward by the (lengthening)
    backoff, a live retry's credential lifetime grows with its schedule and is
    never purged out from under it. It is dropped only when the send **gave up**
    (`discarded`) more than a grace window ago — so `calctl imip resend` (D5b) can
    drive a metered manual retry of a given-up send first — or when an `available`
    job is overdue past that grace (a stuck/abandoned row).
  - **Fail-safe:** a missing/undecryptable key → discard that job + D5b surface;
    never fall back to an unmetered relay.
  - **Rotation keyring:** the `Keyring` supports a current key plus older keys
    (accepted for decryption only), and `key_id` per row selects the decrypting
    key, so a rotation needn't strand in-flight retries; a `key_id` no longer in
    the ring is undecryptable → fail-safe (discard + D5b). **Wiring status:** the
    server currently loads a single key (`key_id` 1) from config — multi-key
    rotation config (a list of prior keys) is a small follow-up, not yet wired.
    Until then, rotating the key strands credentials written under the old one;
    they fail-safe within the grace window.
  - **Scope.** The encrypted secret is the user's *mailbox* password, so D12's value
    hinges on the key being out-of-band and out of DB backups: on a single host a
    full host compromise defeats it (the same posture the DB credentials already
    have), but it protects the common case — a leaked DB dump/snapshot/read-only
    injection. Why this beats a standing service credential, and the scoped-token
    upgrade that would beat *it*: see **Alternatives considered**.

- **D13 — SMTP result handling.** Classify on the reply code, per command:
  - **2xx** (the `250` after `DATA`) = accepted → complete; never retried.
  - **4xx** (greylist, temp policy, rate limit) = transient → reschedule.
  - **5xx** = permanent → discard (a `550`/`553` bad recipient, or a `530/534/535`
    auth failure incl. a stale stored password).
  - **Classify on the code class, not command context.** `transact` returns a
    single wrapped error, so the handler keys on the class of the embedded
    `textproto.Error` (5xx vs 4xx) plus `net.Error` — exactly as today's
    `classifySendError`/`retryableSend`. A `550` and a `450` already differ by
    class, so per-command context is neither available nor needed.
  - **Post-`DATA` guard (keep the existing behavior):** once `DATA` gets its `250`
    the message is accepted; a later failure (e.g. `QUIT`) is **not** reported as
    non-delivery, so we don't mark an accepted message "not sent" and resend it.
    The only residual ambiguity is a connection drop *during* the `250` read →
    treated as a network error → retried (at-least-once, D10), never assumed-sent.

## Schema

Additive table in `scripts/schema/pgsql.calserver.sql`. Per-user durability needs
**no separate table** — the credential is columns on the job, nulled on terminal.

```sql
-- Durable background-job queue. NEW calendsd-only table; a co-resident sabre
-- never references it. Oban-shaped (see JOB-QUEUE-PLAN.md).
-- INVARIANTS: (1) args is inspectable JSONB and MUST NOT contain a secret (D8);
-- the submission credential lives ONLY in enc_credential, encrypted (D12).
-- (2) enc_credential is set only for a failing per-user send: NULLed on
-- 'completed', retained until the TTL purge on 'discarded' (manual resend, D5b/D12).
CREATE TABLE jobs (
    id             BIGSERIAL   PRIMARY KEY,
    queue          TEXT        NOT NULL DEFAULT 'default',
    worker         TEXT        NOT NULL,               -- "imip_send"
    args           JSONB       NOT NULL,               -- rendered body + uid/recipient/method/sequence/dtstamp/object_path; NO secret
    state          TEXT        NOT NULL DEFAULT 'available', -- available|executing|completed|discarded
    attempt        INT         NOT NULL DEFAULT 0,
    max_attempts   INT         NOT NULL DEFAULT 6,
    scheduled_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempted_at   TIMESTAMPTZ,                        -- current 'executing' claim start (D9 reclaim)
    last_error     TEXT,
    -- Per-user submit-as-user credential, AEAD-encrypted (D12). NULL for relay
    -- jobs and for 'completed' sends; retained until TTL purge on 'discarded'.
    enc_credential BYTEA,
    enc_nonce      BYTEA,
    key_id         SMALLINT,
    inserted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at   TIMESTAMPTZ
);

CREATE INDEX jobs_claim_ix   ON jobs (queue, state, scheduled_at);
CREATE INDEX jobs_reclaim_ix ON jobs (state, attempted_at);

-- Coalesce: at most one pending job per (uid, recipient, method). A newer revision
-- supersedes via ON CONFLICT DO UPDATE, ordered by (SEQUENCE, DTSTAMP) from args
-- (D10). A REQUEST's SEQUENCE is client-controlled (echoed, may be non-monotonic);
-- a server-generated CANCEL's is incremented (monotonic). So SEQUENCE is an
-- ordering key with DTSTAMP as tiebreaker, never the identity. Scoped to
-- 'available' so a supersede racing an in-flight send inserts fresh.
CREATE UNIQUE INDEX jobs_coalesce_ix ON jobs (
    (args->>'uid'), (args->>'recipient'), (args->>'method')
) WHERE state = 'available';
```

Notes: `state` is TEXT (a `CHECK (state IN …)` is an optional typo-guard);
`object_path` in `args` is nullable (resource replies / server mail don't write
back); completed rows are pruned (step 7).

## Go design

New package `internal/jobs`.

```go
type Job struct {
    ID          int64
    Queue       string
    Worker      string
    Args        json.RawMessage
    Attempt     int
    MaxAttempts int
    Credential  []byte // decrypted submission password, or nil for relay jobs (D12)
}

// nil → complete; Discard(err) → terminal now; other error → reschedule (D11).
type Handler func(ctx context.Context, j Job) error
func Discard(err error) error

type Queue struct { /* *sql.DB, registry, workers, backoff, notify, aead */ }
func (q *Queue) Register(worker string, h Handler)
func (q *Queue) Enqueue(ctx context.Context, worker string, args any, opts ...Opt) error // opts incl. WithCredential([]byte)
func (q *Queue) Start(ctx context.Context) // claim loop + LISTEN + reclaim/TTL/prune sweeps
```

The queue owns the AEAD: `Enqueue(..., WithCredential(pw))` encrypts before
insert; the claim decrypts into `Job.Credential` before calling the handler and
zeroes it after. Handlers never see ciphertext or the key.

Claim-then-release (D9): `UPDATE … SET state='executing', attempt=attempt+1,
attempted_at=now() WHERE id = (SELECT id … WHERE state='available' AND
scheduled_at<=now() ORDER BY scheduled_at FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING …`; commit; run handler with no open txn; settle in a second txn
(complete/reschedule/discard; `enc_credential=NULL` on `completed`, retained until
the TTL purge on `discarded` so a manual `calctl imip resend` can reuse it — D5b/
D12). Reclaim sweep:
`state='executing' AND attempted_at < now()-reclaim_after → available` (or
`discarded` past `max_attempts`); `reclaim_after` must exceed the mailer timeout.

## The iMIP worker

- **Args**: `{ uid, recipient, method, sequence, dtstamp, outcome, object_path?,
  message_b64 }` — no secret. `sequence`/`dtstamp` are pulled from the VEVENT at
  enqueue and used only to order supersedes (D10); `message_b64` is already framed
  for the chosen identity.
- **Send**: `Job.Credential != nil` → `Submission.SendAs(username, cred, …)`
  (metered); else → `Mailer.Send(SenderEmail, …)` (relay). Username travels in
  `args` (it's the public login, not a secret); the body is already framed for the
  chosen identity at enqueue.
- **Result** (D13): 2xx → complete + (if `object_path`) `patchScheduleStatus(…sent)`;
  4xx/network → return error (reschedule); 5xx → `patchScheduleStatus(…classified
  5.x)` + `Discard`.
- **Enqueue points** (one `enqueueDurable`, split only by identity): a per-user
  send → `Enqueue(…, WithCredential)`; a relay send → `Enqueue` (no credential).
  Both run on the request path. No credential key (or no queue) → a synchronous
  send instead (D5).

## Write-back concurrency

The terminal `SCHEDULE-STATUS` is written back by the durable worker (on delivery
or discard) or, for a send that resolved synchronously/inline, on the request
goroutine. Multiple workers can patch **different attendees of the same object**
concurrently, so the read-modify-write is serialized per object through a
`WriteBackLocker` interface on `SchedulePlugin`:

- **Single node** (no locker wired): the in-process striped `objectLocks`.
- **Multi node** (`AdvisoryLocker`, wired in `compose`): a Postgres **session
  advisory lock** (`pg_advisory_lock`/`_unlock`) keyed on the object path, layered
  over the same in-process stripe. The advisory lock is the mutex — held only for
  the brief read-modify-write — while the read/write still go through the DAV tree
  on pooled connections, so no etag/synctoken plumbing is re-implemented.

This is deliberately **not** a `SELECT … FOR UPDATE` on the `calendarobjects` row
(which would mean bypassing the tree and re-deriving etag/synctoken): the concern
is serializing *write-backs* across nodes, and the advisory lock does exactly that
with far less surface. It's best-effort — a DB blip drops to the in-process lock
(a stale status is cosmetic; the send already happened). Tested cross-instance in
`writeback_lock_test.go`.

## Operator CLI (`calctl jobs`)

**Status: implemented** (`internal/jobs/ops.go`, `cmd/calctl/jobs.go`).

The durable table is worker-agnostic (D1), so the low-level tooling is a generic
`calctl jobs` group that operates on rows by id; the iMIP-semantic recovery
(`calctl imip pending`/`resend`, D5b) sits above it. **Security property: the CLI
manipulates queue *state* only — it never handles the credential or the
encryption key.** It redacts `enc_credential` on display (presence + `key_id`
only) and cannot set it; a retry just resets the row and the **worker** remains
the only component that decrypts and sends. So the ops tooling needs DB access
but not the key, and no secret ever reaches a terminal or shell history — the
`Ops` layer's SQL never selects `enc_credential`, and a test greps a marshalled
job to prove the plaintext can't leak.

**`imip resend --relay`** (implemented) is the unmetered override for a per-user
job whose credential is gone: `Ops.RelayOverride` drops the credential and
re-queues, and the worker relays the (user-framed) body via the server envelope —
a split sender, the deliberate DMARC-risk exception the operator opts into. The
default `imip resend` stays metered and *refuses* a credential-less per-user job.

**Reconciliation scan** (implemented, on-demand) lives in `calctl doctor` rather
than as a per-scrape metric — the `imip-pending-stale` check piggybacks on the
existing deep object scan and flags objects stuck at `SCHEDULE-STATUS=1.0` whose
UID has **no** live `imip_send` job (so freshly-queued sends aren't false
positives). A per-scrape metric was rejected: it would seq-scan `calendarobjects`
every 15s. The `calserver_jobs_total{state}` gauge covers the job-level view.

**List / show (read-only):**

- `calctl jobs list [--state S] [--worker W] [--queue Q] [--recipient R]
  [--failed] [--limit N]` — a table: `id`, `state`, `worker`, `recipient` (from
  `args`), `attempt/max`, `scheduled_at` (+ age), `cred?` (yes/no — *presence
  only*), truncated `last_error`. `--failed` is the common operator view
  (`discarded`, or `available` with `attempt>0`).
- `calctl jobs show <id>` — one job in full: every column **except** the secret;
  `args` pretty-printed (uid, recipient, method, subject, body size); the
  credential shown as `present (key_id=N)` or `none`, never decrypted.
  `--full-body` dumps the framed RFC822 (attendee-visible event content — not a
  secret, but gated so it's off by default).

**Edit / act (guarded mutations):**

- `calctl jobs retry <id>` — reset to `available`, `scheduled_at=now()`, clear the
  attempt count: an immediate manual retry of a backed-off or `discarded` job. For
  a per-user job it relies on a still-decryptable `enc_credential`; the CLI warns
  (and refuses, absent `--relay`) if none survives, since the worker would only
  fail-safe discard it.
- `calctl jobs cancel <id>` — set `discarded` and NULL any retained credential:
  stop delivery and drop the secret now.
- `calctl jobs reschedule <id> --at <ts>` — move `scheduled_at` (delay, or bring
  forward without clearing attempts).
- `calctl jobs delete <id>` — hard-delete the row (also erases any retained
  credential); for pruning or a mistaken enqueue.
- **Not editable — deliberately:** `args` (body, recipient, uid, method),
  `worker`, `enc_credential`, `key_id`. Rewriting a send's target or body would
  break coalescing/idempotency (D10) and could forge mail; the credential is
  write-once by the queue. To change *what* is sent, `cancel` and let the app
  re-enqueue.

`calctl imip resend <id | --all-stale>` (D5b) is the domain wrapper over
`jobs retry`: for a per-user job it resets the row so the worker decrypts and
`SendAs` (metered); `--relay` instead re-enqueues a credential-free relay job (the
logged, unmetered override for when no usable credential survives).

## Forward-compat with Oban (mapping)

`worker "imip_send"` → `"Calendsd.Workers.ImipSend"`; `args` unchanged;
`state` text→enum (same names); `attempt`/`max_attempts`/`scheduled_at`/
`attempted_at` same; `last_error` → `errors` (wrap); `queue`/`priority`/`meta`/
`tags` defaults. **`enc_credential`/`enc_nonce`/`key_id` have no Oban column** —
they'd move to `meta` (still not plaintext) or a sibling table; the Elixir worker
decrypts with the same config key. D5b (the synchronous / manual-resend fallback)
is calendsd-side and unaffected. Cutover: boot Oban on the same DB, stop the Go
consumer.

## Implementation steps

1. **Schema.** `jobs` table + indexes (incl. dedup) in the calendsd schema.
   Additive-only.
2. **`internal/jobs` package.** Queue/Register/Enqueue/Start/Discard; AEAD
   encrypt-on-enqueue / decrypt-on-claim (key from config); claim-then-release
   loop (D9); backoff; `LISTEN/NOTIFY jobs`; periodic tick driving catch-up +
   reclaim (D9) + credential TTL purge (D12) + completed-row prune. Postgres
   integration tests (step 8).
3. **Storage/SQL.** Insert/claim/settle/reclaim/prune + the coalesce upsert
   (`ON CONFLICT … DO UPDATE`, newer-`(SEQUENCE, DTSTAMP)`-wins — D10); the
   transactional `SELECT … FOR UPDATE` object write-back.
4. **Wire iMIP.** Split `enqueueFirst` by identity: per-user first attempt RAM;
   per-user transient failure → `Enqueue(WithCredential)`; relay paths → `Enqueue`.
   Register `imip_send` with D11/D13 classification + conditional write-back.
   Convert `patchScheduleStatus` to the single transactional row-lock path used by
   **both** tiers; retire `objectLocks`.
5. **Config.** `imip.credential_key` (base64 32B, env override; feature gated on
   it), `reclaim_after` (> mailer timeout), retention, worker count, backoff, TTL.
6. **Operator CLI + D5b recovery.** Generic `calctl jobs list/show/retry/cancel/
   reschedule/delete` (state-only, credential-redacted, audit-logged — never
   handles the key). Reconciliation scan for stale `1.0` attendees +
   `imip_pending_stale` metric; the iMIP wrappers `calctl imip pending` (surface)
   and `calctl imip resend` (reset the job so the worker decrypts the retained
   credential → metered `SendAs`; `--relay` for the logged unmetered override when
   no credential survives); optional client "delivery pending" + "resend
   invitation" affordance.
7. **Server wiring + ops.** Construct/start the queue + sweeps in `server.go`.
   Prune `completed`. Metrics: reuse `imip_deliveries_total`; add `jobs_*`
   depth-by-state.
8. **Tests (Postgres-backed).** retry-to-success; permanent **discard** (D11/D13);
   backoff exhaustion; **restart durability** of an `available` job; **orphan
   reclaim** (backdate `attempted_at`, sweep, assert redelivery — the crash-mid-send
   case); **credential lifecycle** (encrypted at rest, decrypts to a working send,
   NULLed on `completed`, retained on `discarded` then TTL-purged); **per-user
   retry stays metered** (`SendAs`, not
   relay); **coalesce/supersede** (a re-issued PUT is a no-op; a newer revision by
   `(SEQUENCE, DTSTAMP)` replaces the pending job's body, an older one is dropped —
   D10); **one-job-per-recipient** (a retry re-sends only its
   recipient); **manual resend** (`calctl imip resend` on a `discarded` job with a
   retained credential decrypts → metered `SendAs`; and the `--relay` override path
   when no credential survives); **write-back under the row lock** across a RAM and
   a durable write-back on one multi-attendee object. Keep the RAM `SendQueue` unit
   tests for the first-attempt tier.
9. **Docs.** `docs/imip.md`: durable + metered per-user via encrypted credential;
   the D12 reversal of "never persisted" and its scope/mitigations; at-least-once /
   one-per-recipient / iCal idempotency (D10); SMTP result rules (D13); `args`
   hygiene (D8).

## Risks / follow-ups

- **The stored secret is the mailbox password (D12).** Encryption value hinges on
  the key being out-of-band and out of DB backups; on a single host, full host
  compromise defeats it (same posture as the DB credentials). **Accepted** as the
  deliberate price of durable, metered per-user delivery; the service-credential
  alternative was rejected as a worse, standing "send-as-anyone" exposure (D12,
  Alternatives).
- **Credential TTL vs outage length (D12).** Only rescues outages shorter than the
  TTL; a longer TTL widens exposure *and* the manual-resend window (D5b) both. A
  stuck send past the window drops the credential → D5b's no-credential tier.
- **Per-recipient credential fan-out (D12).** A PUT inviting N external attendees
  stores N rows, each an encrypted copy of the same password (distinct per-row
  nonces — cryptographically fine). Each row's credential is NULLed the moment its
  send succeeds and TTL-purged otherwise, so the at-rest footprint is bounded by
  in-flight + recently-failed sends.
- **Stale password on retry (D12/D13).** A user who changes their password after a
  send is queued gets `535` on retry → discard + D5b (benign but surfaces as a
  failure).
- **At-least-once duplicates (D10).** A crash between SMTP `250` and `completed`
  re-sends; bounded by `reclaim_after`, harmless via `UID`/`SEQUENCE` idempotency.
- **Orphaned `executing` rows (D9).** Without the reclaim sweep the durability goal
  silently half-fails; sweep + test are not optional.
- **Multi-node write-back (handled).** Per-object write-backs serialize
  cross-node via a Postgres advisory lock (`AdvisoryLocker`), not just the
  in-process `objectLocks` — safe to add calendsd nodes. See *Write-back
  concurrency*.
- **Per-user off-path delivery needs the encryption key.** Without a key (or a DB)
  per-user sends are synchronous on the request path — they still deliver, but
  block on SMTP. Relay-native sends need a relay `Mailer`; a deployment with
  neither still functions synchronously, with D5b for anything left pending.
- **Row growth / pruning (D4/step 7).** `message_b64` rows are multi-KB.
- **Scope creep (D6).** No push/purge migration in this PR.

## Alternatives considered

**Decision: D12 (encrypted per-user credential) is the committed path.** Per-user
invites auto-survive a restart and stay metered, at the accepted cost of encrypted
mailbox passwords on disk (scoped, TTL'd, out-of-band key).

- **Submit-as-user service credential — rejected.** One server-held credential
  mizu trusts to send as any user would make every send durable and metered with
  *no per-user secret stored* — but it is a standing "send-as-anyone" master key:
  a single leak (from config) = impersonate every user indefinitely, the worst
  blast radius here and catastrophic for a mail provider (mass phishing from real
  customer addresses). Reconsider only if mizu offers a network-isolated injection
  channel where isolation, not a shared secret, is the control.
- **Two-tier: RAM first attempt + durable retry — built, then removed.** Kept an
  in-memory first attempt so a *successful* per-user send never persisted the
  credential; only failing sends held one at rest. Removed once we saw the "fewer
  rows at rest" margin only exists in the DB+key-compromise case D12 already
  concedes — the tier was a whole in-memory subsystem and a cross-goroutine
  handoff for no real gain (D5, "Why one tier").
- **No async tier at all — the fallback.** When no key/DB is configured, per-user
  sends are simply synchronous on the request path (they block on SMTP but still
  deliver); D5b surfaces anything left pending. This is the degraded mode, not the
  target.
- **Scoped, short-lived submission token — future upgrade, not a blocker.** If mizu
  could mint a single-purpose, expiring token at request time (when the user's
  password is already present), we would store *that* in place of the reusable
  password: it can't read mailboxes, can't be broadly reused, and expires —
  dominating D12 on exposure without a standing master credential. A clean drop-in
  if mizu ever supports it (the `enc_credential` column simply holds the token).

Not decision-relevant: whether mizu meters by authenticated user or From-address.
D12 submits *as the user with their own credential*, so metering works either way;
the distinction would only matter to the rejected service-credential path.

## Key files

- `scripts/schema/pgsql.calserver.sql` — `jobs` table + indexes.
- `internal/jobs/` — `jobs.go` (queue, worker loop, claim/settle), `aead.go`
  (keyring), `store.go` (claim/reclaim/TTL/prune + coalesce SQL), `ops.go` (the
  operator query/mutation layer, credential-redacted).
- `internal/caldav/imip.go` — `enqueueDurable` (per-user with credential, relay
  without); `DurableHandler` (send-as-user vs relay, D11/D13 classification,
  write-back); synchronous fallback when no key/DB. (`imip_queue.go`, the RAM
  `SendQueue`, was deleted.)
- `internal/caldav/schedule.go` — deferral unchanged; stored `1.0` is the D5b
  record.
- `internal/config/`, `internal/server/server.go` — credential key + queue wiring.
- `cmd/calctl/jobs.go` — `calctl jobs list/show/retry/cancel/reschedule/delete`
  and the `imip pending`/`imip resend` wrappers (D5b).
- `cmd/calctl` — generic `jobs list/show/retry/cancel/reschedule/delete`
  (state-only, credential-redacted, audit-logged); iMIP wrappers `imip pending`
  (surface) / `imip resend` (metered via the retained credential, or `--relay`
  override) (D5b).
- `docs/imip.md` — metering + D12 reversal, D10 semantics, D13 rules, D8 invariant.
```
