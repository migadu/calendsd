package caldav

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
	"calserver/internal/jobs"
	"calserver/internal/metrics"
)

// Mailer sends an email. Implementations wrap SMTP or a test spy.
type Mailer interface {
	Send(from string, to []string, msg []byte) error
}

// SMTPMailer sends mail through an SMTP relay: opportunistic STARTTLS when
// the server advertises it (like net/smtp.SendMail, verifying against the
// system roots), optional Auth. The whole exchange shares one deadline so a
// hung relay cannot stall the triggering CalDAV request — the send is
// synchronous inside the PUT/DELETE.
type SMTPMailer struct {
	Addr string    // host:port
	Auth smtp.Auth // optional
	// Timeout bounds dial + the whole SMTP exchange. Zero means the same
	// default as SubmissionMailer.
	Timeout time.Duration
}

func (m *SMTPMailer) Send(from string, to []string, msg []byte) error {
	host, _, err := net.SplitHostPort(m.Addr)
	if err != nil {
		return fmt.Errorf("relay: bad address %q: %w", m.Addr, err)
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = defaultSubmissionTimeout
	}
	deadline := time.Now().Add(timeout)
	conn, err := (&net.Dialer{Deadline: deadline}).Dial("tcp", m.Addr)
	if err != nil {
		return fmt.Errorf("relay dial: %w", err)
	}
	conn.SetDeadline(deadline)
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("relay client: %w", err)
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("relay STARTTLS: %w", err)
		}
	}
	if m.Auth != nil {
		// smtp.PlainAuth's own guard still applies here: it refuses to send
		// credentials on a non-TLS connection to a non-localhost server.
		if err := client.Auth(m.Auth); err != nil {
			return fmt.Errorf("relay auth: %w", err)
		}
	}
	return transact(client, from, to, msg)
}

// IMipPlugin delivers scheduling messages to external (mailto:) recipients
// by email. Port of sabre's Schedule\IMipPlugin. It runs as an
// ExternalDeliverer consulted by the SchedulePlugin when local delivery
// doesn't apply.
type IMipPlugin struct {
	// SenderEmail is the server's own address. It is always the SMTP envelope
	// sender (MAIL FROM), so bounces return to the server and SPF aligns on the
	// server's domain. Unless SendAsOrganizer is set it is also the From header.
	SenderEmail string
	// SendAsOrganizer puts the scheduling sender's own address in the From
	// header — the organizer for invitations (REQUEST/CANCEL), the replying
	// attendee for replies (REPLY); always a local principal — while SenderEmail
	// stays the SMTP envelope sender (a split sender). Only safe when the relay
	// can DKIM-sign for those local domain(s), or receivers will fail DMARC; off
	// by default. A malformed sender address falls back to SenderEmail. When
	// off, From is SenderEmail and the sender's address goes in Reply-To.
	SendAsOrganizer bool
	// Mailer performs the actual send on the shared-relay path (server-
	// originated mail and resource replies, and every send when Submission is
	// nil).
	Mailer Mailer
	// Submission, when non-nil, enables send-as-logged-in-user: an invitation
	// or reply whose sender is the logged-in user is submitted to the mail
	// server authenticated as that user (the Core rule), so it is DKIM/
	// allowed_from-checked and metered against their sending limits. Nil keeps
	// the legacy shared-relay behavior for every message.
	Submission UserMailer
	// SenderAddresses returns the calendar-user addresses a principal may send
	// as, used to authorize that the logged-in user owns msg.Sender. Wired from
	// SchedulePlugin via UseScheduler; nil disables the per-user path.
	SenderAddresses func(principalURI string) []string
	// IsResourceSender reports whether a calendar-user address belongs to an
	// auto-schedule (resource/location) principal, whose replies have no
	// interactive login and are relayed through the server rather than sent as a
	// user. Wired via UseScheduler.
	IsResourceSender func(address string) bool
	// Metrics counts deliveries by outcome — the schedule-status branches
	// mapped directly, so only smtp_error reads as an incident — and times
	// the SMTP send. A nil Set records nothing.
	Metrics *metrics.Set
	// Logger records delivery outcomes: sends at Info, anything that means the
	// mail did NOT go out at Warn (the SCHEDULE-STATUS the client sees carries
	// the same diagnosis, but only the log survives in the journal). Nil uses
	// slog.Default(). Never receives credentials.
	Logger *slog.Logger
	// Durable, when non-nil, is the restart-surviving job queue (internal/jobs)
	// every background send goes on (JOB-QUEUE-PLAN.md): relay-identity sends
	// credential-free; per-user sends with the submission password encrypted at
	// rest (D12), so the send is off the request path, metered, and survives a
	// restart. Nil (an in-memory server with no DB) makes external sends
	// synchronous. Enqueue happens on the request path; the send runs in a worker.
	Durable DurableQueue
	// WriteBack patches a stored object's SCHEDULE-STATUS once a background send
	// settles (or a synchronous deferred send resolves inline). Wired from
	// SchedulePlugin.patchScheduleStatus via UseScheduler; nil no-ops.
	WriteBack func(path, method, recipient, status string)

	server *dav.Server
}

// DurableQueue is the narrow view of internal/jobs.Queue the plugin needs: it
// enqueues sends and reports whether it can encrypt a credential (so the per-
// user durable path is only taken when a key is configured — D12). The plugin
// stays ignorant of the backing store (D2).
type DurableQueue interface {
	Enqueue(ctx context.Context, worker string, args any, opts ...jobs.Opt) error
	// EncryptsCredentials reports whether a keyring is configured; when false the
	// per-user durable retry can't run (no key) and the caller falls back to the
	// synchronous send / D5b rather than persisting a plaintext or bypassing metering.
	EncryptsCredentials() bool
}

// imipWorker is the durable job's worker name (Oban-mappable — see the plan).
const imipWorker = "imip_send"

// imipJob is one durable send's args (JSONB). NO secret lives here (D8): the
// per-user submission password travels only through jobs.WithCredential into the
// encrypted columns. uid/recipient/method are the coalesce key; sequence/dtstamp
// order supersedes; object_path is empty when there is no status to write back.
type imipJob struct {
	EnvelopeFrom string `json:"envelope_from"`      // MAIL FROM: user address (per-user) or SenderEmail (relay)
	Username     string `json:"username,omitempty"` // SendAs login for per-user; empty for relay
	Recipient    string `json:"recipient"`          // bare address (SendAs/Mailer recipient + coalesce key)
	Method       string `json:"method"`
	UID          string `json:"uid"`
	Sequence     string `json:"sequence"`
	DTStamp      string `json:"dtstamp"`
	Outcome      string `json:"outcome"`               // success metric label (sent / sent_as_user / resource_relay)
	ObjectPath   string `json:"object_path,omitempty"` // stored object to write SCHEDULE-STATUS back onto; "" = none
	MessageB64   string `json:"message_b64"`           // fully-framed RFC822, base64 (D4)
}

// Terminal SCHEDULE-STATUS values (RFC 6638 §3.2.9). statusQueued is the
// pending state a message carries while it waits in the retry queue; the
// PUT/DELETE write-back replaces it with statusSent or statusFailed once the
// background send settles.
const (
	statusQueued      = "1.0;Scheduling message is queued for delivery"
	statusSent        = "1.1;Scheduling message is sent via iMip"
	statusFailed      = "5.1;Could not deliver invite by email"
	statusAuthFailed  = "5.3;Submission authentication failed"
	statusRateLimited = "5.3;Temporarily rejected by the submission server (sending limit or transient policy); message not sent"
)

// classifySendError maps a terminal submission failure to the SCHEDULE-STATUS
// and metric label it should carry: an auth rejection, a 4xx (over-limit or
// other transient policy), or any other error. Shared by the synchronous path
// (mapSubmitError) and the queued path's write-back, so a send that fails
// inline and one that fails after retries resolve to the same status.
func classifySendError(err error) (status, outcome string) {
	var tperr *textproto.Error
	if errors.As(err, &tperr) {
		switch {
		case tperr.Code == 530 || tperr.Code == 534 || tperr.Code == 535:
			return statusAuthFailed, "auth_error"
		case tperr.Code >= 400 && tperr.Code < 500:
			return statusRateLimited, "rate_limited"
		}
	}
	return statusFailed, "smtp_error"
}

// retryableSend reports whether a send that failed with err is worth retrying;
// attempts counts the failures so far, including the one that produced err. A
// 4xx SMTP rejection (greylisting, sending limits, transient policy) and any
// network-level error (dial, timeout, reset) are transient. A 5xx SMTP code is
// a permanent rejection — an auth failure in particular never gets better by
// repeating the same credential. An error we can't classify (a refused plaintext
// connection, a malformed address, a broken config) gets one retry in case it
// was a blip, not the whole schedule.
func retryableSend(err error, attempts int) bool {
	var tperr *textproto.Error
	if errors.As(err, &tperr) {
		return tperr.Code >= 400 && tperr.Code < 500
	}
	var nerr net.Error
	if errors.As(err, &nerr) {
		return true
	}
	return attempts <= 1
}

func (p *IMipPlugin) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// QueuesExternalSends reports whether async delivery will actually offload the
// send to the durable background queue. When false, a caller must not defer a
// send (the status would never leave pending): it delivers synchronously.
func (p *IMipPlugin) QueuesExternalSends() bool { return p.Durable != nil }

// writeStatus patches the terminal SCHEDULE-STATUS back onto the stored object,
// the one write-back both the durable worker and the synchronous fallback use. objectPath is empty for
// sends whose status isn't stored (a DELETE's cancels, a resource REPLY), where
// it no-ops. recipient is the mailto: form patchScheduleStatus matches against.
func (p *IMipPlugin) writeStatus(objectPath string, msg *ical.Message, status string) {
	if objectPath == "" || p.WriteBack == nil {
		return
	}
	p.WriteBack(objectPath, msg.Method, msg.Recipient, status)
}

// logDelivery mirrors a delivery outcome (the same string the metric counts)
// into the journal — the SCHEDULE-STATUS diagnosis is only visible to the
// CalDAV client, so without this a failed send leaves no server-side trace
// beyond a counter. Sends log at Info, the routine insignificant-change skip
// at Debug, and every outcome where no mail went out at Warn. err is the
// SMTP-layer error when there is one; credentials are never logged.
func (p *IMipPlugin) logDelivery(outcome string, msg *ical.Message, err error) {
	attrs := []any{
		"outcome", outcome, "method", msg.Method,
		"sender", msg.Sender, "recipient", msg.Recipient,
	}
	if msg.ScheduleStatus != "" {
		attrs = append(attrs, "status", msg.ScheduleStatus)
	}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	switch outcome {
	case "sent", "sent_as_user", "resource_relay":
		p.logger().Info("imip delivered", attrs...)
	case "queued":
		p.logger().Warn("imip queued for retry", attrs...)
	case "skipped":
		p.logger().Debug("imip skipped", attrs...)
	default:
		p.logger().Warn("imip not delivered", attrs...)
	}
}

func (p *IMipPlugin) PluginName() string { return "imip" }

func (p *IMipPlugin) Init(s *dav.Server) { p.server = s }

// UseScheduler wires the address/resource resolvers from the SchedulePlugin, so
// the per-user submission path can authorize senders (does the logged-in user
// own msg.Sender?) and detect resource-originated replies. Called from server
// wiring after both plugins are constructed.
func (p *IMipPlugin) UseScheduler(s *SchedulePlugin) {
	p.SenderAddresses = s.addressesForPrincipal
	p.IsResourceSender = func(address string) bool {
		uri := s.principalForAddress(address)
		return uri != "" && s.AutoSchedule != nil && s.AutoSchedule(uri)
	}
	// Both the durable worker and the synchronous fallback write terminal
	// statuses back through here, so they share one read-modify-write path
	// (serialized per object by the scheduler's in-process write-back lock — see
	// patchScheduleStatus for the single-instance caveat).
	p.WriteBack = s.patchScheduleStatus
}

// DeliverExternal implements ExternalDeliverer: it emails significant messages
// to mailto: recipients, sending synchronously and setting the schedule status.
// r carries the authenticated principal and the submission credential; when
// Submission is configured, the message is sent as the logged-in user (see
// deliverAsUser).
func (p *IMipPlugin) DeliverExternal(r *http.Request, msg *ical.Message) bool {
	return p.deliverExternal(r, msg, false, "")
}

// DeliverExternalAsync implements AsyncExternalDeliverer: same delivery, but the
// send is handed to a background queue so the triggering request (a DELETE
// fanning out CANCELs, or a PUT inviting many attendees) does not block on SMTP.
// objectPath is the stored object whose SCHEDULE-STATUS the terminal outcome is
// written back onto ("" when nothing is stored — a DELETE's cancels, a resource
// REPLY). The send settles in the background (RAM or durable tier) and patches
// the status then; a message that instead reached a terminal status *inline*
// (auth failure, invalid recipient, a queue-full fallback send) is written back
// here, so a deferred send is never left stuck pending.
func (p *IMipPlugin) DeliverExternalAsync(r *http.Request, msg *ical.Message, objectPath string) bool {
	handled := p.deliverExternal(r, msg, true, objectPath)
	if msg.ScheduleStatus != statusQueued {
		status := msg.ScheduleStatus
		if status == "" {
			status = statusFailed
		}
		p.writeStatus(objectPath, msg, status)
	}
	return handled
}

func (p *IMipPlugin) deliverExternal(r *http.Request, msg *ical.Message, background bool, objectPath string) bool {
	if !msg.SignificantChange {
		if msg.ScheduleStatus == "" {
			msg.ScheduleStatus = "1.0;We got the message, but it's not significant enough to warrant an email"
		}
		p.Metrics.IMipDelivery("skipped")
		p.logDelivery("skipped", msg, nil)
		return false
	}
	if !strings.HasPrefix(msg.Sender, "mailto:") || !strings.HasPrefix(msg.Recipient, "mailto:") {
		// Undeliverable by email (urn:uuid: or other non-mailto calendar-user
		// address). Still counted: an invitation that silently isn't sent is
		// exactly what this metric exists to surface.
		p.Metrics.IMipDelivery("unsupported")
		p.logDelivery("unsupported", msg, nil)
		return false
	}
	sender := strings.TrimPrefix(msg.Sender, "mailto:")
	recipient := strings.TrimPrefix(msg.Recipient, "mailto:")
	// Reject a recipient that is not a syntactically valid address, so
	// malformed attendee data can't reach the SMTP layer.
	if _, err := mail.ParseAddress(recipient); err != nil {
		msg.ScheduleStatus = "5.3;Recipient was not a valid email address"
		p.Metrics.IMipDelivery("invalid_recipient")
		p.logDelivery("invalid_recipient", msg, nil)
		return true
	}

	subject := "iTIP message"
	if summary := eventSummaryText(msg); summary != "" {
		switch strings.ToUpper(msg.Method) {
		case "REPLY":
			subject = "Re: " + summary
		case "REQUEST":
			subject = "Invitation: " + summary
		case "CANCEL":
			subject = "Cancelled: " + summary
		}
	}

	// Every value woven into the message below is derived from attacker-
	// controlled calendar data (SUMMARY, ORGANIZER/ATTENDEE CN and address,
	// METHOD). framedEmail strips CR/LF so a crafted event cannot inject extra
	// headers (Bcc, a forged body) into the outgoing message.
	body := ical.EncodeLenient(msg.Calendar)

	// With Submission configured, apply the Core rule: the logged-in user is
	// always the sender. Otherwise fall back to the legacy shared relay for
	// every message (non-mizu deployments).
	if p.Submission != nil {
		return p.deliverAsUser(r, msg, sender, recipient, subject, body, background, objectPath)
	}
	return p.deliverViaRelay(msg, sender, recipient, subject, body, "sent", background, objectPath)
}

// deliverAsUser routes one message under the Core rule (send-as-logged-in-user):
//   - msg.Sender is one of the logged-in user's addresses → submit as them;
//   - msg.Sender is an auto-schedule resource → relay through the server;
//   - otherwise (a delegated human organizer) → refuse, so we never send as
//     someone who is not the logged-in user.
func (p *IMipPlugin) deliverAsUser(r *http.Request, msg *ical.Message, sender, recipient, subject string, body []byte, background bool, objectPath string) bool {
	switch {
	case p.senderOwnedByPrincipal(msg.Sender, dav.Principal(r.Context())):
		// fall through to the per-user submission below.
	case p.IsResourceSender != nil && p.IsResourceSender(msg.Sender):
		// A resource (room) answering a remote organizer: no interactive login,
		// so relay through the shared server sender. The one sanctioned relay.
		return p.deliverViaRelay(msg, sender, recipient, subject, body, "resource_relay", background, objectPath)
	default:
		// The sender is not the logged-in user and not a resource — a delegated
		// human organizer. We hold only the logged-in user's credential and must
		// not send as another person, nor relay (that would bypass their limits).
		msg.ScheduleStatus = "5.3;Not authorized to send scheduling mail as this sender"
		p.Metrics.IMipDelivery("not_authorized_sender")
		p.logDelivery("not_authorized_sender", msg, nil)
		return true
	}

	// The AUTH identity is the login the user presented over CalDAV Basic (the
	// public username); the secret is the password stashed request-scoped by the
	// auth plugin. A login carrying no reusable plaintext (Digest) has no
	// credential, so we fail closed rather than fall back to the relay.
	username, _, hasBasic := r.BasicAuth()
	password := dav.SubmissionSecret(r.Context())
	if !hasBasic || password == "" {
		msg.ScheduleStatus = "5.3;No submission credential for the logged-in user"
		p.Metrics.IMipDelivery("no_credential")
		p.logDelivery("no_credential", msg, nil)
		return true
	}

	// From is the user's own identity (== the envelope sender), so From and the
	// in-body ORGANIZER stay one identity and no Reply-To split is needed.
	email := framedEmail([]string{"From: " + formatAddress(msg.SenderName, sender)},
		msg, recipient, subject, body)

	// Off the request path: enqueue the send durably with the password encrypted
	// at rest (D12), so it is metered, survives a restart, and doesn't block the
	// PUT/DELETE on SMTP. Needs a credential key (EncryptsCredentials). Without a
	// durable queue or a key, fall through to a synchronous attempt — the send
	// still goes out, it just blocks the request (the pre-queue behavior).
	if background && p.Durable != nil && p.Durable.EncryptsCredentials() {
		if p.enqueueDurable(msg, sender, username, recipient, "sent_as_user", objectPath, email, []byte(password)) {
			return true
		}
	}
	start := time.Now()
	err := p.Submission.SendAs(username, password, sender, []string{recipient}, email)
	p.Metrics.ObserveIMipSend(time.Since(start).Seconds())
	if err != nil {
		return p.mapSubmitError(msg, err)
	}
	msg.ScheduleStatus = statusSent
	p.Metrics.IMipDelivery("sent_as_user")
	p.logDelivery("sent_as_user", msg, nil)
	return true
}

// deliverViaRelay sends through the shared server relay (Mailer): the legacy
// path when Submission is nil, and the resource-reply path. successOutcome is
// the metric recorded on success ("sent" or "resource_relay").
func (p *IMipPlugin) deliverViaRelay(msg *ical.Message, sender, recipient, subject string, body []byte, successOutcome string, background bool, objectPath string) bool {
	// The envelope sender (MAIL FROM) is always the server's own address. In
	// SendAsOrganizer mode the visible From is the scheduling sender instead — a
	// split sender — so Reply-To would only duplicate it. Fall back to the
	// default framing when that address is malformed (a mis-provisioned
	// principal): an invalid From is rejected by strict relays, and the server
	// address keeps the invite deliverable.
	sendAsOrganizer := p.SendAsOrganizer
	if sendAsOrganizer {
		if _, err := mail.ParseAddress(sender); err != nil {
			sendAsOrganizer = false
		}
	}
	var leading []string
	if sendAsOrganizer {
		leading = []string{"From: " + formatAddress(msg.SenderName, sender)}
	} else {
		leading = []string{
			"Reply-To: " + formatAddress(msg.SenderName, sender),
			"From: " + formatAddress(msg.SenderName, p.SenderEmail),
		}
	}
	email := framedEmail(leading, msg, recipient, subject, body)

	if p.Mailer == nil {
		msg.ScheduleStatus = "5.2;No mailer configured"
		p.Metrics.IMipDelivery("no_mailer")
		p.logDelivery("no_mailer", msg, nil)
		return true
	}
	// Relay identity carries no credential, so the whole lifecycle lives on the
	// durable queue — restart-surviving for free. Without a durable queue (an
	// in-memory server), send synchronously.
	if background {
		if p.enqueueDurable(msg, p.SenderEmail, "", recipient, successOutcome, objectPath, email, nil) {
			return true
		}
	}
	start := time.Now()
	err := p.Mailer.Send(p.SenderEmail, []string{recipient}, email)
	// Observed on failure too — a hanging SMTP server is the interesting case.
	p.Metrics.ObserveIMipSend(time.Since(start).Seconds())
	if err != nil {
		msg.ScheduleStatus = statusFailed
		p.Metrics.IMipDelivery("smtp_error")
		p.logDelivery("smtp_error", msg, err)
		return true
	}
	msg.ScheduleStatus = statusSent
	p.Metrics.IMipDelivery(successOutcome)
	p.logDelivery(successOutcome, msg, nil)
	return true
}

// enqueueDurable persists a send on the durable queue and marks the message
// pending (1.0), off the request path. cred is nil for a relay job or the
// plaintext submission password for a per-user job (the queue encrypts it — D12;
// it never enters args). envelopeFrom is the MAIL FROM the worker will use;
// username is the SendAs login (empty for relay). Returns false when there is no
// durable queue or the enqueue failed, so the caller can fall back to a
// synchronous send. Runs on the request goroutine, so it owns msg.
func (p *IMipPlugin) enqueueDurable(msg *ical.Message, envelopeFrom, username, recipient, outcome, objectPath string, email, cred []byte) bool {
	if p.Durable == nil {
		return false
	}
	args := imipJob{
		EnvelopeFrom: envelopeFrom,
		Username:     username,
		Recipient:    recipient,
		Method:       msg.Method,
		UID:          msg.UID,
		Sequence:     eventSequence(msg),
		DTStamp:      eventProp(msg, "DTSTAMP"),
		Outcome:      outcome,
		ObjectPath:   objectPath,
		MessageB64:   base64.StdEncoding.EncodeToString(email),
	}
	// A fresh, short context so the enqueue (a quick local INSERT) is bounded
	// independent of the request's own deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var opts []jobs.Opt
	if cred != nil {
		opts = append(opts, jobs.WithCredential(cred))
	}
	if err := p.Durable.Enqueue(ctx, imipWorker, args, opts...); err != nil {
		p.logger().Warn("imip: durable enqueue failed; leaving send unqueued",
			"err", err, "method", msg.Method, "recipient", msg.Recipient)
		return false
	}
	msg.ScheduleStatus = statusQueued
	p.Metrics.IMipDelivery("queued")
	p.logDelivery("queued", msg, nil)
	return true
}

// DurableHandler is the internal/jobs handler for the "imip_send" worker: it
// re-sends the pre-framed RFC822 body (D4) and settles the SCHEDULE-STATUS.
// A per-user job (Credential present) submits as the user (metered); a relay
// job goes through the shared server sender. Result handling (D13): a success
// completes and writes statusSent; a transient failure reschedules until the
// attempts run out; a permanent failure (or exhaustion) writes the classified
// terminal status and discards. Registered by the server wiring.
func (p *IMipPlugin) DurableHandler(_ context.Context, j jobs.Job) error {
	var a imipJob
	if err := json.Unmarshal(j.Args, &a); err != nil {
		return jobs.Discard(fmt.Errorf("imip: unparseable job args: %w", err))
	}
	body, err := base64.StdEncoding.DecodeString(a.MessageB64)
	if err != nil {
		return jobs.Discard(fmt.Errorf("imip: bad message_b64: %w", err))
	}

	start := time.Now()
	var sendErr error
	if j.Credential != nil {
		if p.Submission == nil {
			return jobs.Discard(errors.New("imip: per-user job but no submission mailer"))
		}
		sendErr = p.Submission.SendAs(a.Username, string(j.Credential), a.EnvelopeFrom, []string{a.Recipient}, body)
	} else {
		if p.Mailer == nil {
			return jobs.Discard(errors.New("imip: relay job but no mailer"))
		}
		sendErr = p.Mailer.Send(a.EnvelopeFrom, []string{a.Recipient}, body)
	}
	p.Metrics.ObserveIMipSend(time.Since(start).Seconds())

	recipient := "mailto:" + a.Recipient
	attrs := []any{"outcome", a.Outcome, "method", a.Method, "recipient", recipient, "attempt", j.Attempt}
	if sendErr == nil {
		p.Metrics.IMipDelivery(a.Outcome)
		p.logger().Info("imip delivered (durable)", attrs...)
		p.writeBackStatus(a, statusSent)
		return nil
	}
	// Transient and attempts remain → reschedule (the queue applies backoff).
	if retryableSend(sendErr, j.Attempt) && j.Attempt < j.MaxAttempts {
		p.logger().Warn("imip durable retry scheduled", append(attrs, "err", sendErr)...)
		return sendErr
	}
	// Terminal: a permanent rejection, or a transient one out of retries.
	status, oc := classifySendError(sendErr)
	if retryableSend(sendErr, j.Attempt) {
		oc = "retry_exhausted"
	}
	p.Metrics.IMipDelivery(oc)
	p.logger().Warn("imip durable giving up", append(attrs, "err", sendErr, "status", status)...)
	p.writeBackStatus(a, status)
	return jobs.Discard(sendErr)
}

// writeBackStatus patches a durable job's terminal status onto its stored object
// (no-op when the job carried no object_path — a cancel or resource reply).
func (p *IMipPlugin) writeBackStatus(a imipJob, status string) {
	if a.ObjectPath == "" || p.WriteBack == nil {
		return
	}
	p.WriteBack(a.ObjectPath, a.Method, "mailto:"+a.Recipient, status)
}

// senderOwnedByPrincipal reports whether the full msg.Sender (a "mailto:" URI)
// is one of the addresses the principal may send as.
func (p *IMipPlugin) senderOwnedByPrincipal(sender, principal string) bool {
	if principal == "" || p.SenderAddresses == nil {
		return false
	}
	for _, addr := range p.SenderAddresses(principal) {
		if strings.EqualFold(addr, sender) {
			return true
		}
	}
	return false
}

// mapSubmitError translates a synchronous submission failure into a schedule-
// status and metric via the shared classifier: an auth rejection (5.3), an
// over-limit/temporary 4xx (5.3, the expected client-side rate_limited series,
// not alert-worthy smtp_error), or any other error (5.1).
func (p *IMipPlugin) mapSubmitError(msg *ical.Message, err error) bool {
	status, outcome := classifySendError(err)
	msg.ScheduleStatus = status
	p.Metrics.IMipDelivery(outcome)
	p.logDelivery(outcome, msg, err)
	return true
}

// framedEmail assembles the RFC822 message: leading holds the From/Reply-To
// framing, then the To/Subject/MIME/Content-Type tail and body. stripHeaderChars
// removes CR/LF from untrusted values so they can't inject headers.
func framedEmail(leading []string, msg *ical.Message, recipient, subject string, body []byte) []byte {
	headers := append(leading,
		"To: "+formatAddress(msg.RecipientName, recipient),
		"Subject: "+stripHeaderChars(subject),
		"MIME-Version: 1.0",
		"Content-Type: text/calendar; charset=UTF-8; method="+stripHeaderChars(msg.Method),
	)
	return []byte(strings.Join(headers, "\r\n") + "\r\n\r\n" + string(body))
}

func formatAddress(name, addr string) string {
	name, addr = stripHeaderChars(name), stripHeaderChars(addr)
	if name != "" && name != addr {
		return fmt.Sprintf("%s <%s>", name, addr)
	}
	return addr
}

// stripHeaderChars removes CR and LF so untrusted values (event summaries,
// attendee names, addresses) cannot inject extra email headers or a body.
func stripHeaderChars(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// eventSummaryText returns the SUMMARY of the message's first VEVENT.
func eventSummaryText(msg *ical.Message) string {
	return eventProp(msg, "SUMMARY")
}

// eventProp returns the named property of the message's first VEVENT ("" if
// absent). Used to lift the coalescing keys (SEQUENCE, DTSTAMP) into a durable
// job's args (D10).
func eventProp(msg *ical.Message, name string) string {
	if msg.Calendar == nil {
		return ""
	}
	for _, comp := range msg.Calendar.Children {
		if comp.Name == "VEVENT" {
			if pr := comp.Props.Get(name); pr != nil {
				return pr.Value
			}
		}
	}
	return ""
}

// eventSequence returns the first VEVENT's SEQUENCE, defaulting to "0" (the iCal
// default) so the coalesce ordering always has a value to compare.
func eventSequence(msg *ical.Message) string {
	if s := eventProp(msg, "SEQUENCE"); s != "" {
		return s
	}
	return "0"
}
