// Package server composes the full calendsd DAV server from a shared sabre
// Postgres database: tree (principals + calendars), auth, ACL, and CalDAV
// plugins. Mirrors scripts/sabre-server.php so both servers expose the same
// feature set during parity testing.
package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"calendsd/internal/acl"
	"calendsd/internal/auth"
	"calendsd/internal/caldav"
	"calendsd/internal/carddav"
	"calendsd/internal/contactsapi"
	"calendsd/internal/dav"
	"calendsd/internal/jobs"
	"calendsd/internal/metrics"
	"calendsd/internal/push"
	"calendsd/internal/storage"
)

// DefaultRealm is the auth realm when none is configured. It must never
// change: users.digesta1 bakes the realm into every stored hash, and Basic
// verifies against that same hash, so a different realm breaks every login.
const DefaultRealm = "SabreDAV"

// Config carries server composition options.
type Config struct {
	BaseURI string
	// Realm must match the realm baked into users.digesta1 (PLAN.md §5).
	Realm string
	// IMipSender and IMipSMTP enable emailing invites to external attendees.
	// iMip is enabled when either IMipSMTP (the shared relay) or IMipSubmission
	// (per-user submission) is set.
	IMipSender string
	IMipSMTP   string
	// IMipSendAsOrganizer puts the scheduling sender's own address (organizer
	// for invitations, attendee for replies) in the From header of scheduling
	// emails (IMipSender stays the SMTP envelope sender — a split sender). Only
	// safe when the relay can DKIM-sign for those local domain(s); off by
	// default. See caldav.IMipPlugin.SendAsOrganizer.
	IMipSendAsOrganizer bool
	// IMipSubmission, when set, is the SMTP submission host:port used to send
	// invitations AS the logged-in user (authenticated with their credentials),
	// so mail is DKIM/allowed_from-checked and metered against their sending
	// limits. Empty keeps the shared-relay behavior. IMipSMTP is still used for
	// resource-originated replies (which have no interactive login).
	IMipSubmission string
	// IMipSubmissionImplicitTLS selects implicit TLS (SMTPS, :465) for the
	// submission connection; false uses STARTTLS (:587).
	IMipSubmissionImplicitTLS bool
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
	// MaxAttendees caps the distinct ATTENDEEs one object may schedule;
	// zero means caldav.DefaultMaxAttendees.
	MaxAttendees int
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
	// PasswdFile, when non-empty, authenticates against a local Dovecot
	// passwd-file ("user:{SCHEME}hash:..." lines) with the same Basic-only
	// verification as RcptdURL. The file is re-read when it changes.
	// Mutually exclusive with RcptdURL.
	PasswdFile string
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
	// present (fail closed). The worker that delivers notifications is
	// returned by Build and started separately.
	Push PushConfig
	// WebURL is the calclient origin emails link to. Empty disables the
	// share-invite email (docs/share-invite-email.md); so does a recipient
	// domain without share_notify = 'email', and a missing relay (IMipSMTP).
	// Not part of Canonical(): it changes no DAV semantics.
	WebURL string
	// IMipQueue holds the durable iMIP job-queue settings (JOB-QUEUE-PLAN.md).
	// The queue is constructed by Build and started separately; its
	// CredentialKey gates the metered per-user durable retry.
	IMipQueue IMipQueueConfig
}

// IMipQueueConfig carries the durable iMIP job-queue settings, resolved to
// concrete types so callers depend only on server. Mirrors
// config.Config.Scheduling.IMip (credential key + [queue] tunables).
type IMipQueueConfig struct {
	// CredentialKey is the raw 32-byte AEAD key (decoded from the base64 config
	// value / $CALSERVER_IMIP_CREDENTIAL_KEY) that encrypts stored submission
	// passwords (D12). Nil means no key configured: the per-user durable path is
	// off and per-user sends fall back to a synchronous send. A secret — excluded from
	// Canonical(), which echoes only its presence.
	CredentialKey []byte
	// ReclaimAfter/CredentialTTL/Retention are the background-sweep windows; zero
	// means the internal/jobs default. ReclaimAfter must exceed the mailer
	// timeout. Workers is the claim-loop count (zero = default).
	ReclaimAfter  time.Duration
	CredentialTTL time.Duration
	Retention     time.Duration
	Workers       int
}

// PushConfig carries the WebDAV-Push VAPID keypair and topic secret. Mirrors
// config.Config.Push; kept in this package so callers depend only on server.
type PushConfig struct {
	Enabled         bool
	VAPIDSubject    string
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	TopicSecret     string
	// MaxSubscriptions caps live push subscriptions per principal; zero
	// means push.DefaultMaxSubscriptionsPerPrincipal.
	MaxSubscriptions int
}

// Canonical renders the effective configuration as one deterministic line.
// Both calendsd and the admin panel (`calctl panel`) log it at startup, and
// the dashboard echoes its own copy, so config drift between the two processes
// (which would make the admin privilege simulator lie) reduces to comparing
// two strings (ADMIN-PLAN.md §2). Values are normalized the way the server
// applies them (base-URI slashes, realm default), so equivalent configs
// always render the same line. No secrets: DSN and SMTP host stay out.
func (c Config) Canonical() string {
	base := dav.NormalizeBaseURI(c.BaseURI)
	realm := c.Realm
	if realm == "" {
		realm = DefaultRealm
	}
	return fmt.Sprintf("base-uri=%s realm=%s imip=%t imip-as-organizer=%t imip-submission=%t imip-durable=%t auto-provision=%t per-user-props=%t expand-groups=%t principals-readable=%t auto-schedule=%s browser=%t tempfilter=%t rcptd=%t passwdfile=%t reqlog=%t accesslog=%t push=%t",
		base, realm, c.IMipSMTP != "" || c.IMipSubmission != "", c.IMipSendAsOrganizer, c.IMipSubmission != "", len(c.IMipQueue.CredentialKey) > 0, c.AutoProvision, c.PerUserProps,
		c.ExpandGroups, c.PrincipalsReadable,
		strings.Join(c.AutoSchedulePrefixes, ","), c.Browser, c.TempFileFilter, c.RcptdURL != "", c.PasswdFile != "", c.RequestLogDir != "", c.AccessLog, c.Push.Enabled)
}

// Validate reports a configuration the server cannot run with. [push]
// enabled without its keys must fail closed at startup, as a config error
// rather than a crash.
func (c Config) Validate() error {
	if c.Push.Enabled && !c.pushConfig().Valid() {
		return errors.New("[push] enabled but vapid_public_key, vapid_private_key or topic_secret is empty")
	}
	if c.RcptdURL != "" && c.PasswdFile != "" {
		return errors.New("[server] rcptd.url and passwd_file are mutually exclusive")
	}
	if c.PasswdFile != "" {
		f, err := os.Open(c.PasswdFile)
		if err != nil {
			return fmt.Errorf("[server] passwd_file: %w", err)
		}
		f.Close()
	}
	return nil
}

// pushConfig projects the server config onto the push package's config,
// filling in the mount prefix the plugin needs for absolute Location URLs.
func (c Config) pushConfig() push.Config {
	base := dav.NormalizeBaseURI(c.BaseURI)
	return push.Config{
		VAPIDSubject:    c.Push.VAPIDSubject,
		VAPIDPublicKey:  c.Push.VAPIDPublicKey,
		VAPIDPrivateKey: c.Push.VAPIDPrivateKey,
		TopicSecret:     c.Push.TopicSecret,
		BaseURI:         base,

		MaxSubscriptionsPerPrincipal: c.Push.MaxSubscriptions,
	}
}

// pushStore builds the subscription store with the configured per-principal
// cap.
func (c Config) pushStore(db *sql.DB) *push.Store {
	return &push.Store{DB: db, MaxPerPrincipal: c.Push.MaxSubscriptions}
}

// Built is everything Build assembles: the DAV handler to serve, the
// background services to run, and the two pieces the webmail contacts API
// (internal/contactsapi) must share with the DAV server rather than
// re-create — the credential backend, whose rcptd cache and first-login
// provisioning are per instance, and the soft-delete gate.
type Built struct {
	Handler http.Handler
	// Worker is non-nil when [push] is on and a dsn was given; the caller
	// MUST run Worker.Run(ctx) to deliver notifications.
	Worker *push.Worker
	// Queue is non-nil when iMIP is configured and a dsn was given; the
	// caller MUST run Queue.Start(ctx) to process durable scheduling sends
	// (per-user retries and relay-native sends), or those jobs never run and
	// their objects stay pending. Without a dsn, external iMIP sends are
	// synchronous.
	Queue *jobs.Queue
	// Auth is the backend DAV authenticates with: the rcptd backend when
	// [server.rcptd] is configured, else the users-table Basic backend.
	Auth auth.Backend
	// Tombstoned mirrors the DAV auth gate's soft-delete check, bounded by
	// the caller's ctx.
	Tombstoned func(ctx context.Context, principal string) bool
	// accessLog carries [logging].access_log to the contacts API listener,
	// which has its own handler chain and would otherwise log nothing.
	accessLog bool
}

// ContactsAPI builds the webmail contacts API over the same database and the
// same credential backend as the DAV server this Built holds. Serve it on its
// own loopback listener ([server] api_addr); never mount it on the DAV one.
func (b *Built) ContactsAPI(db *sql.DB) http.Handler {
	return WrapAccessLogPathOnly(WrapRecoverJSON(contactsapi.New(db, b.Auth, b.Tombstoned, contactsapi.Options{
		Logger: slog.Default(),
	})), b.accessLog)
}

// Build composes the storage-backed server, wrapped with the
// /.well-known/caldav redirect real clients use for discovery (RFC 6764),
// plus the background services the config enables; see Built. dsn is the
// Postgres DSN those services open; "" leaves them nil.
func Build(db *sql.DB, cfg Config, dsn string) *Built {
	srv, backend := compose(db, cfg, nil)
	users := &storage.Users{DB: db}
	var worker *push.Worker
	if cfg.Push.Enabled && dsn != "" {
		store := cfg.pushStore(db)
		worker = push.NewWorker(cfg.pushConfig(), store,
			push.NewWebPushSender(cfg.pushConfig()), srv, dsn, slog.Default())
	}
	var queue *jobs.Queue
	if dsn != "" {
		queue = buildIMipQueue(db, cfg, dsn, srv)
	}
	// Recovery sits innermost, so every wrapper below records a panic as the
	// 500 it is answered with.
	var h http.Handler = WrapWellKnown(WrapRecover(srv), srv.BaseURI)
	// Request logging wraps outside the well-known redirect so it captures the
	// final status of every request, including redirects and auth challenges.
	h = WrapRequestLog(h, cfg.RequestLogDir)
	// Access log wraps outside the per-request file dump so its status and byte
	// count reflect the final response, including well-known redirects.
	h = WrapAccessLog(h, cfg.AccessLog)
	// Health sits outside both logs, so a monitor polling every few seconds
	// does not flood them, but inside metrics, so probes are still counted.
	h = WrapHealth(h, db.PingContext)
	h = WrapRequestDeadline(h, RequestDeadline)
	// Metrics wrap outermost too, so the well-known row counts redirects;
	// order relative to the request log (a debug aid) is immaterial.
	return &Built{
		Handler: WrapMetrics(h, cfg.Metrics, srv.BaseURI),
		Worker:  worker,
		Queue:   queue,
		Auth:    backend,
		Tombstoned: func(ctx context.Context, principal string) bool {
			// Fails open on a DB error, like the DAV gate (see compose).
			ok, _ := users.IsDeleted(ctx, principal)
			return ok
		},
		accessLog: cfg.AccessLog,
	}
}

// buildIMipQueue constructs the durable iMIP job queue, wires it into the
// server's iMIP plugin (so per-user retries hand off with the credential
// encrypted and relay-native sends go durable — JOB-QUEUE-PLAN.md), and
// registers the imip_send handler. Returns nil when the server has no iMIP
// plugin. The caller MUST run queue.Start(ctx). A configured credential key
// enables the metered per-user durable path; without one, only relay-native
// sends are durable and per-user sends fall back to a synchronous send (D5b
// surfaces any that then fail).
func buildIMipQueue(db *sql.DB, cfg Config, dsn string, srv *dav.Server) *jobs.Queue {
	var imip *caldav.IMipPlugin
	for m := range dav.Hooks[*caldav.IMipPlugin](srv) {
		imip = m
		break
	}
	if imip == nil {
		return nil
	}

	var keyring *jobs.Keyring
	if key := cfg.IMipQueue.CredentialKey; len(key) > 0 {
		kr, err := jobs.NewKeyring(jobs.Key{ID: 1, Key: key})
		if err != nil {
			// The key was validated to 32 bytes at config load, so this is a
			// programming error, not operator input — fail loudly.
			panic(fmt.Sprintf("server: iMIP credential key: %v", err))
		}
		keyring = kr
	}

	q := jobs.New(db, jobs.Config{
		DSN:           dsn,
		Keyring:       keyring,
		ReclaimAfter:  cfg.IMipQueue.ReclaimAfter,
		CredentialTTL: cfg.IMipQueue.CredentialTTL,
		Retention:     cfg.IMipQueue.Retention,
		Workers:       cfg.IMipQueue.Workers,
	})
	q.Register("imip_send", imip.DurableHandler)
	imip.Durable = q
	// Share-invite notices ride the same queue (relay identity, no credential).
	for sp := range dav.Hooks[*caldav.SharingPlugin](srv) {
		if sm, ok := sp.Notifier.(*caldav.ShareMailer); ok {
			q.Register(caldav.ShareNotifyWorker, sm.DurableHandler)
			sm.Durable = q
		}
	}
	return q
}

// WrapWellKnown adds the /.well-known/caldav and /.well-known/carddav →
// base-URI redirects (RFC 6764). It matches the two paths itself rather than
// through an http.ServeMux, which would redirect every unclean path (such as
// "/calendars//") before the DAV server could normalize it, like sabre does.
func WrapWellKnown(h http.Handler, baseURI string) http.Handler {
	redirect := http.RedirectHandler(baseURI, http.StatusMovedPermanently)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/caldav", "/.well-known/carddav":
			redirect.ServeHTTP(w, r)
		default:
			h.ServeHTTP(w, r)
		}
	})
}

// NewLoggedIn builds the production plugin stack with authentication
// replaced by a fixed principal (the auth.NewLoggedInPlugin test pattern).
// The admin privilege simulator uses it to answer "what would user X see on
// path Y" through the identical composition (ADMIN-PLAN.md §3.5).
func NewLoggedIn(db *sql.DB, cfg Config, principal string) *dav.Server {
	srv, _ := compose(db, cfg, auth.NewLoggedInPlugin(principal))
	return srv
}

// compose builds the full server; authOverride nil means real Digest/Basic
// authentication against the users table. It also returns the credential
// backend it wired (nil under authOverride), for the contacts API to share.
func compose(db *sql.DB, cfg Config, authOverride *auth.Plugin) (*dav.Server, auth.Backend) {
	if cfg.Realm == "" {
		cfg.Realm = DefaultRealm
	}

	principals := &storage.Principals{DB: db, Metrics: cfg.Metrics}
	calendars := &storage.CalDAV{DB: db, Metrics: cfg.Metrics}
	users := &storage.Users{DB: db}

	addressbooks := &storage.CardDAV{DB: db, Metrics: cfg.Metrics}

	principalCollection := acl.NewPrincipalCollection(principals, "principals")
	principalCollection.Proxies = true // expose calendar-proxy delegation principals
	calendarRoot := caldav.NewCalendarRoot(principals, calendars, "calendars")
	calendarRoot.AutoProvision = cfg.AutoProvision
	calendarRoot.PerUserProps = cfg.PerUserProps
	addressBookRoot := carddav.NewAddressBookRoot(principals, addressbooks, "addressbooks")
	addressBookRoot.AutoProvision = cfg.AutoProvision
	root := dav.NewRoot(principalCollection, calendarRoot, addressBookRoot)

	srv := dav.NewServer(cfg.BaseURI, root)
	srv.Metrics = cfg.Metrics
	// tombstoned gates soft-deleted accounts out of every real auth path. It
	// fails open (allow) on a DB error via the discarded error — a DB blip must
	// not lock everyone out; the purge job is the backstop (SOFT-DELETE-PLAN
	// §5, §12).
	tombstoned := func(ctx context.Context, principal string) bool { ok, _ := users.IsDeleted(ctx, principal); return ok }
	var backend auth.Backend
	switch {
	case authOverride != nil:
		// The injected-plugin/test path (NewLoggedIn) is left ungated on
		// purpose, matching how BeforeMethod skips fixedPrincipal.
		srv.AddPlugin(authOverride)
	case cfg.RcptdURL != "" || cfg.PasswdFile != "":
		// Authenticate against rcptd's /auth or a local Dovecot passwd-file
		// (hashes verified locally via internal/passwd). Basic-only: neither
		// source has a digesta1, so Digest can't work. First-time logins get
		// a principal from the provisioner.
		prov := &provisioner{
			principals: principals,
			writes:     &storage.AdminWrites{DB: db},
			realm:      cfg.Realm,
			seen:       map[string]time.Time{},
		}
		var rb *auth.RcptdBackend
		if cfg.PasswdFile != "" {
			var err error
			if rb, err = auth.NewPasswdFileBackend(cfg.Realm, cfg.PasswdFile, 5*time.Minute); err != nil {
				// Validate already checked the file is readable; reaching
				// here means it vanished since, or a caller skipped Validate.
				panic("server: " + err.Error())
			}
		} else {
			rb = auth.NewRcptdBackend(cfg.Realm, cfg.RcptdURL, cfg.RcptdToken, 5*time.Minute)
		}
		rb.Provision = prov.ensure
		rb.Logger = slog.Default()
		rb.Metrics = cfg.Metrics
		// The limiter wraps the backend, not the plugin, so the contacts API
		// (which calls the backend directly) is throttled too (audit 5.8).
		limited := auth.NewLimiter(rb)
		ap := auth.NewPlugin(limited)
		ap.Metrics = cfg.Metrics
		ap.Tombstoned = tombstoned
		srv.AddPlugin(ap)
		backend = limited
	default:
		basic := &auth.BasicBackend{Realm: cfg.Realm, Metrics: cfg.Metrics, Validate: func(ctx context.Context, username, password string) bool {
			// Basic verifies against the same digesta1 hash, compared in
			// constant time so verification does not leak the stored hash.
			hash, err := users.HashA1(ctx, cfg.Realm, username)
			if err != nil {
				slog.Error("basic auth: reading digesta1", "user", username, "err", err)
				return false
			}
			return hash != "" && subtle.ConstantTimeCompare(
				[]byte(auth.DigestA1(username, cfg.Realm, password)), []byte(hash)) == 1
		}}
		limited := auth.NewLimiter(
			&auth.DigestBackend{Realm: cfg.Realm, HashA1: users.HashA1, Metrics: cfg.Metrics},
			basic,
		)
		ap := auth.NewPlugin(limited)
		ap.Metrics = cfg.Metrics
		ap.Tombstoned = tombstoned
		srv.AddPlugin(ap)
		backend = limited
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
		// Each owner's free-busy visibility decides who passes calendars'
		// read-free-busy (VAVAILABILITY-PLAN.md §10).
		FreeBusyPolicy: calendars,
	})
	// AddVTimezones departs from sabre deliberately: bare IANA TZIDs (calweb
	// writes them) get the VTIMEZONE RFC 5545 requires.
	srv.AddPlugin(&caldav.Plugin{AddVTimezones: true, MaxAttendees: cfg.MaxAttendees})
	// WebDAV-Push (PUSH-PLAN.md). Registered here on purpose: after the ACL
	// plugin (so its PropFind values honor the read-deny latch) and after
	// caldav.Plugin, but before SharingPlugin (so a collection POST reaches it
	// first and a non-push body is passed on intact). Enabled-but-unconfigured
	// fails closed at startup, mirroring the ACL assertion below.
	if cfg.Push.Enabled {
		if err := cfg.Validate(); err != nil {
			// Config resolution (config.SharedServerFlags) already reports
			// this to the operator; reaching here is a caller that skipped it.
			panic("server: " + err.Error())
		}
		srv.AddPlugin(push.NewPlugin(cfg.pushConfig(), cfg.pushStore(db)))
	}
	schedule := &caldav.SchedulePlugin{
		ExpandGroups: cfg.ExpandGroups,
		MaxAttendees: cfg.MaxAttendees,
		// Serialize SCHEDULE-STATUS write-backs cluster-wide (a Postgres advisory
		// lock), so durable iMIP workers on additional nodes can't clobber one
		// another's per-attendee patches.
		WriteBackLocker: &caldav.AdvisoryLocker{DB: db},
	}
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
	// Share-invite email (docs/share-invite-email.md). Always wired so an
	// opted-in domain on a deployment that can't send is metered (no_web_url,
	// no_mailer); domains without a policy row are skipped before any send.
	shareMailer := &caldav.ShareMailer{
		SenderEmail: cfg.IMipSender,
		WebURL:      cfg.WebURL,
		Policy:      &storage.CachedSharePolicy{Source: &storage.DomainPolicy{DB: db}, TTL: time.Minute},
		Shares:      calendars,
		Principals:  principals,
		IsResource:  schedule.AutoSchedule,
		Metrics:     cfg.Metrics,
	}
	if cfg.IMipSMTP != "" {
		// Server mail through the shared relay, never as the user.
		shareMailer.Mailer = &caldav.SMTPMailer{Addr: cfg.IMipSMTP}
	}
	srv.AddPlugin(&caldav.SharingPlugin{Notifier: shareMailer})
	srv.AddPlugin(&caldav.NotificationsPlugin{})
	srv.AddPlugin(&carddav.Plugin{})
	if cfg.IMipSMTP != "" || cfg.IMipSubmission != "" {
		imip := &caldav.IMipPlugin{
			SenderEmail:     cfg.IMipSender,
			SendAsOrganizer: cfg.IMipSendAsOrganizer,
			Metrics:         cfg.Metrics,
		}
		if cfg.IMipSMTP != "" {
			// The shared relay: server-originated mail and resource replies.
			imip.Mailer = &caldav.SMTPMailer{Addr: cfg.IMipSMTP}
		}
		if cfg.IMipSubmission != "" {
			// Per-user submission: send invitations as the logged-in user.
			imip.Submission = &caldav.SubmissionMailer{
				Addr:     cfg.IMipSubmission,
				Implicit: cfg.IMipSubmissionImplicitTLS,
			}
		}
		// Wire the scheduler resolvers and — for every deployment — the
		// SCHEDULE-STATUS write-back both the durable worker and the synchronous fallback use. The
		// per-user resolvers are consulted only when Submission is set, but the
		// write-back is needed whenever an async send settles (relay-only too).
		imip.UseScheduler(schedule)
		srv.AddPlugin(imip)
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
	return srv, backend
}
