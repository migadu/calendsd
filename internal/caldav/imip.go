package caldav

import (
	"fmt"
	"net/mail"
	"net/smtp"
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

// SMTPMailer sends mail through an SMTP server (net/smtp).
type SMTPMailer struct {
	Addr string    // host:port
	Auth smtp.Auth // optional
}

func (m *SMTPMailer) Send(from string, to []string, msg []byte) error {
	return smtp.SendMail(m.Addr, m.Auth, from, to, msg)
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
	// Mailer performs the actual send.
	Mailer Mailer
	// Metrics counts deliveries by outcome — the schedule-status branches
	// mapped directly, so only smtp_error reads as an incident — and times
	// the SMTP send. A nil Set records nothing.
	Metrics *metrics.Set

	server *dav.Server
}

func (p *IMipPlugin) PluginName() string { return "imip" }

func (p *IMipPlugin) Init(s *dav.Server) { p.server = s }

// DeliverExternal implements ExternalDeliverer: it emails significant
// messages to mailto: recipients, setting the schedule status.
func (p *IMipPlugin) DeliverExternal(msg *ical.Message) bool {
	if !msg.SignificantChange {
		if msg.ScheduleStatus == "" {
			msg.ScheduleStatus = "1.0;We got the message, but it's not significant enough to warrant an email"
		}
		p.Metrics.IMipDelivery("skipped")
		return false
	}
	if !strings.HasPrefix(msg.Sender, "mailto:") || !strings.HasPrefix(msg.Recipient, "mailto:") {
		// Undeliverable by email (urn:uuid: or other non-mailto calendar-user
		// address). Still counted: an invitation that silently isn't sent is
		// exactly what this metric exists to surface.
		p.Metrics.IMipDelivery("unsupported")
		return false
	}
	sender := strings.TrimPrefix(msg.Sender, "mailto:")
	recipient := strings.TrimPrefix(msg.Recipient, "mailto:")
	// Reject a recipient that is not a syntactically valid address, so
	// malformed attendee data can't reach the SMTP layer.
	if _, err := mail.ParseAddress(recipient); err != nil {
		msg.ScheduleStatus = "5.3;Recipient was not a valid email address"
		p.Metrics.IMipDelivery("invalid_recipient")
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

	// Every value below is derived from attacker-controlled calendar data
	// (SUMMARY, ORGANIZER/ATTENDEE CN and address, METHOD). Strip CR/LF so a
	// crafted event cannot inject additional headers (Bcc, a forged body) into
	// the outgoing message.
	body := ical.EncodeLenient(msg.Calendar)
	// The envelope sender (MAIL FROM, passed to Mailer.Send below) is always the
	// server's own address. In SendAsOrganizer mode the visible From is the
	// scheduling sender instead — a split sender — so Reply-To would only
	// duplicate it. Fall back to the default framing when that address is
	// malformed (a mis-provisioned principal): an invalid From is rejected by
	// strict relays, and the server address keeps the invite deliverable.
	sendAsOrganizer := p.SendAsOrganizer
	if sendAsOrganizer {
		if _, err := mail.ParseAddress(sender); err != nil {
			sendAsOrganizer = false
		}
	}
	var headers []string
	if sendAsOrganizer {
		headers = append(headers, "From: "+formatAddress(msg.SenderName, sender))
	} else {
		headers = append(headers,
			"Reply-To: "+formatAddress(msg.SenderName, sender),
			"From: "+formatAddress(msg.SenderName, p.SenderEmail),
		)
	}
	headers = append(headers,
		"To: "+formatAddress(msg.RecipientName, recipient),
		"Subject: "+stripHeaderChars(subject),
		"MIME-Version: 1.0",
		"Content-Type: text/calendar; charset=UTF-8; method="+stripHeaderChars(msg.Method),
	)
	email := []byte(strings.Join(headers, "\r\n") + "\r\n\r\n" + string(body))

	if p.Mailer == nil {
		msg.ScheduleStatus = "5.2;No mailer configured"
		p.Metrics.IMipDelivery("no_mailer")
		return true
	}
	start := time.Now()
	err := p.Mailer.Send(p.SenderEmail, []string{recipient}, email)
	// Observed on failure too — a hanging SMTP server is the interesting case.
	p.Metrics.ObserveIMipSend(time.Since(start).Seconds())
	if err != nil {
		msg.ScheduleStatus = "5.1;Could not deliver invite by email"
		p.Metrics.IMipDelivery("smtp_error")
		return true
	}
	msg.ScheduleStatus = "1.1;Scheduling message is sent via iMip"
	p.Metrics.IMipDelivery("sent")
	return true
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
