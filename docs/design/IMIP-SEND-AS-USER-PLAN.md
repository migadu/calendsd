# iMIP: Send Invitations As the Logged-In User

> **Status: implemented.** All 8 steps landed. Key code: `internal/caldav/`
> (`imip.go` DeliverExternal branching, `submission.go` SubmissionMailer),
> `internal/dav/context.go` (SubmissionSecret), `internal/auth/auth.go` (stash),
> `internal/config/` + `internal/server/server.go` (wiring), `docs/imip.md`.
> Tests: `imip_sendasuser_test.go`, `submission_test.go`, `context_test.go`.

## Goal

When a logged-in user organizes, updates, replies to, or cancels an event,
the resulting iTIP email must go out **as that user** — authenticated with
their own credentials through the `mizu_out` submission tier — so that it is:

- DKIM/SPF/DMARC-aligned and attributed to the user (not spoofed via a shared relay), and
- **metered against that user's outbound sending limits.**

Sending through `mizu_out` submission is the *only* path that counts toward the
user's limits (the limits live in mizu_out's `[server.rate_limit]` and the
rspamd rate counters and suspender). Any internal-ingest shortcut or
shared-relay send would bypass them.

## Core rule

**The logged-in user is always the sender of the invitation.** In iTIP terms
the "sender" of a scheduling message *is* an identity in the message — the
`ORGANIZER` for REQUEST/CANCEL, the replying `ATTENDEE` for REPLY. So this rule
means:

> **The logged-in user must be the `ORGANIZER`/replying `ATTENDEE` of any
> message calendsd sends as them, and the email `From:` equals that identity.**

Enforce it by **validating** that the scheduling sender is one of the logged-in
user's own addresses — **not** by rewriting `From:`. The `From:` header and the
in-body `ORGANIZER` must stay the same identity, because attendee replies route
to `ORGANIZER` (not `From`) and clients track/dedupe the meeting by `ORGANIZER`;
a split would break reply routing.

This holds automatically for all normal user-initiated scheduling:
`logged-in user` = `calendar owner` = `ORGANIZER` = `msg.Sender`. It collapses
the authorization to a single check —
`msg.Sender ∈ addressesForPrincipal(dav.Principal(r))`:

- **match** → send as the logged-in user (the per-user submission path);
- **no match** → this is *not* the logged-in user sending their own invitation.
  We never send per-user-as-someone-else. It is exactly one of two divergence
  cases, both handled below: a **delegated-organizer PUT** (reject) or a
  **resource auto-reply** (relay).

## Background / current state

- **`IMipPlugin.DeliverExternal(msg)`** (`internal/caldav/imip.go`) emails iTIP
  messages through a single **unauthenticated relay** (`SMTPMailer{Addr}`); the
  envelope sender is always the server's `IMipSender`. The existing
  `SendAsOrganizer` flag only rewrites the `From:` header and relies on the
  relay to DKIM-sign — it never authenticates as the user (effectively
  trusted-relay spoofing) and does not count against the user's limits.
- **Auth**: calendsd authenticates CalDAV users via HTTP **Basic against
  rcptd's `/auth`** (Dovecot hashes) — `internal/server/server.go` ~line 258.
- **mizu_out submission** (`:465` implicit TLS / `:587` STARTTLS,
  `type: submission`) authenticates users against the **same rcptd `/auth`**
  endpoint, enforces an **`allowed_from` CDB**, DKIM-signs, and rate-limits.
  So mizu_out already does the full MUA-submission job.
- **The insight**: "send as the logged-in user" = make calendsd an SMTP
  **submission client to `mizu_out`, authenticating as the user**. Same identity
  source, downstream DKIM/SPF/DMARC/limits, no spoofing. calendsd already
  receives the user's password on the CalDAV Basic request that triggers the
  send, so the credential is in hand.
- **Structural enabler**: `SchedulePlugin.deliver(r *http.Request, msg)`
  (`internal/caldav/schedule_delivery.go`) already holds the request, but the
  `ExternalDeliverer.DeliverExternal(msg)` interface drops it. Threading `r`
  through gives the delivery path the authenticated principal (and, once
  stashed, the password).
- **Per-recipient message model**: `ical.ParseEvent` produces **one
  `*ical.Message` per recipient**, and `deliver(r, msg)` runs once per message
  (`schedule.go:127-137`). Each `DeliverExternal` call therefore sees a single
  recipient — there is no point that holds all attendees of an event at once.
- **Two ways `msg.Sender` diverges from the logged-in user** (the only cases the
  Core rule's "no match" branch must handle):
  - **Delegated-organizer PUT**: a delegate saves an event on a shared calendar
    whose `ORGANIZER` is the calendar owner. `dav.Principal(r)` (the delegate)
    ≠ `msg.Sender` (the owner). The parse already keys on the owner
    (`addressesForPrincipal(calNode.Owner())`, `schedule.go:126`), but the only
    credential in hand is the delegate's — which `allowed_from` must reject for
    the owner's address. Proper support is the iTIP `ORGANIZER` + `SENT-BY`
    model, out of v1 scope. **Reject** in v1 (see decision).
  - **Resource auto-reply**: auto-scheduled resources (rooms) generate a `REPLY`
    to the organizer, queued during the organizer's PUT and flushed later in
    `AfterWrite` (`schedule.go:176`, `schedule_delivery.go:203-215`). The sender
    is the resource; there is no interactive login and no reusable password.
    **Relay** in v1 (see decision).
  - **Distinguishing them matters**: both fail the `msg.Sender ∈ principal set`
    check, but one must reject and the other must relay. Use a *positive* test
    for a resource/non-login identity (the sender principal resolves to a
    resource-type / login-less principal), **not** "sender ≠ request principal"
    — otherwise a delegated *human* organizer would be silently relayed through
    the shared sender, bypassing that user's send limits (the exact outcome this
    design exists to prevent).

## Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Credential model | **Reuse the user's CalDAV Basic password** | mizu_out only does plaintext verification via rcptd `/auth` (no SASL/SCRAM/CRAM), so hash-auth is impossible (a hash used to log in is just pass-the-hash). A master/service credential was rejected: it is a standing skeleton key (send-as-anyone) = far larger blast radius. Reusing the password adds no new class of secret — calendsd already receives and verifies it on every Basic request. |
| Fallback when per-user submission can't happen | **Fail with an iTIP schedule-status** | No shared-relay fallback: relaying would bypass the user's sending limits, contradicting the goal. Client sees non-delivery. |
| Sender identity | **The logged-in user must be the message sender** (Core rule): `msg.Sender ∈ addressesForPrincipal(dav.Principal(r))`, matched against **any** of the principal's resolved addresses (not just the primary) | Any of a principal's own addresses/aliases is legitimate; restricting to the primary would reject an organizer who used a valid alias as `ORGANIZER`. Validate — never rewrite `From:` — so `From:` and in-body `ORGANIZER` stay one identity. calendsd's check is defense-in-depth; mizu_out's `allowed_from` CDB is the real backstop. Cross-account/shared identities still deferred. |
| Delegated-organizer PUT (sender is another *human*, not the logged-in user) | **v1: reject with an iTIP schedule-status; do not send** | The logged-in user is not the organizer, so the Core rule forbids sending as them, and we have no credential for the real organizer. Silently relaying would bypass the organizer's send limits and mismatch `From:`/`ORGANIZER`. Proper delegation is the iTIP `ORGANIZER` + `SENT-BY` project (v2+). |
| Resource-originated replies (auto-scheduled rooms) | **v1: relay through the legacy `server` sender for these, and only these, sends** | The sender is a login-less resource with no reusable password and no per-user send-limit to meter, so the "count against limits" goal doesn't apply. Detect it with a **positive** resource/non-login test on the sender principal (not "sender ≠ request principal", which would also catch delegated humans). Relaying keeps rooms able to notify remote organizers. A dedicated per-resource submission credential is the v2 cleanup. |

### Security constraints on the reused password

- **TLS only** to mizu_out (implicit 465 / STARTTLS 587) — never `AUTH PLAIN` in the clear.
- **Request-scoped only** — in-memory for the lifetime of the triggering
  request; never persisted, never cached beyond the request.
  - **Superseded** by `JOB-QUEUE-PLAN.md`: to make background sends survive a
    restart, this "never persisted" rule was deliberately, narrowly reversed —
    the password is now stored **AES-256-GCM-encrypted** (out-of-band key) while a
    durable send is pending, and NULLed the moment it succeeds. See that plan's D12
    for the threat-model rationale.
- **Never logged** — reqlog already scrubs Basic; ensure the SMTP client path
  cannot log credentials.

## Implementation steps

### 1. Thread the request into external delivery
Change the interface in `internal/caldav/schedule_delivery.go`:
`ExternalDeliverer.DeliverExternal(msg)` → `DeliverExternal(r *http.Request, msg *ical.Message)`.
The sole caller, `SchedulePlugin.deliver(r, msg)` (~line 34), already has `r`.
Update the two direct-call tests.

### 2. Capture the submission credential (request-scoped)
On a **successful Basic auth**, stash the plaintext password into the request
context via a new `dav` helper (e.g. `SetSubmissionSecret` / `Credential`).
In-memory, request-lifetime only. The authenticated principal is already in
context (`internal/dav/context.go`).

- Stash from `r.BasicAuth()` in the auth **plugin** after a backend returns ok,
  not from inside the rcptd backend — the plaintext is on the request
  regardless of the rcptd verify-cache (`rcptd.go` `cacheHit`), and this keeps
  one stash point for both the rcptd and fallback Basic backends.
- **Digest and any non-Basic login carry no plaintext** → nothing to stash → the
  send fails with a schedule-status (per the fallback decision). The non-rcptd
  config branch (`server.go:277-286`) still wires `DigestBackend`, so a Digest
  client can authenticate for CalDAV yet cannot send iMIP as itself. This is
  expected, not a bug; note it in docs.
- The credential lives on the same `*http.Request` that `AfterWrite` reuses
  (`schedule.go:176`), so request-scoped lifetime is sufficient for
  queued-reply flushes — except resource replies, which use the relay fallback
  and need no credential.

### 3. Per-user submission mailer
Add a `SubmissionMailer` that dials `mizu_out` **over TLS only**, `AUTH PLAIN`
as `(user, password)`, `MAIL FROM` = user's address.

- **TLS transport**: implicit-TLS 465 needs a manual `tls.Dial` +
  `smtp.NewClient` — `net/smtp.SendMail` only does STARTTLS, not implicit TLS.
  For STARTTLS 587 the TLS-only guarantee comes for free: `smtp.PlainAuth`
  refuses to transmit credentials on a non-TLS, non-localhost connection.
- **Connection reuse**: because delivery is one message per recipient (see
  Background), "one connection, many `RCPT`s" does not map onto the call
  structure. If reuse is wanted, key a **request-scoped mailer/connection cache
  by principal** and tear it down at end of request; otherwise dial per message.
  v1 may dial per message and defer pooling.
- **Bounce routing changes**: today the envelope sender is always the server, so
  DSNs return to the server (`imip.go:37`). Setting `MAIL FROM` = the user moves
  bounces to the user's own mailbox. Intended for "send as user," but call it
  out — it interacts with the synchronous-send failure mode below.

### 4. Rework `IMipPlugin.DeliverExternal(r, msg)`
Implements the **Core rule** — the logged-in user is always the sender — as a
single classification of `msg.Sender`, with a positive resource test to keep the
two divergence cases apart.

- Resolve the authenticated principal from `r` (`dav.Principal`).
- **Wire a principal→addresses resolver *and* a resource/non-login test into
  `IMipPlugin`.** The sender check needs the principal's address *set*, but
  `IMipPlugin` today only holds `server *dav.Server` (`imip.go:54`);
  `addressesForPrincipal` lives on `SchedulePlugin`. Inject both a resolver func
  and an `isResourceSender(msg.Sender) bool` predicate when the plugin is built
  in `server.go`.
- **Classify `msg.Sender` and branch:**
  1. **`msg.Sender` ∈ `addressesForPrincipal(dav.Principal(r))`** → the Core-rule
     happy path. Pull the password from context; if absent (Digest / non-Basic
     login) → **fail with a schedule-status**. Otherwise send via
     `SubmissionMailer` **as the logged-in user**. `From:` = `msg.Sender`
     (organizer for REQUEST/CANCEL, replying attendee for REPLY) — never a
     rewrite; it already equals the user's identity.
  2. **`msg.Sender` is a resource / login-less identity** (positive
     `isResourceSender` test) → route through the legacy `server` relay path.
     This is the one sanctioned relay fallback (metric `resource_relay`).
  3. **otherwise** (a delegated *human* organizer — sender is a real user that
     is not the logged-in principal) → **fail with a schedule-status**
     (`not_authorized_sender`). Do **not** relay: that would bypass the
     organizer's send limits and split `From:`/`ORGANIZER`.
- **Map mizu_out responses** on path 1: `4xx`/`5xx` (auth failure, over-limit,
  rejected) → the appropriate iTIP schedule-status so the client sees
  non-delivery.
- mizu_out's `allowed_from` CDB remains the ultimate backstop behind the local
  address check.
- Keep the existing CR/LF header-injection stripping (`stripHeaderChars`).

### 5. Config + wiring
- New config: `scheduling.imip.submission` (host) + a mode selector
  (`per_user` default; keep legacy `server` relay for non-mizu deployments) +
  TLS mode.
- Plumb through `internal/config/config.go`, `internal/config/shared.go`,
  `config.toml.example`, and `internal/server/server.go` (build a
  `SubmissionMailer` when configured and pass the mode).

### 6. Metrics
Extend `IMipDelivery` outcomes: `sent_as_user`, `auth_error`, `rate_limited`,
`not_authorized_sender`, `no_credential`, and `resource_relay` (resource reply
sent via the legacy relay) so the fallback path is observable and doesn't fold
into `not_authorized_sender`.

### 7. Tests
One test per Core-rule branch (§4), plus mechanics:
- **Happy path**: `msg.Sender` == the logged-in principal's own address (and a
  second case with an **alias in the set**) → sent as the user; assert
  `From`/`MAIL FROM`/`AUTH` all == that identity, TLS-required, `sent_as_user`.
- **Delegated human organizer**: `msg.Sender` is another real user, not the
  principal → **fails** with a schedule-status (`not_authorized_sender`); assert
  **no** SMTP auth attempt and **no** relay send.
- **Resource reply**: `msg.Sender` passes the positive `isResourceSender` test →
  routes to the relay (`resource_relay`), no auth attempt as the organizer.
  Cover a REPLY generated in `AfterWrite`.
- **Digest / no credential**: no plaintext stashed → user send fails
  (`no_credential`).
- **Rate-limit / over-limit**: mizu_out `4xx`/`5xx` → mapped schedule-status
  (`rate_limited` / `auth_error`).
- **CANCEL via DELETE**: a `BeforeUnbind`-driven CANCEL (organizer == logged-in
  user) sends as the user — same mechanism as PUT, so it must be exercised too.
- Update `internal/caldav/imip_test.go` and `imip_metrics_test.go` to the new
  `DeliverExternal(r, msg)` signature (inject a request carrying a fake
  principal + secret).
- **Integration**: fake SMTP submission server asserting the `AUTH` username ==
  the logged-in principal and `From` == the organizer.

### 8. Docs
Update `config.toml.example` and add a scheduling note: DKIM, `allowed_from`,
and rate-limits are mizu_out's responsibility; calendsd presents a `From`
within the user's allowed set and authenticates as the user.

## Risks / follow-ups

- **Synchronous SMTP inside the PUT** adds latency and a failure mode; v1 keeps
  it synchronous (as today) with a tight timeout. A retry queue is a sensible v2.
- **Basic-only coupling**: if CalDAV ever moves to tokens/OAuth there is no
  password to reuse — revisit with **app-specific submission credentials** (the
  cleaner long-term model) or **SCRAM-SHA-256 end-to-end** (needs changes to
  mizu + rcptd + CalDAV clients). The same gap exists *today* for Digest logins
  on the non-rcptd fallback branch — they can't send as themselves.
- **Resource replies relay in v1**: auto-scheduled rooms answering a remote
  organizer go through the shared sender, not per-user submission. Acceptable
  because resources have no user send-limit to meter, but it is the one
  deliberate exception to the Core rule (sender is not a logged-in user). v2: a
  dedicated per-resource submission credential removes it.
- **Delegated organizers rejected in v1**: under the Core rule, a delegate
  saving an event whose `ORGANIZER` is the calendar owner cannot send the
  invitation (they aren't the organizer and we hold only their credential). The
  scheduling message fails with a status. If delegation is needed, it is the
  iTIP `ORGANIZER` + `SENT-BY` project — the delegate authenticates as
  themselves but sends on the organizer's behalf, which mizu_out's `allowed_from`
  would have to permit.
- **Over-limit UX**: a user hitting their send limit gets a non-delivery
  schedule-status; confirm that is the desired client-facing behavior.

## Key files

- `internal/caldav/imip.go` — `IMipPlugin.DeliverExternal`, `SMTPMailer`
- `internal/caldav/schedule_delivery.go` — `ExternalDeliverer` interface, `deliver(r, msg)`
- `internal/caldav/ical/itip.go` — `ical.Message`
- `internal/server/server.go` — rcptd auth backend (~line 258), iMIP plugin wiring (~line 333)
- `internal/config/config.go`, `internal/config/shared.go` — config plumbing
- `config.toml.example` — `[scheduling.imip]` section
- Infra reference: the mizu_out deployment configuration (submission
  servers, rcptd `/auth`, `allowed_from`, rate limits)
