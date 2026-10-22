package caldav

// Share-invite email (docs/share-invite-email.md): when a {cs}share request
// newly grants a local principal a usable share, email them a plain-text
// notice with a link that opens calclient on the calendar. The share itself
// is already in effect (storage auto-accepts local sharees); the email is
// informational and strictly best effort — it never fails the share request.
//
// The request path only screens and enqueues. The durable worker re-checks
// everything against current state before each send attempt (the share still
// exists as the same instance, the mailbox is unchanged, the domain still
// opts in) and frames the message from current data, so a queued notice never
// reaches a removed sharee or promises edit rights after a downgrade.

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"calserver/internal/acl"
	davxml "calserver/internal/dav/xml"
	"calserver/internal/jobs"
	"calserver/internal/metrics"
)

// ShareNotifier emails a local principal that a calendar was shared with them.
// Best effort: it never fails the share request.
type ShareNotifier interface {
	NotifyShare(r *http.Request, n ShareNotice)
}

// ShareNotice is one newly usable share, as the sharing plugin reports it.
type ShareNotice struct {
	CalendarID     string // the shared calendars.id
	CalendarPath   string // the owner's calendar path (for logs)
	OwnerPrincipal string
	Principal      string // the sharee
	InstanceID     int64  // the sharee's calendarinstances.id (the generation)
	InstanceURI    string // the sharee's calendar segment in their own home
	Comment        string // the sharer's {cs}summary, uncapped
}

// SharePolicy reports a recipient domain's share_notify setting ("off" or
// "email"). domain is lowercased.
type SharePolicy interface {
	ShareNotify(ctx context.Context, domain string) (string, error)
}

// SharedInstance is the current state of a sharee's instance of a calendar.
type SharedInstance struct {
	ID             int64
	URI            string
	Access         int
	OwnerPrincipal string
	CalendarName   string // the owner's displayname for the calendar, or its URI
}

// ShareLookup reads current share state for the worker's revalidation.
type ShareLookup interface {
	// SharedInstance returns principal's instance of calendarID, or nil (and
	// no error) when they hold none.
	SharedInstance(ctx context.Context, calendarID, principal string) (*SharedInstance, error)
}

// PrincipalLookup resolves a principal's properties (displayname, the
// canonical {http://sabredav.org/ns}email-address). A nil principal with no
// error means it does not exist.
type PrincipalLookup interface {
	PrincipalByPath(ctx context.Context, path string) (*acl.Principal, error)
}

// ShareQueue is the narrow view of internal/jobs.Queue the notifier needs: a
// coalescing enqueue that keeps only the newest generation per key pending.
type ShareQueue interface {
	EnqueueLatest(ctx context.Context, worker, key string, generation int64, args any, opts ...jobs.Opt) error
}

// ShareNotifyWorker is the durable job's worker name.
const ShareNotifyWorker = "share_notify"

// shareCommentMax caps the sharer's comment in the email (runes).
const shareCommentMax = 500

// ShareMailer implements ShareNotifier over the relay Mailer and the durable
// queue. Every dependency is optional in the sense that a missing one skips
// the email with a metered reason; none is ever a request failure.
type ShareMailer struct {
	// Mailer is the shared server relay; SenderEmail its envelope sender and
	// From address. Share notices are server mail: never sent as the user.
	Mailer      Mailer
	SenderEmail string
	// WebURL is calclient's origin (e.g. https://calendar.example.org); the
	// link is WebURL + "/cal/calendar?cal=<uri>". Empty disables the feature.
	WebURL string

	Policy     SharePolicy
	Shares     ShareLookup
	Principals PrincipalLookup
	// IsResource reports an auto-schedule (room/resource) principal; nobody
	// reads their mail, so they never get a notice. Nil means none are.
	IsResource func(principalURI string) bool

	// Durable, when non-nil, carries each notice to a background worker.
	// Nil sends synchronously on the request path (a server with no queue).
	Durable ShareQueue
	Metrics *metrics.Set
	Logger  *slog.Logger
	// Now stamps the Date header; nil uses time.Now (tests pin it).
	Now func() time.Time
}

// shareJob is one durable notice's args (JSONB). No secret. The queue adds
// the coalesce key (calendar|principal) and generation (instance id).
type shareJob struct {
	CalendarID   string `json:"calendar_id"`
	CalendarPath string `json:"calendar_path"`
	Owner        string `json:"owner"`
	Principal    string `json:"principal"`
	InstanceID   int64  `json:"instance_id"`
	InstanceURI  string `json:"instance_uri"`
	Mailbox      string `json:"mailbox"` // canonical recipient at enqueue time
	Comment      string `json:"comment,omitempty"`
}

// shareSkip marks a decision not to send (a terminal, non-error outcome).
type shareSkip struct{ outcome string }

func (s *shareSkip) Error() string { return "share notice skipped: " + s.outcome }

// shareRetry marks a transient failure worth another attempt; outcome is the
// metric recorded if it turns out to be the last one.
type shareRetry struct {
	outcome string
	err     error
}

func (s *shareRetry) Error() string { return s.outcome + ": " + s.err.Error() }
func (s *shareRetry) Unwrap() error { return s.err }

func (m *ShareMailer) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

func (m *ShareMailer) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// NotifyShare screens a new share and enqueues (or, with no queue, sends) its
// notice. Every outcome is metered; nothing is returned to the caller.
func (m *ShareMailer) NotifyShare(r *http.Request, n ShareNotice) {
	job := shareJob{
		CalendarID: n.CalendarID, CalendarPath: n.CalendarPath, Owner: n.OwnerPrincipal,
		Principal: n.Principal, InstanceID: n.InstanceID, InstanceURI: n.InstanceURI,
		Comment: capComment(n.Comment),
	}
	mailbox, err := m.screen(r.Context(), job.Principal, job.Owner)
	if err != nil {
		m.record(job, err)
		return
	}
	job.Mailbox = mailbox

	if m.Durable == nil {
		// No queue: the same validation and framing the worker runs, once.
		m.record(job, m.deliver(r.Context(), job))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key := job.CalendarID + "|" + job.Principal
	if err := m.Durable.EnqueueLatest(ctx, ShareNotifyWorker, key, job.InstanceID, job); err != nil {
		m.record(job, &shareRetry{outcome: "enqueue_error", err: err})
		return
	}
	m.Metrics.ShareNotify("queued")
	m.logger().Debug("share notice queued", m.attrs(job)...)
}

// screen applies the checks that don't depend on the instance's current
// state: exclusions, the canonical mailbox, the recipient domain's policy,
// and the feature's own configuration. It returns the mailbox to send to.
func (m *ShareMailer) screen(ctx context.Context, principal, owner string) (string, error) {
	if principal == "" || principal == owner {
		return "", &shareSkip{outcome: "skipped_excluded"}
	}
	if m.IsResource != nil && m.IsResource(principal) {
		return "", &shareSkip{outcome: "skipped_excluded"}
	}
	mailbox, err := m.mailboxOf(ctx, principal)
	if err != nil {
		return "", err
	}
	policy := "off"
	if m.Policy != nil {
		p, err := m.Policy.ShareNotify(ctx, mailboxDomain(mailbox))
		if err != nil {
			return "", &shareRetry{outcome: "policy_error", err: err}
		}
		policy = p
	}
	if policy != "email" {
		return "", &shareSkip{outcome: "skipped_policy"}
	}
	if m.WebURL == "" {
		return "", &shareSkip{outcome: "no_web_url"}
	}
	if m.Mailer == nil {
		return "", &shareSkip{outcome: "no_mailer"}
	}
	return mailbox, nil
}

// mailboxOf resolves a principal's canonical mailbox — never the address the
// sharer typed, nor anything inferred from the principal URI.
func (m *ShareMailer) mailboxOf(ctx context.Context, principal string) (string, error) {
	if m.Principals == nil {
		return "", &shareSkip{outcome: "invalid_recipient"}
	}
	p, err := m.Principals.PrincipalByPath(ctx, principal)
	if err != nil {
		return "", &shareRetry{outcome: "lookup_error", err: err}
	}
	mailbox, ok := validMailbox(sharePrincipalEmail(p))
	if !ok {
		return "", &shareSkip{outcome: "invalid_recipient"}
	}
	return mailbox, nil
}

// deliver revalidates job against current state and sends it. nil means sent;
// a *shareSkip is a terminal decision not to send; a *shareRetry is transient;
// anything else is a terminal send failure.
func (m *ShareMailer) deliver(ctx context.Context, job shareJob) error {
	if m.Shares == nil {
		return &shareSkip{outcome: "skipped_obsolete"}
	}
	inst, err := m.Shares.SharedInstance(ctx, job.CalendarID, job.Principal)
	if err != nil {
		return &shareRetry{outcome: "lookup_error", err: err}
	}
	if inst == nil || inst.ID != job.InstanceID || inst.URI != job.InstanceURI ||
		(inst.Access != AccessRead && inst.Access != AccessReadWrite) {
		return &shareSkip{outcome: "skipped_obsolete"}
	}
	mailbox, err := m.screen(ctx, job.Principal, inst.OwnerPrincipal)
	if err != nil {
		return err
	}
	// The mailbox moved since enqueue: the notice was addressed to someone
	// who may no longer own that address, so drop it rather than redirect.
	if !strings.EqualFold(mailbox, job.Mailbox) {
		return &shareSkip{outcome: "skipped_obsolete"}
	}
	owner, err := m.Principals.PrincipalByPath(ctx, inst.OwnerPrincipal)
	if err != nil {
		return &shareRetry{outcome: "lookup_error", err: err}
	}
	email := m.frame(job, inst, owner, mailbox)
	if err := m.Mailer.Send(m.SenderEmail, []string{mailbox}, email); err != nil {
		return err
	}
	return nil
}

// DurableHandler is the internal/jobs handler for the share_notify worker.
func (m *ShareMailer) DurableHandler(ctx context.Context, j jobs.Job) error {
	var job shareJob
	if err := json.Unmarshal(j.Args, &job); err != nil {
		return jobs.Discard(fmt.Errorf("share_notify: unparseable job args: %w", err))
	}
	err := m.deliver(ctx, job)
	var skip *shareSkip
	var retry *shareRetry
	switch {
	case err == nil:
		m.record(job, nil)
		return nil
	case errors.As(err, &skip):
		m.record(job, err)
		return nil // a decision, not a failure: complete the job
	case errors.As(err, &retry):
		if j.Attempt < j.MaxAttempts {
			m.logger().Warn("share notice retry scheduled", append(m.attrs(job), "attempt", j.Attempt, "err", err)...)
			return err
		}
		m.record(job, &shareRetry{outcome: "retry_exhausted", err: err})
		return jobs.Discard(err)
	case retryableSend(err, j.Attempt) && j.Attempt < j.MaxAttempts:
		m.logger().Warn("share notice retry scheduled", append(m.attrs(job), "attempt", j.Attempt, "err", err)...)
		return err
	case retryableSend(err, j.Attempt):
		m.record(job, &shareRetry{outcome: "retry_exhausted", err: err})
		return jobs.Discard(err)
	default:
		m.record(job, err)
		return jobs.Discard(err)
	}
}

// record meters and logs one settled outcome: nil is a send, a skip is
// expected (Debug), anything else went wrong (Warn).
func (m *ShareMailer) record(job shareJob, err error) {
	attrs := m.attrs(job)
	var skip *shareSkip
	var retry *shareRetry
	switch {
	case err == nil:
		m.Metrics.ShareNotify("sent")
		m.logger().Info("share notice sent", attrs...)
	case errors.As(err, &skip):
		m.Metrics.ShareNotify(skip.outcome)
		level := slog.LevelDebug
		if skip.outcome == "invalid_recipient" || skip.outcome == "no_mailer" || skip.outcome == "no_web_url" {
			level = slog.LevelWarn // an opted-in domain the deployment can't serve
		}
		m.logger().Log(context.Background(), level, "share notice not sent", append(attrs, "outcome", skip.outcome)...)
	case errors.As(err, &retry):
		m.Metrics.ShareNotify(retry.outcome)
		m.logger().Warn("share notice not sent", append(attrs, "outcome", retry.outcome, "err", retry.err)...)
	default:
		m.Metrics.ShareNotify("smtp_error")
		m.logger().Warn("share notice not sent", append(attrs, "outcome", "smtp_error", "err", err)...)
	}
}

// attrs identifies a notice in logs: never the comment body.
func (m *ShareMailer) attrs(job shareJob) []any {
	return []any{"calendar", job.CalendarPath, "principal", job.Principal, "recipient", job.Mailbox, "instance", job.InstanceID}
}

// frame builds the RFC 5322 message. Header values go through net/mail and
// mime encoding (RFC 2047), which also neutralizes CR/LF; the body is plain
// text with no text/calendar part — there is no event to add.
func (m *ShareMailer) frame(job shareJob, inst *SharedInstance, owner *acl.Principal, mailbox string) []byte {
	ownerEmail, ownerOK := validMailbox(sharePrincipalEmail(owner))
	ownerName := oneLine(sharePrincipalName(owner))
	if ownerName == "" {
		if ownerOK {
			ownerName = ownerEmail
		} else {
			ownerName = strings.TrimPrefix(inst.OwnerPrincipal, "principals/")
		}
	}
	calName := oneLine(inst.CalendarName)
	if calName == "" {
		calName = inst.URI
	}

	headers := []string{
		"From: " + (&mail.Address{Name: ownerName + " via Calendar", Address: m.SenderEmail}).String(),
	}
	if ownerOK {
		headers = append(headers, "Reply-To: "+(&mail.Address{Address: ownerEmail}).String())
	}
	headers = append(headers,
		"To: "+(&mail.Address{Address: mailbox}).String(),
		"Subject: "+mime.QEncoding.Encode("utf-8", fmt.Sprintf("%s shared \u201c%s\u201d with you", ownerName, calName)),
		"Date: "+m.now().Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: 8bit",
	)

	who := ownerName
	if ownerOK && !strings.EqualFold(ownerName, ownerEmail) {
		who = fmt.Sprintf("%s (%s)", ownerName, ownerEmail)
	}
	access := "You can view events."
	if inst.Access == AccessReadWrite {
		access = "You can view and edit events."
	}
	var body []string
	body = append(body, fmt.Sprintf("%s shared the calendar \u201c%s\u201d with you.", who, calName), access, "")
	if c := strings.TrimSpace(job.Comment); c != "" {
		body = append(body, strings.Split(normalizeNewlines(c), "\n")...)
		body = append(body, "")
	}
	body = append(body,
		"Open it: "+shareLink(m.WebURL, inst.URI),
		"",
		"It's already in your calendar list. If you don't want it, open the",
		"invitations panel (bell icon) and choose Decline.",
	)
	return []byte(strings.Join(headers, "\r\n") + "\r\n\r\n" + strings.Join(body, "\r\n") + "\r\n")
}

// shareLink is calclient's deep link to the sharee's copy of the calendar.
func shareLink(webURL, instanceURI string) string {
	return strings.TrimRight(webURL, "/") + "/cal/calendar?cal=" + url.QueryEscape(instanceURI)
}

// validMailbox accepts exactly one bare addr-spec (no display name, no list).
func validMailbox(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s || !strings.Contains(s, "@") {
		return "", false
	}
	return s, true
}

// mailboxDomain is the lowercased domain of a validated mailbox.
func mailboxDomain(mailbox string) string {
	return strings.ToLower(mailbox[strings.LastIndexByte(mailbox, '@')+1:])
}

func sharePrincipalEmail(p *acl.Principal) string {
	if p == nil {
		return ""
	}
	s, _ := p.Props[xml.Name{Space: davxml.NSSabreDAV, Local: "email-address"}].(string)
	return s
}

func sharePrincipalName(p *acl.Principal) string {
	if p == nil {
		return ""
	}
	s, _ := p.Props[xml.Name{Space: davxml.NSDav, Local: "displayname"}].(string)
	return s
}

// capComment trims the sharer's comment to shareCommentMax runes and drops
// control characters other than line breaks and tabs.
func capComment(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r >= 0x20 && r != 0x7f {
			return r
		}
		return -1
	}, s)
	if utf8.RuneCountInString(s) <= shareCommentMax {
		return s
	}
	return string([]rune(s)[:shareCommentMax]) + "…"
}

func normalizeNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// oneLine collapses a value destined for a single header or sentence.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
