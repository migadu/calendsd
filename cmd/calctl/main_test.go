package main

import (
	"flag"
	"slices"
	"testing"
)

// TestParseGlobal pins the split between calctl's global flags and the
// subcommand's: global flags come before the command (either -config or
// --config, with = or a separate value) and everything after it is passed
// through untouched.
func TestParseGlobal(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		config  string
		command string
		sub     []string
	}{
		{[]string{"health"}, "", "health", []string{}},
		{[]string{"--config", "/c.toml", "health", "--json"}, "/c.toml", "health", []string{"--json"}},
		{[]string{"-config=/c.toml", "stats"}, "/c.toml", "stats", []string{}},
		{[]string{"principal", "add", "bob", "--email", "b@x"}, "", "principal", []string{"add", "bob", "--email", "b@x"}},
		{[]string{"panel", "--config", "/p.toml"}, "", "panel", []string{"--config", "/p.toml"}},
		{[]string{"--config", "/c.toml"}, "/c.toml", "", nil},
	} {
		config, command, sub := parseGlobal(flag.ContinueOnError, tc.args)
		if config != tc.config || command != tc.command || !slices.Equal(sub, tc.sub) {
			t.Errorf("parseGlobal(%q) = %q, %q, %q; want %q, %q, %q",
				tc.args, config, command, sub, tc.config, tc.command, tc.sub)
		}
	}
}
