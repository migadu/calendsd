package config

import (
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
}
