package caldav

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"time"
)

// UserMailer submits a message to an SMTP submission server authenticated as a
// specific user, so the mail is sent *as* the logged-in user — the submission
// tier (mizu_out) then enforces allowed_from, DKIM-signs, and meters it against
// that user's sending limits. It is distinct from Mailer, whose Send is the
// unauthenticated relay path used for server-originated and resource mail.
type UserMailer interface {
	SendAs(username, password, from string, to []string, msg []byte) error
}

// SubmissionMailer implements UserMailer against an SMTP submission server.
//
// Transport is TLS-only, by construction: with Implicit set it dials implicit
// TLS (SMTPS, :465); otherwise it requires the server to advertise STARTTLS
// (:587) and upgrades before doing anything else. Credentials are never sent
// on an unencrypted connection — SendAs re-checks TLSConnectionState before
// authenticating (net/smtp's own PlainAuth guard can't see a manually dialled
// implicit-TLS connection, so we guard explicitly and uniformly for both ports).
type SubmissionMailer struct {
	// Addr is the submission server host:port, e.g. "smtp.example.org:465".
	Addr string
	// Implicit selects implicit TLS (SMTPS, typically :465). False uses
	// STARTTLS (typically :587).
	Implicit bool
	// Host is the TLS server name and SMTP client hostname. Defaults to the
	// host part of Addr.
	Host string
	// TLSConfig overrides the TLS settings. Nil verifies against the system
	// roots using Host as the server name. Tests inject a config trusting a
	// local test CA.
	TLSConfig *tls.Config
	// Timeout bounds the whole submission — dial, TLS, and every SMTP
	// exchange share one deadline — so a hung submission server cannot stall
	// the triggering CalDAV request indefinitely (the send is synchronous
	// inside the PUT/DELETE). Zero means defaultSubmissionTimeout.
	Timeout time.Duration
}

// defaultSubmissionTimeout is deliberately tight: the submission tier is
// adjacent infrastructure, and the user's PUT is blocked while we wait.
const defaultSubmissionTimeout = 10 * time.Second

func (m *SubmissionMailer) timeout() time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	return defaultSubmissionTimeout
}

// SendAs authenticates as username/password and submits msg with envelope
// sender from to the given recipients. username is the SMTP AUTH identity (the
// user's login); from is the envelope MAIL FROM (the user's own address, which
// allowed_from must permit). Any error means the message was not accepted.
func (m *SubmissionMailer) SendAs(username, password, from string, to []string, msg []byte) error {
	host := m.Host
	if host == "" {
		h, _, err := net.SplitHostPort(m.Addr)
		if err != nil {
			return fmt.Errorf("submission: bad address %q: %w", m.Addr, err)
		}
		host = h
	}
	tlsConf := m.TLSConfig
	if tlsConf == nil {
		tlsConf = &tls.Config{ServerName: host}
	} else if tlsConf.ServerName == "" {
		tlsConf = tlsConf.Clone()
		tlsConf.ServerName = host
	}

	client, err := m.dial(host, tlsConf)
	if err != nil {
		return err
	}
	defer client.Close()

	// Defense in depth: never transmit AUTH on a connection that is not in fact
	// encrypted, whichever transport was configured.
	if _, ok := client.TLSConnectionState(); !ok {
		return errors.New("submission: refusing to authenticate on a non-TLS connection")
	}
	if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
		return fmt.Errorf("submission auth: %w", err)
	}
	return transact(client, from, to, msg)
}

// transact runs the MAIL FROM / RCPT TO / DATA sequence on an established
// client. The server accepts the message when DATA gets its 250, so a failure
// after that (QUIT) is not reported as non-delivery: the schedule-status would
// read as "not sent" and the client could resend an already-delivered message.
func transact(client *smtp.Client, from string, to []string, msg []byte) error {
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp RCPT TO %q: %w", rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close: %w", err)
	}
	_ = client.Quit()
	return nil
}

// dial returns an *smtp.Client on a TLS connection, or an error. It never
// returns a plaintext client: implicit TLS is dialled directly, and the
// STARTTLS path refuses to continue if the server does not advertise the
// extension (so credentials can't be sent in the clear on a downgraded
// connection). The returned client's connection carries a deadline covering
// the whole session (see Timeout).
func (m *SubmissionMailer) dial(host string, tlsConf *tls.Config) (*smtp.Client, error) {
	deadline := time.Now().Add(m.timeout())
	dialer := &net.Dialer{Deadline: deadline}
	if m.Implicit {
		conn, err := tls.DialWithDialer(dialer, "tcp", m.Addr, tlsConf)
		if err != nil {
			return nil, fmt.Errorf("submission dial (implicit TLS): %w", err)
		}
		conn.SetDeadline(deadline)
		client, err := smtp.NewClient(conn, host)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("submission client: %w", err)
		}
		return client, nil
	}
	conn, err := dialer.Dial("tcp", m.Addr)
	if err != nil {
		return nil, fmt.Errorf("submission dial (STARTTLS): %w", err)
	}
	conn.SetDeadline(deadline)
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("submission client: %w", err)
	}
	if ok, _ := client.Extension("STARTTLS"); !ok {
		client.Close()
		return nil, errors.New("submission: server does not advertise STARTTLS; refusing to send credentials in the clear")
	}
	if err := client.StartTLS(tlsConf); err != nil {
		client.Close()
		return nil, fmt.Errorf("submission STARTTLS: %w", err)
	}
	return client, nil
}
