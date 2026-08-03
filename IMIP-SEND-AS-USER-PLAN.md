# iMIP: Send Invitations As the Logged-In User

## Goal

When a logged-in user organizes, updates, replies to, or cancels an event,
the resulting iTIP email must go out **as that user** — authenticated with
their own credentials through the `mizu_out` submission tier — so that it is:

- DKIM/SPF/DMARC-aligned and attributed to the user (not spoofed via a shared relay), and
- **metered against that user's outbound sending limits.**

Sending through `mizu_out` submission is the *only* path that counts toward the
user's limits (the limits live in mizu_out's `[server.rate_limit]` and the
rspamd `migadu_counters`/`migadu_suspender`). Any internal-ingest shortcut or
shared-relay send would bypass them.

## Background / current state

- **`IMipPlugin.DeliverExternal(msg)`** (`internal/caldav/imip.go`) emails iTIP
  messages through a single **unauthenticated relay** (`SMTPMailer{Addr}`); the
  envelope sender is always the server's `IMipSender`. The existing
  `SendAsOrganizer` flag only rewrites the `From:` header and relies on the
  relay to DKIM-sign — it never authenticates as the user (effectively
  trusted-relay spoofing) and does not count against the user's limits.
- **Auth**: calserver authenticates CalDAV users via HTTP **Basic against
  rcptd's `/auth`** (Dovecot hashes) — `internal/server/server.go` ~line 258.
- **mizu_out submission** (`:465` implicit TLS / `:587` STARTTLS,
  `type: submission`) authenticates users against the **same rcptd `/auth`**
  endpoint, enforces an **`allowed_from` CDB**, DKIM-signs, and rate-limits.
  So mizu_out already does the full MUA-submission job.
- **The insight**: "send as the logged-in user" = make calserver an SMTP
  **submission client to `mizu_out`, authenticating as the user**. Same identity
  source, downstream DKIM/SPF/DMARC/limits, no spoofing. calserver already
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
- **Sender ≠ request principal for resource replies**: auto-scheduled resources
  (rooms) generate a `REPLY` to the organizer, queued during the organizer's PUT
  and flushed later in `AfterWrite` (`schedule.go:176`,
  `schedule_delivery.go:203-215`). There the message *sender* is the resource,
  but the authenticated principal on `r` — and any stashed password — belong to
  the **organizer** who made the PUT. See the resource-reply decision below.

## Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Credential model | **Reuse the user's CalDAV Basic password** | mizu_out only does plaintext verification via rcptd `/auth` (no SASL/SCRAM/CRAM), so hash-auth is impossible (a hash used to log in is just pass-the-hash). A master/service credential was rejected: it is a standing skeleton key (send-as-anyone) = far larger blast radius. Reusing the password adds no new class of secret — calserver already receives and verifies it on every Basic request. |
| Fallback when per-user submission can't happen | **Fail with an iTIP schedule-status** | No shared-relay fallback: relaying would bypass the user's sending limits, contradicting the goal. Client sees non-delivery. |
| Sender identity (v1) | **Any address the server already resolves for the principal** (`addressesForPrincipal`), not just the primary | Restricting to the primary would *reject* organizers who legitimately used a valid alias as `ORGANIZER` — a new restriction the old relay path never imposed. calserver's check is defense-in-depth; mizu_out's `allowed_from` CDB is the real backstop. Cross-account/shared identities still deferred. |
| Resource-originated replies (auto-scheduled rooms) | **v1: fall back to the legacy `server` relay for these, and only these, sends** | The sender is the resource, but the only credential in hand is the organizer's — sending "as the resource" authenticated as the organizer is exactly what `allowed_from` must reject. A resource has no interactive login and thus no reusable password. Relaying resource replies through the shared sender keeps rooms able to notify remote organizers; resources are server-owned identities with no user send-limit to meter, so the "must count against limits" goal doesn't apply to them. A dedicated resource submission credential is the v2 cleanup. |

### Security constraints on the reused password

- **TLS only** to mizu_out (implicit 465 / STARTTLS 587) — never `AUTH PLAIN` in the clear.
- **Request-scoped only** — in-memory for the lifetime of the triggering
  request; never persisted, never cached beyond the request.
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
- Resolve the authenticated principal from `r` (`dav.Principal`).
- **Wire a principal→addresses resolver into `IMipPlugin`.** The sender check
  needs the principal's address *set*, but `IMipPlugin` today only holds
  `server *dav.Server` (`imip.go:54`); `addressesForPrincipal` lives on
  `SchedulePlugin`. Inject a resolver func (or share it) when the plugin is
  built in `server.go`.
- **Authorize the sender**: require `msg.Sender` ∈ the principal's resolved
  address set; otherwise set a clean schedule-status and do not send. (mizu_out
  `allowed_from` is the ultimate backstop.)
- **Resource-reply branch**: if `msg.Sender` is *not* in the request principal's
  set (the auto-scheduled-resource REPLY case), do **not** fail — route this
  send through the legacy `server` relay path (per the resource-reply decision).
  This is the one sanctioned relay fallback.
- Otherwise pull the password from context; if absent → **fail with a status**
  (no relay fallback for user sends).
- Send via `SubmissionMailer` as the user. `From:` = `msg.Sender` (organizer for
  REQUEST/CANCEL, replying attendee for REPLY).
- **Map mizu_out responses**: `4xx`/`5xx` (auth failure, over-limit, rejected) →
  the appropriate iTIP schedule-status so the client sees non-delivery.
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
- **Unit**: sender-authorization (own address vs foreign, **alias in the set is
  accepted**), correct `From`/`MAIL FROM`/`AUTH`, TLS-required,
  fail-when-no-password, rate-limit response mapping.
- **Resource reply**: a REPLY whose sender is not the request principal routes
  to the relay (asserts `resource_relay`, no auth attempt as the organizer).
- **Digest login**: no plaintext stashed → user send fails with a status.
- Update `internal/caldav/imip_test.go` and `imip_metrics_test.go` to the new
  `DeliverExternal(r, msg)` signature (inject a request carrying a fake
  principal + secret).
- **Integration**: fake SMTP submission server asserting the `AUTH` username ==
  the logged-in principal and `From` == the organizer.

### 8. Docs
Update `config.toml.example` and add a scheduling note: DKIM, `allowed_from`,
and rate-limits are mizu_out's responsibility; calserver presents a `From`
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
  because resources have no user send-limit to meter, but it means the "always
  authenticate as the sender" property has one deliberate exception. v2: a
  dedicated per-resource submission credential removes it.
- **Over-limit UX**: a user hitting their send limit gets a non-delivery
  schedule-status; confirm that is the desired client-facing behavior.

## Key files

- `internal/caldav/imip.go` — `IMipPlugin.DeliverExternal`, `SMTPMailer`
- `internal/caldav/schedule_delivery.go` — `ExternalDeliverer` interface, `deliver(r, msg)`
- `internal/caldav/ical/itip.go` — `ical.Message`
- `internal/server/server.go` — rcptd auth backend (~line 258), iMIP plugin wiring (~line 333)
- `internal/config/config.go`, `internal/config/shared.go` — config plumbing
- `config.toml.example` — `[scheduling.imip]` section
- Infra reference: `roles/mizu` and `inventory/group_vars/mizu_out/vars.yml`
  (submission servers, rcptd `/auth`, `allowed_from`, rate limits)
