# iMIP — Emailing Scheduling Messages

When an attendee (or organizer) on a scheduling operation is not a local
principal, calserver delivers the iTIP message (`REQUEST`, `REPLY`, `CANCEL`)
by email — "iMIP". There are two ways it can send, selected by config under
`[scheduling.imip]`.

## Two send paths

**Shared relay (`smtp`)** — the classic behavior. calserver hands the message
to one SMTP relay. The envelope sender (`MAIL FROM`) is always the server's own
`sender` address; `send_as_organizer` optionally puts the organizer/attendee in
the `From` header, but the message is *not* authenticated as anyone and only
passes DMARC if the relay is configured to DKIM-sign for those domains.

**Per-user submission (`submission`)** — send *as the logged-in user*. When set,
an invitation or reply whose sender is the logged-in user is submitted to the
`submission` server **authenticated with that user's own credentials** (the ones
they presented over CalDAV Basic auth), exactly like their mail client would.

Both may be configured together: `submission` handles user-originated mail, and
`smtp` remains the relay for resource replies (see below).

## Division of responsibility (per-user submission)

calserver deliberately does **not** sign, rewrite, or rate-limit. It:

- **Authenticates as the user.** The SMTP AUTH identity is the CalDAV login; the
  password is the one presented on the triggering request. It is never logged, and
  persisted only **encrypted**, and only while a durable send is pending (see
  *Durable delivery*). This requires **Basic** auth — a Digest login carries no
  reusable password and cannot send as itself.
- **Presents a `From` within the user's own address set** and never rewrites it,
  so the `From` header and the in-body `ORGANIZER` stay one identity (a split
  would break attendee-reply routing).
- **Always uses TLS** (implicit `:465` or STARTTLS `:587`); credentials are never
  sent on an unencrypted connection.
- **Delivers off the request path** and bounds each attempt with a tight deadline
  (10s for the whole dial + SMTP exchange): the send is enqueued and run by a
  background worker, so a hung submission server never stalls the PUT (see
  *Durable delivery*). Without a credential key it degrades to a synchronous
  send inside the request.

The **submission server** (e.g. a mizu_out tier) owns everything else:

- **`allowed_from`** — which `From` addresses the authenticated account may use
  (calserver's own check is defense-in-depth; this is the real backstop).
- **DKIM signing** for the user's domain, so the mail passes DMARC.
- **Rate / sending limits** — because the message is authenticated as the user,
  it is metered against *their* limits, not a shared relay's.

## The Core rule: the logged-in user is always the sender

For every message, calserver classifies the sender:

1. **The logged-in user** (sender ∈ their address set) → submitted as them.
2. **An auto-schedule resource** (a room; no interactive login) → relayed
   through the shared `smtp` sender. Resources have no per-user limit to meter,
   so this is the one sanctioned relay in per-user mode. Requires `smtp` to be
   configured; otherwise these replies fail.
3. **Anyone else** (a delegated organizer — a *different* real user) → refused
   with a schedule-status. We hold only the logged-in user's credential and must
   not send as another person, nor relay (that would bypass their limits).
   Delegated sending (iTIP `ORGANIZER` + `SENT-BY`) is not supported yet.

## Durable delivery

Background sends go through a **Postgres-backed job queue** (see
`JOB-QUEUE-PLAN.md`). A PUT or DELETE enqueues the send and returns; a worker
delivers it off the request path. The stored `SCHEDULE-STATUS` is `1.0` (pending)
until the send settles, then the worker patches the terminal status back onto the
object. Unlike the old in-memory queue, a pending send **survives a process
restart** — that is the point of the durable table.

**Result handling.** A **transient** failure — a network/TLS error, a timeout, or
a 4xx SMTP rejection (greylisting, sending limit) — is retried with backoff (30s,
2m, 5m, 15m, 30m) until the attempts run out. A **permanent** failure (a 5xx
rejection, and an authentication failure in particular) is never retried and gets
its terminal `5.x` status immediately. A worker that dies mid-send has its job
reclaimed after a timeout and retried, so an in-flight send survives a crash too.

**Encrypted credential at rest.** A per-user send must authenticate *as the user*
to stay metered, so — unlike relay sends, which carry no secret — its job stores
the user's submission password **encrypted** (AES-256-GCM) with a key from config
(`credential_key`). This is a deliberate, scoped reversal of the older "the
credential is never persisted" rule: durability requires persistence. The
password is **never** in the job's plaintext `args` (a standing invariant); it
lives only in a dedicated encrypted column, is erased the instant the send
succeeds, and is TTL-purged otherwise. Its security rests on the key living
out-of-band (in config, excluded from DB backups): a leaked DB dump alone yields
only undecryptable ciphertext. A full host compromise (DB *and* key) defeats it —
the same posture the database credentials already have.

If no `credential_key` is configured, the per-user durable path is off and those
sends are **synchronous** on the request path instead (they still deliver, but
block on SMTP). Relay-native sends are durable either way.

**At-least-once.** An SMTP send and a database commit can't be one atomic
transaction, so a crash between the SMTP `250` and the commit re-sends on
recovery. This is harmless: one job is one recipient, and a duplicate
`REQUEST`/`CANCEL` with the same `UID`+`SEQUENCE` is idempotent at the recipient's
calendar — a second email, not a second event. A lost invite is worse than a
duplicate.

**Stuck at pending — recovery.** If the send can't proceed as the user (no key, a
TTL-purged credential, or a rotated-out key), the object stays at `1.0;pending`
rather than going out unmetered. That pending state is the durable record; recover
by resending the invitation from the organizer's client (their live request
carries the password) — a plain re-save of an unchanged event won't resend.

## Metrics

`calserver_imip_deliveries_total{outcome}` labels each attempt. `smtp_error`,
`auth_error`, and `retry_exhausted` are infrastructure incidents worth alerting
on; `rate_limited`, `not_authorized_sender`, and `no_credential` are expected
client-side outcomes. `sent_as_user` and `resource_relay` are the two success
paths in per-user mode. A message enqueued for durable delivery counts once as
`queued` and once more at its terminal outcome.

## Configuration

See `config.toml.example` under `[scheduling.imip]`: `smtp`, `sender`,
`send_as_organizer`, `submission`, and `submission_implicit_tls`.

For durable delivery, `credential_key` is the base64 of a random 32-byte key
(`head -c32 /dev/urandom | base64`); `$CALSERVER_IMIP_CREDENTIAL_KEY` overrides
it. Keep it out of DB backups — that separation is its whole security value.
`[scheduling.imip.queue]` optionally tunes the background sweeps (`reclaim_after`
> the SMTP timeout, `credential_ttl`, `retention`, `workers`); each empty/zero
value uses a built-in default. The durable queue runs whenever calserver has a
database; the in-memory server (no DB) sends synchronously.
