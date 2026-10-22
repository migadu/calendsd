package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"calendsd/internal/server"
	"calendsd/internal/storage"
)

// DefaultConfigPaths are the locations searched, in order, for a config file
// when -config is not given explicitly. Every binary that reads calendsd's
// config (calendsd, calctl) discovers it through FindDefaultConfig.
var DefaultConfigPaths = []string{
	"/etc/calserver/config.toml",
	"/usr/local/etc/calserver/config.toml",
	"config.toml",
}

// FindDefaultConfig returns the first existing DefaultConfigPaths entry, or ""
// when none is present (so an unconfigured in-memory server still starts).
func FindDefaultConfig() string {
	for _, p := range DefaultConfigPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// optDur parses an optional duration config value: empty yields 0 (the caller's
// default), a malformed value is a fatal config error naming the key.
func optDur(key, val string) (time.Duration, error) {
	if val == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(val)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

// Merged is the result of resolving command-line flags over the optional
// -config file. File and Set stay exposed so each binary can merge its own
// extra flags (addr, TLS, admin auth) under the same precedence rules.
type Merged struct {
	File Config
	// ConfigPath is the config file that was loaded, whether given via -config
	// or discovered among DefaultConfigPaths; empty when none was found.
	ConfigPath string
	// Set records which flags were given explicitly on the command line.
	Set map[string]bool
	// DSN is the resolved database DSN.
	DSN string
	// DBPool is the resolved connection-pool bound; zero fields fall back to
	// storage.DefaultPool. Pass it to storage.OpenPool.
	DBPool storage.Pool
	// LogLevel is the resolved log verbosity (debug/info/warn/error), or
	// empty for the default. Pass it to logging.Setup, which also honours
	// $CALSERVER_LOG_LEVEL when this is empty.
	LogLevel string
	// Server is the resolved effective server configuration — the surface
	// server.Config.Canonical() echoes.
	Server server.Config
}

// Str resolves one string option: an explicit flag wins, then a non-empty
// file value, then the flag's default.
func (m *Merged) Str(name, flagVal, fileVal string) string {
	if m.Set[name] || fileVal == "" {
		return flagVal
	}
	return fileVal
}

// Bool resolves one boolean option: an explicit flag wins, else the file.
func (m *Merged) Bool(name string, flagVal, fileVal bool) bool {
	if m.Set[name] {
		return flagVal
	}
	return fileVal
}

// SharedServerFlags registers the flags that define the DAV server's
// effective configuration on the default FlagSet. calendsd and
// `calctl panel` both resolve through this one function, so the two
// processes can never drift in how they turn identical inputs into a
// server.Config — the property the Canonical() config echo depends on
// (ADMIN-PLAN.md §2). The returned resolver must run after flag.Parse.
func SharedServerFlags(dbUsage string) func() (*Merged, error) {
	configPath := flag.String("config", "", "path to a TOML config file (see config.toml.example); command-line flags override its values. When empty, the first existing of /etc/calserver/config.toml, /usr/local/etc/calserver/config.toml, ./config.toml is used")
	dsn := flag.String("db", "", dbUsage)
	baseURI := flag.String("base-uri", "/", "URI prefix the server is mounted at")
	realm := flag.String("realm", server.DefaultRealm, "auth realm; must match the realm baked into users.digesta1")
	imipSMTP := flag.String("imip-smtp", "", "SMTP host:port for emailing invites to external attendees (empty disables iMip)")
	imipSender := flag.String("imip-sender", "", "From address for iMip invitation emails")
	imipSendAsOrganizer := flag.Bool("imip-send-as-organizer", false, "put the scheduling sender's own address (organizer for invitations, attendee for replies) in the From header of iMip emails (imip-sender stays the envelope sender); only safe when the relay can DKIM-sign for those local domains")
	imipSubmission := flag.String("imip-submission", "", "SMTP submission host:port to send invitations AS the logged-in user (authenticated with their own credentials); empty uses the shared relay (imip-smtp)")
	imipSubmissionImplicitTLS := flag.Bool("imip-submission-implicit-tls", false, "submission uses implicit TLS (SMTPS, :465); false uses STARTTLS (:587)")
	browser := flag.Bool("browser", false, "serve HTML indexes for GET on collections (like sabre's Browser plugin; default matches sabre without it: 501)")
	tempFileFilter := flag.Bool("temp-file-filter", false, "intercept OS junk files (._*, .DS_Store, Thumbs.db, editor swap files) into a scratch directory instead of the backend, like sabre's TemporaryFileFilterPlugin")
	tempFileDir := flag.String("temp-file-dir", "", "scratch directory for -temp-file-filter (default: a calserver-tempfiles subdir of the system temp dir)")
	autoSchedule := flag.String("auto-schedule-prefixes", "", "comma-separated principal-name prefixes (e.g. resource,location) whose scheduling messages are auto-accepted Calendar-Server style; empty disables")
	principalsReadable := flag.Bool("principals-readable", false, "grant authenticated users read access to principal resources (Calendar-Server behavior)")
	autoProvision := flag.Bool("auto-provision", false, "recreate the default calendar/addressbook when a principal has none (Calendar-Server behavior)")
	perUserProps := flag.Bool("per-user-props", false, "store non-owner property writes per user and overlay them on reads (Calendar-Server behavior)")
	expandGroups := flag.Bool("expand-groups", false, "expand group ATTENDEEs into per-member invitations (Calendar-Server behavior)")
	rcptdURL := flag.String("rcptd-url", "", "base URL of an rcptd daemon (e.g. http://127.0.0.1:8090); when set, authenticate Basic logins against its /auth endpoint (Dovecot passwd hashes) instead of the users table. Basic-only (no digesta1) — front with TLS; pair with -auto-provision")
	rcptdToken := flag.String("rcptd-token", "", "bearer token for rcptd /auth (its auth_token); falls back to $RCPTD_AUTH_TOKEN")
	requestLogDir := flag.String("request-log-dir", "", "debug: dump every request/response (headers + bounded bodies, Authorization redacted) to one file per request under this directory; empty disables. Do not use in production")
	accessLog := flag.Bool("access-log", false, "log one line per request (method, path, status, bytes, duration) into the main log; overrides [logging].access_log")
	webURL := flag.String("web-url", "", "calclient origin that emails link to (e.g. https://calendar.example.org); empty disables share-invite emails (docs/share-invite-email.md)")
	logLevel := flag.String("log-level", "", "log verbosity: debug, info, warn, or error (default info). Overrides [logging].level; when neither is set, $CALSERVER_LOG_LEVEL applies")

	return func() (*Merged, error) {
		m := &Merged{Set: map[string]bool{}}
		// An explicit -config wins; otherwise fall back to the well-known
		// locations so `calendsd`/`calctl panel` pick up an installed
		// config without repeating -config on every invocation.
		path := *configPath
		if path == "" {
			path = FindDefaultConfig()
		}
		if path != "" {
			c, err := Load(path)
			if err != nil {
				return nil, err
			}
			m.File = *c
			m.ConfigPath = path
		}
		flag.Visit(func(f *flag.Flag) { m.Set[f.Name] = true })

		autoPrefixes := m.File.CalendarServer.AutoSchedulePrefixes
		if m.Set["auto-schedule-prefixes"] || (autoPrefixes == nil && *autoSchedule != "") {
			autoPrefixes = nil
			if *autoSchedule != "" {
				autoPrefixes = strings.Split(*autoSchedule, ",")
			}
		}

		rcptdTok := m.Str("rcptd-token", *rcptdToken, m.File.Server.Rcptd.Token)
		if rcptdTok == "" {
			rcptdTok = os.Getenv("RCPTD_AUTH_TOKEN")
		}

		m.DSN = m.Str("db", *dsn, m.File.Database.DSN)
		m.DBPool = storage.Pool{
			MaxOpen: m.File.Database.MaxOpenConns,
			MaxIdle: m.File.Database.MaxIdleConns,
		}
		if life := m.File.Database.ConnMaxLifetime; life != "" {
			d, err := time.ParseDuration(life)
			if err != nil {
				return nil, fmt.Errorf("database.conn_max_lifetime: %w", err)
			}
			m.DBPool.ConnMaxLifetime = d
		}
		m.LogLevel = m.Str("log-level", *logLevel, m.File.Logging.Level)

		// iMIP durable-queue credential key: the file value, overridden by
		// $CALSERVER_IMIP_CREDENTIAL_KEY (a secret, so no flag — it must not reach
		// the process arg list). Empty leaves the per-user durable path off; a
		// present-but-malformed key fails closed at startup rather than silently
		// disabling metered durability.
		imipKeyB64 := m.File.Scheduling.IMip.CredentialKey
		if env := os.Getenv("CALSERVER_IMIP_CREDENTIAL_KEY"); env != "" {
			imipKeyB64 = env
		}
		imipKey, err := DecodeIMipCredentialKey(imipKeyB64)
		if err != nil {
			return nil, err
		}
		imipReclaim, err := optDur("scheduling.imip.queue.reclaim_after", m.File.Scheduling.IMip.Queue.ReclaimAfter)
		if err != nil {
			return nil, err
		}
		imipCredTTL, err := optDur("scheduling.imip.queue.credential_ttl", m.File.Scheduling.IMip.Queue.CredentialTTL)
		if err != nil {
			return nil, err
		}
		imipRetention, err := optDur("scheduling.imip.queue.retention", m.File.Scheduling.IMip.Queue.Retention)
		if err != nil {
			return nil, err
		}

		m.Server = server.Config{
			BaseURI:                   m.Str("base-uri", *baseURI, m.File.Server.BaseURI),
			Realm:                     m.Str("realm", *realm, m.File.Server.Realm),
			IMipSMTP:                  m.Str("imip-smtp", *imipSMTP, m.File.Scheduling.IMip.SMTP),
			IMipSender:                m.Str("imip-sender", *imipSender, m.File.Scheduling.IMip.Sender),
			IMipSendAsOrganizer:       m.Bool("imip-send-as-organizer", *imipSendAsOrganizer, m.File.Scheduling.IMip.SendAsOrganizer),
			IMipSubmission:            m.Str("imip-submission", *imipSubmission, m.File.Scheduling.IMip.Submission),
			IMipSubmissionImplicitTLS: m.Bool("imip-submission-implicit-tls", *imipSubmissionImplicitTLS, m.File.Scheduling.IMip.SubmissionImplicitTLS),
			Browser:                   m.Bool("browser", *browser, m.File.CalendarServer.Browser),
			TempFileFilter:            m.Bool("temp-file-filter", *tempFileFilter, m.File.Server.TempFileFilter),
			TempFileDir:               m.Str("temp-file-dir", *tempFileDir, m.File.Server.TempFileDir),
			AutoSchedulePrefixes:      autoPrefixes,
			PrincipalsReadable:        m.Bool("principals-readable", *principalsReadable, m.File.CalendarServer.PrincipalsReadable),
			AutoProvision:             m.Bool("auto-provision", *autoProvision, m.File.CalendarServer.AutoProvision),
			PerUserProps:              m.Bool("per-user-props", *perUserProps, m.File.CalendarServer.PerUserProps),
			ExpandGroups:              m.Bool("expand-groups", *expandGroups, m.File.CalendarServer.ExpandGroups),
			RcptdURL:                  m.Str("rcptd-url", *rcptdURL, m.File.Server.Rcptd.URL),
			RcptdToken:                rcptdTok,
			RequestLogDir:             m.Str("request-log-dir", *requestLogDir, m.File.Logging.RequestDir),
			AccessLog:                 m.Bool("access-log", *accessLog, m.File.Logging.AccessLog),
			WebURL:                    m.Str("web-url", *webURL, m.File.Server.WebURL),
			RetentionDays:             RetentionDays(m.File.Retention.DeletedDays),
			Push: server.PushConfig{
				Enabled:         m.File.Push.Enabled,
				VAPIDSubject:    m.File.Push.VAPIDSubject,
				VAPIDPublicKey:  m.File.Push.VAPIDPublicKey,
				VAPIDPrivateKey: m.File.Push.VAPIDPrivateKey,
				TopicSecret:     m.File.Push.TopicSecret,
			},
			IMipQueue: server.IMipQueueConfig{
				CredentialKey: imipKey,
				ReclaimAfter:  imipReclaim,
				CredentialTTL: imipCredTTL,
				Retention:     imipRetention,
				Workers:       m.File.Scheduling.IMip.Queue.Workers,
			},
		}
		if err := m.Server.Validate(); err != nil {
			return nil, err
		}
		return m, nil
	}
}
