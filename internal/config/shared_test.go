package config

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolveWith runs SharedServerFlags against a fresh default FlagSet and the
// given config file contents, as calserver and calctl panel do at startup.
func resolveWith(t *testing.T, content string) (*Merged, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := flag.CommandLine
	t.Cleanup(func() { flag.CommandLine = saved })
	flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
	resolve := SharedServerFlags("dsn")
	if err := flag.CommandLine.Parse([]string{"-config", path}); err != nil {
		t.Fatal(err)
	}
	return resolve()
}

// [push] enabled without its keys used to pass config resolution and panic
// inside server composition; it must be an ordinary config error.
func TestResolveRejectsPushWithoutKeys(t *testing.T) {
	_, err := resolveWith(t, "[push]\nenabled = true\nvapid_public_key = \"pub\"\n")
	if err == nil || !strings.Contains(err.Error(), "[push]") {
		t.Fatalf("resolve = %v, want a [push] config error", err)
	}
	m, err := resolveWith(t, "[push]\nenabled = true\nvapid_public_key = \"pub\"\nvapid_private_key = \"priv\"\ntopic_secret = \"s\"\n")
	if err != nil || !m.Server.Push.Enabled {
		t.Fatalf("complete [push] config: %v", err)
	}
}
