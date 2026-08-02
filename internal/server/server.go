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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/carddav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	"calserver/internal/metrics"
	"calserver/internal/push"
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
	// TempFileFilter intercepts OS junk files (._*, .DS_Store, Thumbs.db,
	// editor swap files) into a scratch directory instead of the backend,
	// like sabre's TemporaryFileFilterPlugin. Off by default (sabre core has
	// no such plugin); TempFileDir names the scratch directory when on.
	TempFileFilter bool
	TempFileDir    string
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
	// RcptdURL, when non-empty, switches authentication to an rcptd daemon's
	// /auth endpoint (Dovecot password hashes verified locally) instead of the
	// users.digesta1 table. Digest auth is disabled in this mode — rcptd
	// serves no digesta1 — so it's Basic-only; front it with TLS. Pair with
	// AutoProvision so first-time logins get a principal and default
	// collections. RcptdToken is the bearer token rcptd requires (a secret,
	// kept out of Canonical()).
	RcptdURL   string
	RcptdToken string
	// RequestLogDir, when non-empty, dumps every request/response (method,
	// path, headers, bounded bodies; Authorization redacted) to one file per
	// request under this directory. A debugging aid for DAV clients — off by
	// default; do not enable in production (bodies hold calendar/contact data).
	RequestLogDir string
	// AccessLog, when true, emits one structured slog line per request
	// (method, path, status, bytes, duration) through the process logger, so
	// it shares the main log's rotation. Unlike RequestLogDir it logs no
	// bodies, so it is safe to leave on. Off by default.
	AccessLog bool
	// Metrics, when non-nil, receives Prometheus observations from the HTTP
	// middleware, auth backends, storage, and iMip (METRICS-PLAN.md). Not
	// part of Canonical(): it changes no DAV semantics.
	Metrics *metrics.Set
	// RetentionDays is the default grace period, in days, for soft-deleted
	// accounts (SOFT-DELETE-PLAN.md). Read only when scheduling a deletion;
	// the auth gate and purge job work off the stored purge_after. Resolved
	// with config.RetentionDays so it is never zero here. Not part of
	// Canonical(): it changes no DAV semantics.
	RetentionDays int
	// Push holds WebDAV-Push settings (PUSH-PLAN.md). When Enabled, compose()
	// registers the push plugin and asserts the VAPID keys / topic secret are
	// present (fail closed). The worker that delivers notifications is started
	// separately via NewWithBackground.
	Push PushConfig
}

// PushConfig carries the WebDAV-Push VAPID keypair and topic secret. Mirrors
// config.Config.Push; kept in this package so callers depend only on server.
type PushConfig struct {
	Enabled         bool
	VAPIDSubject    string
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	TopicSecret     string
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
	return fmt.Sprintf("base-uri=%s realm=%s imip=%t auto-provision=%t per-user-props=%t expand-groups=%t principals-readable=%t auto-schedule=%s browser=%t tempfilter=%t rcptd=%t reqlog=%t accesslog=%t push=%t",
		base, realm, c.IMipSMTP != "", c.AutoProvision, c.PerUserProps,
		c.ExpandGroups, c.PrincipalsReadable,
		strings.Join(c.AutoSchedulePrefixes, ","), c.Browser, c.TempFileFilter, c.RcptdURL != "", c.RequestLogDir != "", c.AccessLog, c.Push.Enabled)
}

// pushConfig projects the server config onto the push package's config,
// filling in the mount prefix the plugin needs for absolute Location URLs.
func (c Config) pushConfig() push.Config {
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
	return push.Config{
		VAPIDSubject:    c.Push.VAPIDSubject,
		VAPIDPublicKey:  c.Push.VAPIDPublicKey,
		VAPIDPrivateKey: c.Push.VAPIDPrivateKey,
		TopicSecret:     c.Push.TopicSecret,
		BaseURI:         base,
	}
}

// New builds the storage-backed server, wrapped with the /.well-known/caldav
// redirect real clients use for discovery (RFC 6764).
func New(db *sql.DB, cfg Config) http.Handler {
	h, _ := NewWithBackground(db, cfg, "")
	return h
}

// NewWithBackground is New plus any long-running background service the config
// enables. When [push] is on and dsn is non-empty, worker is non-nil and the
// caller MUST run worker.Run(ctx) to deliver notifications (advertisement and
// registration work without it; only delivery needs the worker). dsn is the
// Postgres DSN the worker opens its dedicated LISTEN connection with.
func NewWithBackground(db *sql.DB, cfg Config, dsn string) (http.Handler, *push.Worker) {
	srv := newDAVServer(db, cfg)
	var worker *push.Worker
	if cfg.Push.Enabled && dsn != "" {
		store := &push.Store{DB: db}
		worker = push.NewWorker(cfg.pushConfig(), store,
			push.NewWebPushSender(cfg.pushConfig()), srv, dsn, slog.Default())
	}
	var h http.Handler = WrapWellKnown(srv, srv.BaseURI)
	// Request logging wraps outside the well-known redirect so it captures the
	// final status of every request, including redirects and auth challenges.
	h = WrapRequestLog(h, cfg.RequestLogDir)
	// Access log wraps outside the per-request file dump so its status and byte
	// count reflect the final response, including well-known redirects.
	h = WrapAccessLog(h, cfg.AccessLog)
	// Metrics wrap outermost too, so the well-known row counts redirects;
	// order relative to the request log (a debug aid) is immaterial.
	return WrapMetrics(h, cfg.Metrics, srv.BaseURI), worker
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
	calendars := &storage.CalDAV{DB: db, Metrics: cfg.Metrics}
	users := &storage.Users{DB: db}

	addressbooks := &storage.CardDAV{DB: db, Metrics: cfg.Metrics}

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
	srv.Metrics = cfg.Metrics
	// tombstoned gates soft-deleted accounts out of every real auth path. It
	// fails open (allow) on a DB error via the discarded error — a DB blip must
	// not lock everyone out; the purge job is the backstop (SOFT-DELETE-PLAN
	// §5, §12).
	tombstoned := func(principal string) bool { ok, _ := users.IsDeleted(principal); return ok }
	switch {
	case authOverride != nil:
		// The injected-plugin/test path (NewLoggedIn) is left ungated on
		// purpose, matching how BeforeMethod skips fixedPrincipal.
		srv.AddPlugin(authOverride)
	case cfg.RcptdURL != "":
		// Authenticate against rcptd's /auth (Dovecot hashes verified locally
		// via shared/passwd). Basic-only: rcptd serves no digesta1, so Digest
		// can't work. First-time logins get a principal from the provisioner.
		prov := &provisioner{
			principals: principals,
			writes:     &storage.AdminWrites{DB: db},
			realm:      cfg.Realm,
			seen:       map[string]bool{},
		}
		rb := auth.NewRcptdBackend(cfg.Realm, cfg.RcptdURL, cfg.RcptdToken, 5*time.Minute)
		rb.Provision = prov.ensure
		rb.Logger = slog.Default()
		rb.Metrics = cfg.Metrics
		ap := auth.NewPlugin(rb)
		ap.Metrics = cfg.Metrics
		ap.Tombstoned = tombstoned
		srv.AddPlugin(ap)
	default:
		ap := auth.NewPlugin(
			&auth.DigestBackend{Realm: cfg.Realm, HashA1: users.HashA1, Metrics: cfg.Metrics},
			&auth.BasicBackend{Realm: cfg.Realm, Metrics: cfg.Metrics, Validate: func(username, password string) bool {
				// Basic verifies against the same digesta1 hash, compared in
				// constant time so verification does not leak the stored hash.
				hash, ok := users.HashA1(cfg.Realm, username)
				return ok && subtle.ConstantTimeCompare(
					[]byte(auth.DigestA1(username, cfg.Realm, password)), []byte(hash)) == 1
			}},
		)
		ap.Metrics = cfg.Metrics
		ap.Tombstoned = tombstoned
		srv.AddPlugin(ap)
	}
	if cfg.TempFileFilter {
		// After auth (temp requests stay authenticated), before ACL and the
		// core handlers so junk PUTs never reach calendar-data validation.
		dir := cfg.TempFileDir
		if dir == "" {
			dir = filepath.Join(os.TempDir(), "calserver-tempfiles")
		}
		srv.AddPlugin(&dav.TempFileFilterPlugin{DataDir: dir})
	}
	srv.AddPlugin(&acl.Plugin{
		AdminPrincipals:                   []string{"principals/admin"},
		PrincipalsReadableByAuthenticated: cfg.PrincipalsReadable,
	})
	srv.AddPlugin(&caldav.Plugin{})
	// WebDAV-Push (PUSH-PLAN.md). Registered here on purpose: after the ACL
	// plugin (so its PropFind values honor the read-deny latch) and after
	// caldav.Plugin, but before SharingPlugin (so a collection POST reaches it
	// first and a non-push body is passed on intact). Enabled-but-unconfigured
	// fails closed at startup, mirroring the ACL assertion below.
	if cfg.Push.Enabled {
		if !cfg.pushConfig().Valid() {
			panic("server: [push] enabled but vapid_public_key, vapid_private_key or topic_secret is empty")
		}
		srv.AddPlugin(push.NewPlugin(cfg.pushConfig(), &push.Store{DB: db}))
	}
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
			Metrics:     cfg.Metrics,
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
	// The free-busy, Add-Member, outbox schedule-send, and sharing paths
	// enforce ACL themselves (they sit outside the ACL plugin's BeforeMethod
	// table) by resolving a PrivilegeChecker and now denying when it is
	// absent. Assert one is wired here so a future edit that drops or
	// reorders the acl plugin out of the stack fails loudly at startup
	// rather than silently changing those paths' authorization.
	if _, err := caldav.RequirePrivilegeChecker(srv); err != nil {
		panic(fmt.Sprintf("server: no ACL privilege checker registered: %v", err))
	}
	return srv
}
