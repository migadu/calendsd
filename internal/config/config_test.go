package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
# calserver config
[server]
addr = ":9090"            # inline comment
metrics_addr = "127.0.0.1:9464"
base_uri = "/dav/"
realm = "SabreDAV"

[server.tls]
cert = "/etc/calserver/tls.crt"
key = "/etc/calserver/tls.key"  # note the # is fine outside quotes

[database]
dsn = "postgres://localhost/calserver?sslmode=disable"

[scheduling.imip]
smtp = "mail.example.org:587"
sender = "calendar@example.org"
credential_key = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAEE="

[scheduling.imip.queue]
reclaim_after = "6m"
workers = 8

[calendarserver]
browser = true
auto_provision = false
per_user_props = true
auto_schedule_prefixes = ["resource", "location"]

[retention]
deleted_days = 14
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Addr != ":9090" {
		t.Errorf("Addr = %q", cfg.Server.Addr)
	}
	if cfg.Server.MetricsAddr != "127.0.0.1:9464" {
		t.Errorf("MetricsAddr = %q", cfg.Server.MetricsAddr)
	}
	if cfg.Server.BaseURI != "/dav/" {
		t.Errorf("BaseURI = %q", cfg.Server.BaseURI)
	}
	if cfg.Server.TLS.Cert != "/etc/calserver/tls.crt" || cfg.Server.TLS.Key != "/etc/calserver/tls.key" {
		t.Errorf("TLS = %+v", cfg.Server.TLS)
	}
	if cfg.Database.DSN != "postgres://localhost/calserver?sslmode=disable" {
		t.Errorf("DSN = %q", cfg.Database.DSN)
	}
	if cfg.Scheduling.IMip.SMTP != "mail.example.org:587" {
		t.Errorf("IMip.SMTP = %q", cfg.Scheduling.IMip.SMTP)
	}
	if cfg.Scheduling.IMip.Queue.ReclaimAfter != "6m" || cfg.Scheduling.IMip.Queue.Workers != 8 {
		t.Errorf("IMip.Queue = %+v", cfg.Scheduling.IMip.Queue)
	}
	if k, err := DecodeIMipCredentialKey(cfg.Scheduling.IMip.CredentialKey); err != nil || len(k) != 32 {
		t.Errorf("DecodeIMipCredentialKey = %d bytes, %v; want 32, nil", len(k), err)
	}
	if !cfg.CalendarServer.Browser || cfg.CalendarServer.AutoProvision || !cfg.CalendarServer.PerUserProps {
		t.Errorf("CalendarServer bools = %+v", cfg.CalendarServer)
	}
	if want := []string{"resource", "location"}; !reflect.DeepEqual(cfg.CalendarServer.AutoSchedulePrefixes, want) {
		t.Errorf("AutoSchedulePrefixes = %v, want %v", cfg.CalendarServer.AutoSchedulePrefixes, want)
	}
	if cfg.Retention.DeletedDays != 14 {
		t.Errorf("Retention.DeletedDays = %d, want 14", cfg.Retention.DeletedDays)
	}
}

func TestFindDefaultConfig(t *testing.T) {
	// Exercise the relative-path entry so the test never depends on (or
	// touches) the absolute /etc locations.
	dir := t.TempDir()
	t.Chdir(dir)

	if got := FindDefaultConfig(); got != "" {
		t.Fatalf("expected no config in empty dir, got %q", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[database]\ndsn = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := FindDefaultConfig(); got != "config.toml" {
		t.Fatalf("expected config.toml to be discovered, got %q", got)
	}
}

func TestDecodeIMipCredentialKey(t *testing.T) {
	// Empty is not an error — the feature is just off.
	if k, err := DecodeIMipCredentialKey(""); err != nil || k != nil {
		t.Errorf("empty: got %v, %v; want nil, nil", k, err)
	}
	// A valid 32-byte key decodes.
	valid := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if k, err := DecodeIMipCredentialKey("  " + valid + "  "); err != nil || len(k) != 32 {
		t.Errorf("valid: got %d bytes, %v; want 32, nil", len(k), err)
	}
	// Present-but-malformed fails closed: bad base64, and wrong length.
	if _, err := DecodeIMipCredentialKey("not@base64"); err == nil {
		t.Error("bad base64: want error")
	}
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := DecodeIMipCredentialKey(short); err == nil {
		t.Error("16-byte key: want error (must be 32)")
	}
}

func TestRetentionDays(t *testing.T) {
	// Non-positive falls back to the default; a positive value passes through.
	for _, tc := range []struct{ in, want int }{
		{0, DefaultRetentionDays}, {-5, DefaultRetentionDays}, {7, 7},
	} {
		if got := RetentionDays(tc.in); got != tc.want {
			t.Errorf("RetentionDays(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestLoadUnknownKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[server]\nbogus = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
}

func TestLoadWrongType(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, body string
	}{
		// The classic: a quoted boolean silently read as false before.
		{"quoted bool", "[logging]\naccess_log = \"True\"\n"},
		{"bool for string", "[server]\naddr = true\n"},
		{"string for int", "[retention]\ndeleted_days = \"30\"\n"},
		{"string for array", "[calendarserver]\nauto_schedule_prefixes = \"resource\"\n"},
	} {
		path := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: expected a type error, got nil", tc.name)
		}
	}
}

func TestParseValues(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want any
	}{
		{`"hi"`, "hi"},
		{`true`, true},
		{`false`, false},
		{`42`, 42},
		{`[]`, []string{}},
		{`["a", "b"]`, []string{"a", "b"}},
		{`"has # hash"`, "has # hash"},
		// Escapes are decoded, not kept verbatim.
		{`"pa\"ss\\word"`, `pa"ss\word`},
		{`"a\tb\nc"`, "a\tb\nc"},
		{`"café \U0001F600"`, "café 😀"},
		{`["a,b", "c\"d"]`, []string{"a,b", `c"d`}},
	} {
		got, err := parseValue(tc.in)
		if err != nil {
			t.Errorf("parseValue(%q): %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseValue(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{`"a\q"`, `"\u12"`, `"\uD800"`, `"open\"`, `"a" "b"`, `["a" "b"]`} {
		if v, err := parseValue(bad); err == nil {
			t.Errorf("parseValue(%q) = %#v, want an error", bad, v)
		}
	}
}

// An escaped quote must not end the string for comment stripping, or the
// rest of the value would be read as a comment.
func TestParseEscapedQuoteBeforeHash(t *testing.T) {
	flat, err := parse("[server.rcptd]\ntoken = \"a\\\"#b\" # comment\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := flat["server.rcptd.token"]; got != `a"#b` {
		t.Fatalf("token = %#v, want %q", got, `a"#b`)
	}
}
