package auth

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureHandler is a slog.Handler that records every emitted record so a test
// can assert on the level, message, and attributes of an auth log line. It
// reports Enabled for all levels so Debug lines are captured too, regardless of
// the process default.
type captureHandler struct{ records []slog.Record }

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// only returns the single captured record, failing if there is not exactly one.
func (h *captureHandler) only(t *testing.T) (slog.Record, map[string]string) {
	t.Helper()
	if len(h.records) != 1 {
		t.Fatalf("want exactly 1 log record, got %d", len(h.records))
	}
	rec := h.records[0]
	attrs := map[string]string{}
	rec.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
	return rec, attrs
}

// newTestPlugin builds a plugin whose only valid credentials are admin/secret,
// wired to a capturing logger.
func newTestPlugin() (*Plugin, *captureHandler) {
	cap := &captureHandler{}
	p := NewPlugin(&BasicBackend{
		Realm:    "SabreDAV",
		Validate: func(u, pw string) bool { return u == "admin" && pw == "secret" },
	})
	p.Logger = slog.New(cap)
	return p, cap
}

// TestAuthLogWrongPassword: a rejected login with credentials logs at Warn,
// names the user, gives the reason, and never leaks the password.
func TestAuthLogWrongPassword(t *testing.T) {
	p, cap := newTestPlugin()

	r := httptest.NewRequest("PROPFIND", "/calendars/admin/", nil)
	r.SetBasicAuth("admin", "hunter2")
	r.RemoteAddr = "10.0.0.9:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.5")

	proceed, err := p.BeforeMethod(httptest.NewRecorder(), r)
	if proceed || err == nil {
		t.Fatalf("wrong password should be rejected: proceed=%v err=%v", proceed, err)
	}

	rec, attrs := cap.only(t)
	if rec.Level != slog.LevelWarn {
		t.Errorf("level = %v, want WARN", rec.Level)
	}
	if rec.Message != "auth failed" {
		t.Errorf("message = %q, want %q", rec.Message, "auth failed")
	}
	if attrs["user"] != "admin" {
		t.Errorf("user attr = %q, want admin", attrs["user"])
	}
	if attrs["path"] != "/calendars/admin/" {
		t.Errorf("path attr = %q, want /calendars/admin/", attrs["path"])
	}
	if !strings.Contains(attrs["reason"], "incorrect") {
		t.Errorf("reason attr = %q, want it to mention the credential mismatch", attrs["reason"])
	}
	// The proxy-forwarded client and the direct peer both appear.
	if !strings.Contains(attrs["remote"], "203.0.113.5") || !strings.Contains(attrs["remote"], "10.0.0.9:5555") {
		t.Errorf("remote attr = %q, want both the forwarded and direct address", attrs["remote"])
	}
	// The password must never reach the log, in any field or the message.
	for k, v := range attrs {
		if strings.Contains(v, "hunter2") {
			t.Errorf("password leaked into %s attr: %q", k, v)
		}
	}
	if strings.Contains(rec.Message, "hunter2") {
		t.Error("password leaked into message")
	}
}

// TestAuthLogWrongDigestUser: a rejected Digest login is attributed to the
// username carried in the Digest header, so brute-force attempts over Digest
// are as visible as over Basic.
func TestAuthLogWrongDigestUser(t *testing.T) {
	cap := &captureHandler{}
	p := NewPlugin(&DigestBackend{
		Realm:  "SabreDAV",
		HashA1: func(string, string) (string, error) { return "", nil },
	})
	p.Logger = slog.New(cap)

	r := httptest.NewRequest("PROPFIND", "/", nil)
	r.Header.Set("Authorization",
		`Digest username="mallory", realm="SabreDAV", nonce="n", uri="/", qop=auth, nc=00000001, cnonce="c", response="r"`)
	if proceed, err := p.BeforeMethod(httptest.NewRecorder(), r); proceed || err == nil {
		t.Fatalf("wrong digest login should be rejected: proceed=%v err=%v", proceed, err)
	}

	rec, attrs := cap.only(t)
	if rec.Level != slog.LevelWarn {
		t.Errorf("level = %v, want WARN (credentials were presented)", rec.Level)
	}
	if attrs["user"] != "mallory" {
		t.Errorf("user attr = %q, want mallory (from the Digest header)", attrs["user"])
	}
}

// TestAuthLogNoCredentials: the routine pre-challenge probe (no Authorization
// header) is not a failed attempt, so it logs at Debug with an empty user, not
// at Warn.
func TestAuthLogNoCredentials(t *testing.T) {
	p, cap := newTestPlugin()

	r := httptest.NewRequest("PROPFIND", "/", nil)
	proceed, err := p.BeforeMethod(httptest.NewRecorder(), r)
	if proceed || err == nil {
		t.Fatalf("unauthenticated request should be rejected: proceed=%v err=%v", proceed, err)
	}

	rec, attrs := cap.only(t)
	if rec.Level != slog.LevelDebug {
		t.Errorf("level = %v, want DEBUG (a credential-less probe is not a failed login)", rec.Level)
	}
	if attrs["user"] != "" {
		t.Errorf("user attr = %q, want empty", attrs["user"])
	}
}

// TestAuthLogSuccess: a valid login logs at Debug and records the principal,
// not the password.
func TestAuthLogSuccess(t *testing.T) {
	p, cap := newTestPlugin()

	r := httptest.NewRequest("PROPFIND", "/calendars/admin/", nil)
	r.SetBasicAuth("admin", "secret")
	proceed, err := p.BeforeMethod(httptest.NewRecorder(), r)
	if !proceed || err != nil {
		t.Fatalf("valid login should proceed: proceed=%v err=%v", proceed, err)
	}

	rec, attrs := cap.only(t)
	if rec.Level != slog.LevelDebug {
		t.Errorf("level = %v, want DEBUG", rec.Level)
	}
	if rec.Message != "auth ok" {
		t.Errorf("message = %q, want %q", rec.Message, "auth ok")
	}
	if attrs["principal"] != "principals/admin" {
		t.Errorf("principal attr = %q, want principals/admin", attrs["principal"])
	}
	if strings.Contains(attrs["principal"]+rec.Message, "secret") {
		t.Error("password leaked into success log")
	}
}

// TestAuthLogDefaultsToGlobal confirms a plugin with no explicit Logger falls
// back to slog.Default() rather than panicking on nil.
func TestAuthLogDefaultsToGlobal(t *testing.T) {
	p := NewPlugin(&BasicBackend{Realm: "SabreDAV", Validate: func(string, string) bool { return false }})
	r := httptest.NewRequest("PROPFIND", "/", nil)
	r.SetBasicAuth("admin", "x")
	// Must not panic even though p.Logger is nil.
	if proceed, err := p.BeforeMethod(httptest.NewRecorder(), r); proceed || err == nil {
		t.Fatalf("rejected login expected: proceed=%v err=%v", proceed, err)
	}
}
