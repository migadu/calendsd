package config

import (
	"flag"
	"os"
	"strings"

	"calserver/internal/server"
)

// Merged is the result of resolving command-line flags over the optional
// -config file. File and Set stay exposed so each binary can merge its own
// extra flags (addr, TLS, admin auth) under the same precedence rules.
type Merged struct {
	File Config
	// Set records which flags were given explicitly on the command line.
	Set map[string]bool
	// DSN is the resolved database DSN.
	DSN string
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
// effective configuration on the default FlagSet. calserver and
// calserver-admin both resolve through this one function, so the two
// binaries can never drift in how they turn identical inputs into a
// server.Config — the property the Canonical() config echo depends on
// (ADMIN-PLAN.md §2). The returned resolver must run after flag.Parse.
func SharedServerFlags(dbUsage string) func() (*Merged, error) {
	configPath := flag.String("config", "", "path to a TOML config file (see config.toml.example); command-line flags override its values")
	dsn := flag.String("db", "", dbUsage)
	baseURI := flag.String("base-uri", "/", "URI prefix the server is mounted at")
	realm := flag.String("realm", "SabreDAV", "auth realm; must match the realm baked into users.digesta1")
	imipSMTP := flag.String("imip-smtp", "", "SMTP host:port for emailing invites to external attendees (empty disables iMip)")
	imipSender := flag.String("imip-sender", "", "From address for iMip invitation emails")
	browser := flag.Bool("browser", false, "serve HTML indexes for GET on collections (like sabre's Browser plugin; default matches sabre without it: 501)")
	autoSchedule := flag.String("auto-schedule-prefixes", "", "comma-separated principal-name prefixes (e.g. resource,location) whose scheduling messages are auto-accepted Calendar-Server style; empty disables")
	principalsReadable := flag.Bool("principals-readable", false, "grant authenticated users read access to principal resources (Calendar-Server behavior)")
	autoProvision := flag.Bool("auto-provision", false, "recreate the default calendar/addressbook when a principal has none (Calendar-Server behavior)")
	perUserProps := flag.Bool("per-user-props", false, "store non-owner property writes per user and overlay them on reads (Calendar-Server behavior)")
	expandGroups := flag.Bool("expand-groups", false, "expand group ATTENDEEs into per-member invitations (Calendar-Server behavior)")
	rcptdURL := flag.String("rcptd-url", "", "base URL of an rcptd daemon (e.g. http://127.0.0.1:8090); when set, authenticate Basic logins against its /auth endpoint (Dovecot passwd hashes) instead of the users table. Basic-only (no digesta1) — front with TLS; pair with -auto-provision")
	rcptdToken := flag.String("rcptd-token", "", "bearer token for rcptd /auth (its auth_token); falls back to $RCPTD_AUTH_TOKEN")

	return func() (*Merged, error) {
		m := &Merged{Set: map[string]bool{}}
		if *configPath != "" {
			c, err := Load(*configPath)
			if err != nil {
				return nil, err
			}
			m.File = *c
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
		m.Server = server.Config{
			BaseURI:              m.Str("base-uri", *baseURI, m.File.Server.BaseURI),
			Realm:                m.Str("realm", *realm, m.File.Server.Realm),
			IMipSMTP:             m.Str("imip-smtp", *imipSMTP, m.File.Scheduling.IMip.SMTP),
			IMipSender:           m.Str("imip-sender", *imipSender, m.File.Scheduling.IMip.Sender),
			Browser:              m.Bool("browser", *browser, m.File.CalendarServer.Browser),
			AutoSchedulePrefixes: autoPrefixes,
			PrincipalsReadable:   m.Bool("principals-readable", *principalsReadable, m.File.CalendarServer.PrincipalsReadable),
			AutoProvision:        m.Bool("auto-provision", *autoProvision, m.File.CalendarServer.AutoProvision),
			PerUserProps:         m.Bool("per-user-props", *perUserProps, m.File.CalendarServer.PerUserProps),
			ExpandGroups:         m.Bool("expand-groups", *expandGroups, m.File.CalendarServer.ExpandGroups),
			RcptdURL:             m.Str("rcptd-url", *rcptdURL, m.File.Server.Rcptd.URL),
			RcptdToken:           rcptdTok,
		}
		return m, nil
	}
}
