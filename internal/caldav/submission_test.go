package caldav_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"calendsd/internal/caldav"
)

// selfSignedTLS returns a *tls.Config usable as both server (Certificates) and
// client (RootCAs + ServerName "localhost"), so a test can stand up a TLS SMTP
// server the SubmissionMailer will trust.
func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
		RootCAs:      pool,
		ServerName:   "localhost",
	}
}

// fakeSMTP is a one-shot SMTP submission server that records the exchange. It
// speaks just enough ESMTP for net/smtp's client: EHLO, STARTTLS, AUTH PLAIN,
// MAIL, RCPT, DATA, QUIT.
type fakeSMTP struct {
	tlsConf       *tls.Config
	implicit      bool // wrap the accepted conn in TLS immediately (SMTPS)
	offerSTARTTLS bool // advertise STARTTLS on the plaintext EHLO

	// captured
	authUser, authPass, mailFrom string
	rcpts                        []string
	data                         string
	authOverTLS                  bool // was TLS active when AUTH arrived?

	done chan struct{}
}

func (f *fakeSMTP) serve(ln net.Listener) {
	defer close(f.done)
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	tlsUp := false
	if f.implicit {
		tc := tls.Server(conn, f.tlsConf)
		if err := tc.Handshake(); err != nil {
			return
		}
		conn = tc
		tlsUp = true
	}

	reader := bufio.NewReader(conn)
	write := func(s string) { io.WriteString(conn, s) }
	write("220 fake ESMTP\r\n")
	inData := false
	var dataBuf strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				f.data = dataBuf.String()
				inData = false
				write("250 queued\r\n")
				continue
			}
			dataBuf.WriteString(line)
			dataBuf.WriteString("\r\n")
			continue
		}
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			if !tlsUp && f.offerSTARTTLS {
				write("250-fake\r\n250-STARTTLS\r\n250 AUTH PLAIN\r\n")
			} else {
				write("250-fake\r\n250 AUTH PLAIN\r\n")
			}
		case strings.HasPrefix(up, "STARTTLS"):
			write("220 go ahead\r\n")
			tc := tls.Server(conn, f.tlsConf)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn = tc
			reader = bufio.NewReader(conn)
			tlsUp = true
		case strings.HasPrefix(up, "AUTH PLAIN"):
			f.authOverTLS = tlsUp
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				if raw, err := base64.StdEncoding.DecodeString(fields[2]); err == nil {
					parts := strings.Split(string(raw), "\x00")
					if len(parts) == 3 {
						f.authUser, f.authPass = parts[1], parts[2]
					}
				}
			}
			write("235 authenticated\r\n")
		case strings.HasPrefix(up, "MAIL FROM:"):
			f.mailFrom = strings.TrimPrefix(line[len("MAIL FROM:"):], "")
			write("250 ok\r\n")
		case strings.HasPrefix(up, "RCPT TO:"):
			f.rcpts = append(f.rcpts, line[len("RCPT TO:"):])
			write("250 ok\r\n")
		case strings.HasPrefix(up, "DATA"):
			write("354 go ahead\r\n")
			inData = true
		case strings.HasPrefix(up, "QUIT"):
			write("221 bye\r\n")
			return
		default:
			write("250 ok\r\n")
		}
	}
}

// startFakeSMTP launches srv on a loopback listener and returns its address.
func startFakeSMTP(t *testing.T, srv *fakeSMTP) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv.done = make(chan struct{})
	go func() {
		defer ln.Close()
		srv.serve(ln)
	}()
	t.Cleanup(func() { <-srv.done })
	return ln.Addr().String()
}

func TestSubmissionMailerSTARTTLS(t *testing.T) {
	cfg := selfSignedTLS(t)
	srv := &fakeSMTP{tlsConf: cfg, offerSTARTTLS: true}
	addr := startFakeSMTP(t, srv)

	m := &caldav.SubmissionMailer{Addr: addr, Host: "localhost", TLSConfig: cfg}
	if err := m.SendAs("alice@example.org", "hunter2", "alice@example.org",
		[]string{"guest@example.com"}, []byte("Subject: hi\r\n\r\nbody\r\n")); err != nil {
		t.Fatalf("SendAs: %v", err)
	}
	<-srv.done

	if !srv.authOverTLS {
		t.Error("AUTH was accepted before STARTTLS — credentials would be sent in the clear")
	}
	if srv.authUser != "alice@example.org" || srv.authPass != "hunter2" {
		t.Errorf("auth = (%q, %q), want (alice@example.org, hunter2)", srv.authUser, srv.authPass)
	}
	if !strings.Contains(srv.mailFrom, "alice@example.org") {
		t.Errorf("MAIL FROM = %q, want to contain alice@example.org", srv.mailFrom)
	}
	if len(srv.rcpts) != 1 || !strings.Contains(srv.rcpts[0], "guest@example.com") {
		t.Errorf("RCPT = %v, want [guest@example.com]", srv.rcpts)
	}
	if !strings.Contains(srv.data, "body") {
		t.Errorf("DATA = %q, want to contain the message body", srv.data)
	}
}

func TestSubmissionMailerImplicitTLS(t *testing.T) {
	cfg := selfSignedTLS(t)
	srv := &fakeSMTP{tlsConf: cfg, implicit: true}
	addr := startFakeSMTP(t, srv)

	m := &caldav.SubmissionMailer{Addr: addr, Host: "localhost", Implicit: true, TLSConfig: cfg}
	if err := m.SendAs("bob@example.org", "s3cret", "bob@example.org",
		[]string{"guest@example.com"}, []byte("Subject: hi\r\n\r\nbody\r\n")); err != nil {
		t.Fatalf("SendAs: %v", err)
	}
	<-srv.done

	if !srv.authOverTLS {
		t.Error("AUTH not recorded as over TLS on an implicit-TLS connection")
	}
	if srv.authUser != "bob@example.org" || srv.authPass != "s3cret" {
		t.Errorf("auth = (%q, %q), want (bob@example.org, s3cret)", srv.authUser, srv.authPass)
	}
	if !strings.Contains(srv.data, "body") {
		t.Errorf("DATA = %q, want to contain the message body", srv.data)
	}
}

// TestSMTPMailerSends: the legacy relay mailer completes a plain (no-STARTTLS,
// no-auth) exchange — the classic local-relay configuration — end to end.
func TestSMTPMailerSends(t *testing.T) {
	srv := &fakeSMTP{}
	addr := startFakeSMTP(t, srv)

	m := &caldav.SMTPMailer{Addr: addr}
	if err := m.Send("server@example.org", []string{"guest@example.com"},
		[]byte("Subject: hi\r\n\r\nbody\r\n")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-srv.done

	if !strings.Contains(srv.mailFrom, "server@example.org") {
		t.Errorf("MAIL FROM = %q, want to contain server@example.org", srv.mailFrom)
	}
	if len(srv.rcpts) != 1 || !strings.Contains(srv.rcpts[0], "guest@example.com") {
		t.Errorf("RCPT = %v, want [guest@example.com]", srv.rcpts)
	}
	if !strings.Contains(srv.data, "body") {
		t.Errorf("DATA = %q, want to contain the message body", srv.data)
	}
}

// TestSMTPMailerTimesOut: like the submission mailer, the relay send runs
// inside the user's CalDAV request, so a relay that accepts and hangs must
// fail within the deadline.
func TestSMTPMailerTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() { // accept, send no greeting, hold the connection open
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-stop
	}()

	m := &caldav.SMTPMailer{Addr: ln.Addr().String(), Timeout: 200 * time.Millisecond}
	start := time.Now()
	err = m.Send("server@example.org", []string{"guest@example.com"}, []byte("body"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Send succeeded against a hanging server; want a timeout error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Send took %v; want the ~200ms deadline to bound it", elapsed)
	}
}

// TestSubmissionMailerTimesOut: the send is synchronous inside the user's
// CalDAV request, so a submission server that accepts the connection and then
// hangs must fail within the configured deadline, not stall the PUT until the
// kernel gives up.
func TestSubmissionMailerTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() { // accept, send no greeting, hold the connection open
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-stop
	}()

	m := &caldav.SubmissionMailer{Addr: ln.Addr().String(), Host: "localhost", Timeout: 200 * time.Millisecond}
	start := time.Now()
	err = m.SendAs("alice@example.org", "hunter2", "alice@example.org",
		[]string{"guest@example.com"}, []byte("body"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("SendAs succeeded against a hanging server; want a timeout error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("SendAs took %v; want the ~200ms deadline to bound it", elapsed)
	}
}

// TestSubmissionMailerRefusesWithoutSTARTTLS: a server that does not advertise
// STARTTLS must never receive credentials — the mailer errors out before AUTH.
func TestSubmissionMailerRefusesWithoutSTARTTLS(t *testing.T) {
	cfg := selfSignedTLS(t)
	srv := &fakeSMTP{tlsConf: cfg, offerSTARTTLS: false}
	addr := startFakeSMTP(t, srv)

	m := &caldav.SubmissionMailer{Addr: addr, Host: "localhost", TLSConfig: cfg}
	err := m.SendAs("alice@example.org", "hunter2", "alice@example.org",
		[]string{"guest@example.com"}, []byte("body"))
	if err == nil {
		t.Fatal("SendAs succeeded against a server without STARTTLS; want an error")
	}
	<-srv.done
	if srv.authUser != "" || srv.authPass != "" {
		t.Errorf("credentials leaked to a non-TLS server: user=%q pass=%q", srv.authUser, srv.authPass)
	}
}
