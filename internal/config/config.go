// Package config loads calserver settings from a TOML file. It parses the
// small subset of TOML the config uses (comments, [section] and [a.b] tables,
// and string/bool/integer/string-array values) without a third-party
// dependency. Command-line flags override any value set here.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the full calserver configuration, matching config.toml.example.
// Zero values mean "unset" so callers can tell a real value from a default.
type Config struct {
	Server struct {
		Addr    string
		BaseURI string
		Realm   string
		TLS     struct {
			Cert string
			Key  string
		}
		// Rcptd, when URL is set, authenticates via an rcptd daemon's /auth
		// endpoint instead of the users table (see [server.rcptd]).
		Rcptd struct {
			URL   string
			Token string
		}
	}
	Database struct {
		DSN string
	}
	// Logging holds request-debugging options. Off by default.
	Logging struct {
		RequestDir string
	}
	Scheduling struct {
		IMip struct {
			SMTP   string
			Sender string
		}
	}
	// CalendarServer holds the Calendar-Server compatibility extensions.
	// Every one defaults to false/empty (sabre parity) when omitted.
	CalendarServer struct {
		Browser              bool
		AutoProvision        bool
		PerUserProps         bool
		ExpandGroups         bool
		PrincipalsReadable   bool
		AutoSchedulePrefixes []string
	}
}

// Load reads and parses a TOML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	flat, err := parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	cfg := &Config{}
	str := func(key string) string { s, _ := flat[key].(string); return s }
	boolean := func(key string) bool { b, _ := flat[key].(bool); return b }
	list := func(key string) []string { l, _ := flat[key].([]string); return l }

	cfg.Server.Addr = str("server.addr")
	cfg.Server.BaseURI = str("server.base_uri")
	cfg.Server.Realm = str("server.realm")
	cfg.Server.TLS.Cert = str("server.tls.cert")
	cfg.Server.TLS.Key = str("server.tls.key")
	cfg.Server.Rcptd.URL = str("server.rcptd.url")
	cfg.Server.Rcptd.Token = str("server.rcptd.token")
	cfg.Database.DSN = str("database.dsn")
	cfg.Logging.RequestDir = str("logging.request_dir")
	cfg.Scheduling.IMip.SMTP = str("scheduling.imip.smtp")
	cfg.Scheduling.IMip.Sender = str("scheduling.imip.sender")
	cfg.CalendarServer.Browser = boolean("calendarserver.browser")
	cfg.CalendarServer.AutoProvision = boolean("calendarserver.auto_provision")
	cfg.CalendarServer.PerUserProps = boolean("calendarserver.per_user_props")
	cfg.CalendarServer.ExpandGroups = boolean("calendarserver.expand_groups")
	cfg.CalendarServer.PrincipalsReadable = boolean("calendarserver.principals_readable")
	cfg.CalendarServer.AutoSchedulePrefixes = list("calendarserver.auto_schedule_prefixes")

	for key := range flat {
		if !known[key] {
			return nil, fmt.Errorf("%s: unknown config key %q", path, key)
		}
	}
	return cfg, nil
}

// known is the set of recognised "section.key" paths; anything else is a typo
// worth reporting rather than silently ignoring.
var known = map[string]bool{
	"server.addr": true, "server.base_uri": true, "server.realm": true,
	"server.tls.cert": true, "server.tls.key": true,
	"server.rcptd.url": true, "server.rcptd.token": true,
	"database.dsn":            true,
	"logging.request_dir":     true,
	"scheduling.imip.smtp":    true, "scheduling.imip.sender": true,
	"calendarserver.browser": true, "calendarserver.auto_provision": true,
	"calendarserver.per_user_props": true, "calendarserver.expand_groups": true,
	"calendarserver.principals_readable":   true,
	"calendarserver.auto_schedule_prefixes": true,
}

// parse reads the TOML subset into a flat map keyed by "section.key".
func parse(text string) (map[string]any, error) {
	out := map[string]any{}
	section := ""
	for n, raw := range strings.Split(text, "\n") {
		line := stripComment(raw)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("line %d: malformed section header %q", n+1, line)
			}
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("line %d: expected key = value, got %q", n+1, line)
		}
		key := strings.TrimSpace(line[:eq])
		if section != "" {
			key = section + "." + key
		}
		value, err := parseValue(strings.TrimSpace(line[eq+1:]))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		out[key] = value
	}
	return out, nil
}

// stripComment removes a trailing "# comment", honouring quoted strings so a
// '#' inside a value is left alone.
func stripComment(line string) string {
	inQuote := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case '#':
			if !inQuote {
				return line[:i]
			}
		}
	}
	return line
}

func parseValue(s string) (any, error) {
	switch {
	case s == "true":
		return true, nil
	case s == "false":
		return false, nil
	case strings.HasPrefix(s, `"`):
		return unquote(s)
	case strings.HasPrefix(s, "["):
		return parseArray(s)
	default:
		if i, err := strconv.Atoi(s); err == nil {
			return i, nil
		}
		return nil, fmt.Errorf("unrecognised value %q", s)
	}
}

func unquote(s string) (string, error) {
	if len(s) < 2 || !strings.HasPrefix(s, `"`) || !strings.HasSuffix(s, `"`) {
		return "", fmt.Errorf("unterminated string %q", s)
	}
	return s[1 : len(s)-1], nil
}

// parseArray reads a single-line array of quoted strings, e.g.
// ["resource", "location"].
func parseArray(s string) ([]string, error) {
	if !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("unterminated array %q (arrays must be single-line)", s)
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []string{}, nil
	}
	var out []string
	for _, part := range strings.Split(inner, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := unquote(part)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
