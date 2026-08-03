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
		Addr string
		// MetricsAddr, when set, serves Prometheus /metrics and
		// /debug/pprof on a second plain-HTTP listener. Bind loopback or a
		// firewalled interface; it has no TLS and no auth.
		MetricsAddr string
		BaseURI     string
		Realm       string
		TLS         struct {
			Cert string
			Key  string
		}
		// Rcptd, when URL is set, authenticates via an rcptd daemon's /auth
		// endpoint instead of the users table (see [server.rcptd]).
		Rcptd struct {
			URL   string
			Token string
		}
		// TempFileFilter intercepts OS junk files (._*, .DS_Store, ...) into a
		// scratch directory instead of the backend, like sabre's
		// TemporaryFileFilterPlugin. TempFileDir is that directory (defaults to
		// a subdir of the system temp dir when empty). Off by default.
		TempFileFilter bool
		TempFileDir    string
	}
	Database struct {
		DSN string
		// Connection-pool bounds. All optional; zero/empty means the
		// storage.DefaultPool value. Tune these when calserver shares a
		// Postgres with a live sabre instance and load could otherwise open
		// too many connections.
		MaxOpenConns int
		MaxIdleConns int
		// ConnMaxLifetime is a Go duration string ("5m", "30s"); empty means
		// the default. Retires connections so the pool recovers across
		// Postgres failovers and poolers.
		ConnMaxLifetime string
	}
	// Logging holds log-verbosity and request-debugging options.
	Logging struct {
		// Level is the log verbosity: debug, info, warn, or error. Empty
		// means the built-in default (info).
		Level      string
		RequestDir string
		// AccessLog emits one slog line per request into the main log (so it
		// shares its rotation). Cheap enough to leave on; off by default.
		AccessLog bool
	}
	Scheduling struct {
		IMip struct {
			SMTP            string
			Sender          string
			SendAsOrganizer bool
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
	// Retention holds account soft-delete settings.
	Retention struct {
		// DeletedDays is how long a soft-deleted account is kept before the
		// purge job removes it. 0 means the built-in default (see
		// RetentionDays). Only consulted when scheduling a deletion.
		DeletedDays int
	}
	// Push holds WebDAV-Push settings (PUSH-PLAN.md). Off by default; when
	// Enabled, the VAPID keys and TopicSecret must be set or the server fails
	// closed at startup.
	Push struct {
		// Enabled turns on WebDAV-Push advertisement, registration, and
		// notification delivery.
		Enabled bool
		// VAPIDSubject is the RFC 8292 'sub' claim (a mailto: or https: URL
		// identifying this server to the push service).
		VAPIDSubject string
		// VAPIDPublicKey / VAPIDPrivateKey are the base64url-encoded P-256
		// VAPID keypair (generate once; the private key is a secret).
		VAPIDPublicKey  string
		VAPIDPrivateKey string
		// TopicSecret is the HMAC key deriving the opaque per-collection push
		// topic. A secret; changing it re-topics every collection (clients
		// re-register on their next discovery).
		TopicSecret string
	}
}

// DefaultRetentionDays is the fallback grace period for soft-deleted accounts
// when [retention].deleted_days is unset or non-positive.
const DefaultRetentionDays = 30

// RetentionDays applies the DefaultRetentionDays fallback so calserver-admin
// and calctl default identically.
func RetentionDays(days int) int {
	if days <= 0 {
		return DefaultRetentionDays
	}
	return days
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
	// A present key of the wrong type must fail like an unknown key does —
	// silently reading `access_log = "True"` (a string) as false hides the
	// intended setting behind a healthy-looking startup.
	var typeErrs []string
	badType := func(key, want string, got any) {
		typeErrs = append(typeErrs, fmt.Sprintf("config key %q must be %s, not %s", key, want, describeValue(got)))
	}
	str := func(key string) string {
		v, present := flat[key]
		if !present {
			return ""
		}
		s, ok := v.(string)
		if !ok {
			badType(key, "a quoted string", v)
		}
		return s
	}
	boolean := func(key string) bool {
		v, present := flat[key]
		if !present {
			return false
		}
		b, ok := v.(bool)
		if !ok {
			badType(key, "true or false (unquoted)", v)
		}
		return b
	}
	integer := func(key string) int {
		v, present := flat[key]
		if !present {
			return 0
		}
		i, ok := v.(int)
		if !ok {
			badType(key, "an integer (unquoted)", v)
		}
		return i
	}
	list := func(key string) []string {
		v, present := flat[key]
		if !present {
			return nil
		}
		l, ok := v.([]string)
		if !ok {
			badType(key, "an array of quoted strings", v)
		}
		return l
	}

	cfg.Server.Addr = str("server.addr")
	cfg.Server.MetricsAddr = str("server.metrics_addr")
	cfg.Server.BaseURI = str("server.base_uri")
	cfg.Server.Realm = str("server.realm")
	cfg.Server.TLS.Cert = str("server.tls.cert")
	cfg.Server.TLS.Key = str("server.tls.key")
	cfg.Server.Rcptd.URL = str("server.rcptd.url")
	cfg.Server.Rcptd.Token = str("server.rcptd.token")
	cfg.Server.TempFileFilter = boolean("server.temp_file_filter")
	cfg.Server.TempFileDir = str("server.temp_file_dir")
	cfg.Database.DSN = str("database.dsn")
	cfg.Database.MaxOpenConns = integer("database.max_open_conns")
	cfg.Database.MaxIdleConns = integer("database.max_idle_conns")
	cfg.Database.ConnMaxLifetime = str("database.conn_max_lifetime")
	cfg.Logging.Level = str("logging.level")
	cfg.Logging.RequestDir = str("logging.request_dir")
	cfg.Logging.AccessLog = boolean("logging.access_log")
	cfg.Scheduling.IMip.SMTP = str("scheduling.imip.smtp")
	cfg.Scheduling.IMip.Sender = str("scheduling.imip.sender")
	cfg.Scheduling.IMip.SendAsOrganizer = boolean("scheduling.imip.send_as_organizer")
	cfg.CalendarServer.Browser = boolean("calendarserver.browser")
	cfg.CalendarServer.AutoProvision = boolean("calendarserver.auto_provision")
	cfg.CalendarServer.PerUserProps = boolean("calendarserver.per_user_props")
	cfg.CalendarServer.ExpandGroups = boolean("calendarserver.expand_groups")
	cfg.CalendarServer.PrincipalsReadable = boolean("calendarserver.principals_readable")
	cfg.CalendarServer.AutoSchedulePrefixes = list("calendarserver.auto_schedule_prefixes")
	cfg.Retention.DeletedDays = integer("retention.deleted_days")
	cfg.Push.Enabled = boolean("push.enabled")
	cfg.Push.VAPIDSubject = str("push.vapid_subject")
	cfg.Push.VAPIDPublicKey = str("push.vapid_public_key")
	cfg.Push.VAPIDPrivateKey = str("push.vapid_private_key")
	cfg.Push.TopicSecret = str("push.topic_secret")

	for key := range flat {
		if !known[key] {
			return nil, fmt.Errorf("%s: unknown config key %q", path, key)
		}
	}
	if len(typeErrs) > 0 {
		return nil, fmt.Errorf("%s: %s", path, strings.Join(typeErrs, "; "))
	}
	return cfg, nil
}

// describeValue names a parsed value's type and content for type-mismatch
// errors, e.g. `the string "True"`.
func describeValue(v any) string {
	switch t := v.(type) {
	case string:
		return fmt.Sprintf("the string %q", t)
	case bool:
		return fmt.Sprintf("the boolean %v", t)
	case int:
		return fmt.Sprintf("the integer %d", t)
	case []string:
		return "an array"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// known is the set of recognised "section.key" paths; anything else is a typo
// worth reporting rather than silently ignoring.
var known = map[string]bool{
	"server.addr": true, "server.metrics_addr": true,
	"server.base_uri": true, "server.realm": true,
	"server.tls.cert": true, "server.tls.key": true,
	"server.rcptd.url": true, "server.rcptd.token": true,
	"server.temp_file_filter": true, "server.temp_file_dir": true,
	"database.dsn":               true,
	"database.max_open_conns":    true,
	"database.max_idle_conns":    true,
	"database.conn_max_lifetime": true,
	"logging.level":              true,
	"logging.request_dir":        true,
	"logging.access_log":         true,
	"scheduling.imip.smtp":       true, "scheduling.imip.sender": true,
	"scheduling.imip.send_as_organizer": true,
	"calendarserver.browser":            true, "calendarserver.auto_provision": true,
	"calendarserver.per_user_props": true, "calendarserver.expand_groups": true,
	"calendarserver.principals_readable":    true,
	"calendarserver.auto_schedule_prefixes": true,
	"retention.deleted_days":                true,
	"push.enabled":                          true,
	"push.vapid_subject":                    true,
	"push.vapid_public_key":                 true,
	"push.vapid_private_key":                true,
	"push.topic_secret":                     true,
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
