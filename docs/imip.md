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
  password is the one presented on the triggering request, held only for that
  request (never logged or persisted). This requires **Basic** auth — a Digest
  login carries no reusable password and cannot send as itself.
- **Presents a `From` within the user's own address set** and never rewrites it,
  so the `From` header and the in-body `ORGANIZER` stay one identity (a split
  would break attendee-reply routing).
- **Always uses TLS** (implicit `:465` or STARTTLS `:587`); credentials are never
  sent on an unencrypted connection.
- **Bounds the send with a tight deadline** (10s for the whole dial + SMTP
  exchange): the first attempt is synchronous inside the triggering CalDAV
  request, so a hung submission server fails that attempt instead of stalling
  the PUT — and the message then falls to the retry queue below.

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

## Retries

A send that fails **transiently** — a network/TLS error, a timeout, or a 4xx
SMTP rejection (greylisting, sending limit) — is not lost: it goes to an
in-memory retry queue. The stored `SCHEDULE-STATUS` becomes `1.0` (pending) and
the queue retries in the background: immediately once (a timed-out connection
often warms the path — e.g. a negative DNS cache — so the instant retry
frequently succeeds), then after 30s, 2m, 5m, 15m, and 30m before giving up.
Permanent failures (5xx rejections, and authentication failures in particular)
are never retried and keep their terminal `5.x` status.

The queue is deliberately **in-memory only**: a queued per-user send carries the
user's submission credential, which must never be persisted. A restart drops
pending messages; every scheduled retry was already logged, so the trace is in
the journal. Final outcomes appear in the log (`imip delivered (retry)` /
`imip giving up`) and in the delivery metric.

Attempts run on a small worker pool (4), so one stalled send does not serialize
the rest, and the queue is **capped** (256 pending): beyond that, enqueueing is
refused (`queue_full` outcome, logged) and the message keeps its terminal
failure status — a long outage cannot pin unbounded credentials in RAM.

## Metrics

`calserver_imip_deliveries_total{outcome}` labels each attempt. `smtp_error`,
`auth_error`, and `retry_exhausted` are infrastructure incidents worth alerting
on; `rate_limited`, `not_authorized_sender`, and `no_credential` are expected
client-side outcomes. `sent_as_user` and `resource_relay` are the two success
paths in per-user mode. A message handed to the retry queue counts once as
`queued` and once more at its terminal outcome.

## Configuration

See `config.toml.example` under `[scheduling.imip]`: `smtp`, `sender`,
`send_as_organizer`, `submission`, and `submission_implicit_tls`.
