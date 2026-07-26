// Package server composes the full calserver DAV server from a shared sabre
// Postgres database: tree (principals + calendars), auth, ACL, and CalDAV
// plugins. Mirrors scripts/sabre-server.php so both servers expose the same
// feature set during parity testing.
package server

import (
	"crypto/subtle"
	"database/sql"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/carddav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	"calserver/internal/storage"
)

// Config carries server composition options.
type Config struct {
	BaseURI string
	// Realm must match the realm baked into users.digesta1 (PLAN.md §5).
	Realm string
	// IMipSender and IMipSMTP enable emailing invites to external
	// attendees; when IMipSMTP is empty, iMip is disabled.
	IMipSender string
	IMipSMTP   string
	// Browser serves HTML indexes for GET on collections (off by default:
	// sabre without its Browser plugin answers 501, which the corpus pins).
	Browser bool
	// AutoProvision recreates the default calendar/addressbook when a
	// principal has none left (Calendar-Server behavior); off by default.
	AutoProvision bool
	// PerUserProps stores non-owner property writes per (principal, path)
	// and overlays them on reads (Calendar-Server behavior); off by default.
	PerUserProps bool
	// ExpandGroups expands group ATTENDEEs into per-member invitations
	// (Calendar-Server behavior); off by default.
	ExpandGroups bool
	// PrincipalsReadable grants authenticated users read on principal
	// resources (Calendar-Server behavior); off by default (sabre parity).
	PrincipalsReadable bool
	// AutoSchedulePrefixes lists principal-name prefixes (e.g.
	// "resource,location" split on commas by the caller) whose scheduling
	// messages are auto-processed Calendar-Server style: accept into the
	// default calendar (decline on conflict), REPLY to the organizer, no
	// inbox drop. Empty disables auto-scheduling (sabre behavior).
	AutoSchedulePrefixes []string
}

// Canonical renders the effective configuration as one deterministic line.
// Both calserver and calserver-admin log it at startup, and the admin
// dashboard echoes its own copy, so config drift between the two processes
// (which would make the admin privilege simulator lie) reduces to comparing
// two strings (ADMIN-PLAN.md §2). Values are normalized the way the server
// applies them (base-URI slashes, realm default), so equivalent configs
// always render the same line. No secrets: DSN and SMTP host stay out.
func (c Config) Canonical() string {
	base := c.BaseURI
	if base == "" {
		base = "/"
	}
	if !strings.HasPrefix(base, "/") {
		base = "/" + base
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	realm := c.Realm
	if realm == "" {
		realm = "SabreDAV"
	}
	return fmt.Sprintf("base-uri=%s realm=%s imip=%t auto-provision=%t per-user-props=%t expand-groups=%t principals-readable=%t auto-schedule=%s browser=%t",
		base, realm, c.IMipSMTP != "", c.AutoProvision, c.PerUserProps,
		c.ExpandGroups, c.PrincipalsReadable,
		strings.Join(c.AutoSchedulePrefixes, ","), c.Browser)
}

// New builds the storage-backed server, wrapped with the /.well-known/caldav
// redirect real clients use for discovery (RFC 6764).
func New(db *sql.DB, cfg Config) http.Handler {
	srv := newDAVServer(db, cfg)
	return WrapWellKnown(srv, srv.BaseURI)
}

// WrapWellKnown adds the /.well-known/caldav → base-URI redirect.
func WrapWellKnown(h http.Handler, baseURI string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/.well-known/caldav", http.RedirectHandler(baseURI, http.StatusMovedPermanently))
	mux.Handle("/.well-known/carddav", http.RedirectHandler(baseURI, http.StatusMovedPermanently))
	mux.Handle("/", h)
	return mux
}

// NewLoggedIn builds the production plugin stack with authentication
// replaced by a fixed principal (the auth.NewLoggedInPlugin test pattern).
// The admin privilege simulator uses it to answer "what would user X see on
// path Y" through the identical composition (ADMIN-PLAN.md §3.5).
func NewLoggedIn(db *sql.DB, cfg Config, principal string) *dav.Server {
	return compose(db, cfg, auth.NewLoggedInPlugin(principal))
}

func newDAVServer(db *sql.DB, cfg Config) *dav.Server {
	return compose(db, cfg, nil)
}

// compose builds the full server; authOverride nil means real Digest/Basic
// authentication against the users table.
func compose(db *sql.DB, cfg Config, authOverride *auth.Plugin) *dav.Server {
	if cfg.Realm == "" {
		cfg.Realm = "SabreDAV"
	}

	principals := &storage.Principals{DB: db}
	calendars := &storage.CalDAV{DB: db}
	users := &storage.Users{DB: db}

	addressbooks := &storage.CardDAV{DB: db}

	root := mock.NewCollection("root")
	principalCollection := acl.NewPrincipalCollection(principals, "principals")
	principalCollection.Proxies = true // expose calendar-proxy delegation principals
	root.Attach(principalCollection)
	calendarRoot := caldav.NewCalendarRoot(principals, calendars, "calendars")
	calendarRoot.AutoProvision = cfg.AutoProvision
	calendarRoot.PerUserProps = cfg.PerUserProps
	root.Attach(calendarRoot)
	addressBookRoot := carddav.NewAddressBookRoot(principals, addressbooks, "addressbooks")
	addressBookRoot.AutoProvision = cfg.AutoProvision
	root.Attach(addressBookRoot)

	srv := dav.NewServer(cfg.BaseURI, root)
	if authOverride != nil {
		srv.AddPlugin(authOverride)
	} else {
		srv.AddPlugin(auth.NewPlugin(
			&auth.DigestBackend{Realm: cfg.Realm, HashA1: users.HashA1},
			&auth.BasicBackend{Realm: cfg.Realm, Validate: func(username, password string) bool {
				// Basic verifies against the same digesta1 hash, compared in
				// constant time so verification does not leak the stored hash.
				hash, ok := users.HashA1(cfg.Realm, username)
				return ok && subtle.ConstantTimeCompare(
					[]byte(auth.DigestA1(username, cfg.Realm, password)), []byte(hash)) == 1
			}},
		))
	}
	srv.AddPlugin(&acl.Plugin{
		AdminPrincipals:                   []string{"principals/admin"},
		PrincipalsReadableByAuthenticated: cfg.PrincipalsReadable,
	})
	srv.AddPlugin(&caldav.Plugin{})
	schedule := &caldav.SchedulePlugin{ExpandGroups: cfg.ExpandGroups}
	if len(cfg.AutoSchedulePrefixes) > 0 {
		prefixes := cfg.AutoSchedulePrefixes
		schedule.AutoSchedule = func(principalURI string) bool {
			name := strings.TrimPrefix(principalURI, "principals/")
			for _, p := range prefixes {
				if p != "" && strings.HasPrefix(name, p) {
					return true
				}
			}
			return false
		}
	}
	srv.AddPlugin(schedule)
	srv.AddPlugin(&caldav.SharingPlugin{})
	srv.AddPlugin(&caldav.NotificationsPlugin{})
	srv.AddPlugin(&carddav.Plugin{})
	if cfg.IMipSMTP != "" {
		srv.AddPlugin(&caldav.IMipPlugin{
			SenderEmail: cfg.IMipSender,
			Mailer:      &caldav.SMTPMailer{Addr: cfg.IMipSMTP},
		})
	}
	srv.AddPlugin(&dav.SyncPlugin{})
	srv.AddPlugin(&dav.PropertyStoragePlugin{Backend: &storage.PropertyStorage{DB: db}})
	if cfg.PerUserProps {
		// After property-storage: its late reads override global values.
		srv.AddPlugin(&dav.PerUserPropsPlugin{
			Backend: &storage.PropertyStorage{DB: db},
			// Calendar Server's per-user set: personal presentation props.
			Names: []xml.Name{
				{Space: "http://apple.com/ns/ical/", Local: "calendar-color"},
				{Space: "http://apple.com/ns/ical/", Local: "calendar-order"},
			},
			Exempt: []string{"principals/admin"},
		})
	}
	if cfg.Browser {
		// Last: only claims GETs no other plugin served.
		srv.AddPlugin(&dav.BrowserPlugin{})
	}
	return srv
}
