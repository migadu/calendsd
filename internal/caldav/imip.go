package caldav

import (
	"crypto/tls"
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
	// Queue, when non-nil, retries sends that failed with a transient error
	// (network trouble, 4xx rejection) in the background instead of losing the
	// message: the stored SCHEDULE-STATUS becomes 1.0 (pending) and the final
	// outcome lands in the log and metrics. Nil keeps single-attempt sends.
	Queue *SendQueue

	server *dav.Server
}

// Terminal SCHEDULE-STATUS values (RFC 6638 §3.2.9). statusQueued is the
// pending state a message carries while it waits in the retry queue; the
// PUT/DELETE write-back replaces it with statusSent or statusFailed once the
// background send settles.
const (
	statusQueued = "1.0;Scheduling message is queued for delivery"
	statusSent   = "1.1;Scheduling message is sent via iMip"
	statusFailed = "5.1;Could not deliver invite by email"
)

func (p *IMipPlugin) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// QueuesExternalSends reports whether async delivery will actually offload the
// send to the background queue. When false, a caller must not defer a send (the
// status would never leave pending): it should deliver synchronously instead.
func (p *IMipPlugin) QueuesExternalSends() bool { return p.Queue != nil }

// queueDone adapts a status write-back (onFinal) to the queue's boolean
// terminal callback: a delivered send resolves to statusSent, an exhausted one
// to statusFailed. Returns nil when there is nothing to report back.
func (p *IMipPlugin) queueDone(onFinal func(status string)) func(bool) {
	if onFinal == nil {
		return nil
	}
	return func(delivered bool) {
		if delivered {
			onFinal(statusSent)
		} else {
			onFinal(statusFailed)
		}
	}
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

// enqueueRetry hands a transiently-failed send to the queue and rewrites the
// message's status to pending. Returns false when there is no queue, the
// error is not worth retrying, or the queue refused the message (full or
// stopped) — leaving the terminal-failure handling to the caller.
func (p *IMipPlugin) enqueueRetry(attempt func() error, outcome string, msg *ical.Message, err error, onDone func(bool)) bool {
	if p.Queue == nil || !retryableSend(err, 1) {
		return false
	}
	if !p.Queue.EnqueueWithDone(attempt, outcome, msg.Method, msg.Sender, msg.Recipient, onDone) {
		return false
	}
	msg.ScheduleStatus = statusQueued
	p.Metrics.IMipDelivery("queued")
	p.logDelivery("queued", msg, err)
	return true
}

// enqueueFirst hands the very first send attempt to the queue, without a
// synchronous attempt first, so the triggering request returns without waiting
// on SMTP (used for cancels on DELETE). It returns false when there is no queue
// or the queue refused the message (full or stopped), leaving the caller to
// fall back to a synchronous send. On success the status is pending (1.0), and
// the terminal outcome lands in the log and metrics — exactly like a retry.
func (p *IMipPlugin) enqueueFirst(attempt func() error, outcome string, msg *ical.Message, onDone func(bool)) bool {
	if p.Queue == nil {
		return false
	}
	if !p.Queue.EnqueueWithDone(attempt, outcome, msg.Method, msg.Sender, msg.Recipient, onDone) {
		return false
	}
	msg.ScheduleStatus = statusQueued
	p.Metrics.IMipDelivery("queued")
	p.logDelivery("queued", msg, nil)
	return true
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
}

// DeliverExternal implements ExternalDeliverer: it emails significant messages
// to mailto: recipients, sending synchronously and setting the schedule status.
// r carries the authenticated principal and the submission credential; when
// Submission is configured, the message is sent as the logged-in user (see
// deliverAsUser).
func (p *IMipPlugin) DeliverExternal(r *http.Request, msg *ical.Message) bool {
	return p.deliverExternal(r, msg, false, nil)
}

// DeliverExternalAsync implements AsyncExternalDeliverer: same delivery, but the
// send is handed to the retry queue so the triggering request (a DELETE fanning
// out CANCELs, or a PUT inviting many attendees) does not block on SMTP. The
// credential and framed message are captured now; the send runs in the
// background. onFinal, when non-nil, is called exactly once with the terminal
// SCHEDULE-STATUS — inline for a synchronous outcome (auth failure, invalid
// recipient, or a queue-full fallback send), or later from the queue worker
// when the background send settles — so the caller can write it back onto the
// stored object. Falls back to a synchronous send when no queue is configured
// or the queue is full.
func (p *IMipPlugin) DeliverExternalAsync(r *http.Request, msg *ical.Message, onFinal func(status string)) bool {
	handled := p.deliverExternal(r, msg, true, p.queueDone(onFinal))
	// A message left in the pending state was accepted by the queue, which owns
	// the terminal callback; anything else reached a terminal status inline.
	if handled && onFinal != nil && msg.ScheduleStatus != statusQueued {
		onFinal(msg.ScheduleStatus)
	}
	return handled
}

func (p *IMipPlugin) deliverExternal(r *http.Request, msg *ical.Message, background bool, onDone func(bool)) bool {
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
		return p.deliverAsUser(r, msg, sender, recipient, subject, body, background, onDone)
	}
	return p.deliverViaRelay(msg, sender, recipient, subject, body, "sent", background, onDone)
}

// deliverAsUser routes one message under the Core rule (send-as-logged-in-user):
//   - msg.Sender is one of the logged-in user's addresses → submit as them;
//   - msg.Sender is an auto-schedule resource → relay through the server;
//   - otherwise (a delegated human organizer) → refuse, so we never send as
//     someone who is not the logged-in user.
func (p *IMipPlugin) deliverAsUser(r *http.Request, msg *ical.Message, sender, recipient, subject string, body []byte, background bool, onDone func(bool)) bool {
	switch {
	case p.senderOwnedByPrincipal(msg.Sender, dav.Principal(r.Context())):
		// fall through to the per-user submission below.
	case p.IsResourceSender != nil && p.IsResourceSender(msg.Sender):
		// A resource (room) answering a remote organizer: no interactive login,
		// so relay through the shared server sender. The one sanctioned relay.
		return p.deliverViaRelay(msg, sender, recipient, subject, body, "resource_relay", background, onDone)
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
	// The attempt closure holds the credential in memory only, for as long as
	// the message stays queued — nothing request-scoped is referenced, so it is
	// safe to run after the triggering request returns.
	attempt := func() error {
		return p.Submission.SendAs(username, password, sender, []string{recipient}, email)
	}
	if background && p.enqueueFirst(attempt, "sent_as_user", msg, onDone) {
		return true
	}
	start := time.Now()
	err := attempt()
	p.Metrics.ObserveIMipSend(time.Since(start).Seconds())
	if err != nil {
		if p.enqueueRetry(attempt, "sent_as_user", msg, err, onDone) {
			return true
		}
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
func (p *IMipPlugin) deliverViaRelay(msg *ical.Message, sender, recipient, subject string, body []byte, successOutcome string, background bool, onDone func(bool)) bool {
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
	attempt := func() error {
		return p.Mailer.Send(p.SenderEmail, []string{recipient}, email)
	}
	if background && p.enqueueFirst(attempt, successOutcome, msg, onDone) {
		return true
	}
	start := time.Now()
	err := attempt()
	// Observed on failure too — a hanging SMTP server is the interesting case.
	p.Metrics.ObserveIMipSend(time.Since(start).Seconds())
	if err != nil {
		if p.enqueueRetry(attempt, successOutcome, msg, err, onDone) {
			return true
		}
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

// mapSubmitError translates a submission failure into a schedule-status and
// metric: an auth rejection, an over-limit/temporary 4xx, or any other error.
func (p *IMipPlugin) mapSubmitError(msg *ical.Message, err error) bool {
	var tperr *textproto.Error
	if errors.As(err, &tperr) {
		switch {
		case tperr.Code == 530 || tperr.Code == 534 || tperr.Code == 535:
			msg.ScheduleStatus = "5.3;Submission authentication failed"
			p.Metrics.IMipDelivery("auth_error")
			p.logDelivery("auth_error", msg, err)
			return true
		case tperr.Code >= 400 && tperr.Code < 500:
			// Any 4xx: over the sending limit, but also greylisting or another
			// transient policy rejection — the server can't tell them apart, so
			// the status must not claim a specific cause. All are the expected,
			// client-side rate_limited series (not alert-worthy smtp_error).
			msg.ScheduleStatus = "5.3;Temporarily rejected by the submission server (sending limit or transient policy); message not sent"
			p.Metrics.IMipDelivery("rate_limited")
			p.logDelivery("rate_limited", msg, err)
			return true
		}
	}
	msg.ScheduleStatus = statusFailed
	p.Metrics.IMipDelivery("smtp_error")
	p.logDelivery("smtp_error", msg, err)
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
	if msg.Calendar == nil {
		return ""
	}
	for _, comp := range msg.Calendar.Children {
		if comp.Name == "VEVENT" {
			if s := comp.Props.Get("SUMMARY"); s != nil {
				return s.Value
			}
		}
	}
	return ""
}
