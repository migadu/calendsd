// Command calserver-admin runs the admin panel (ADMIN-PLAN.md): a
// standalone read-first debugging UI over the same shared sabre Postgres
// database calserver serves. It accepts the same config file and
// Calendar-Server flags as calserver — resolved through the same
// config.SharedServerFlags code — so the effective-config echo on the
// dashboard matches the server's startup log line by construction.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"calserver/internal/admin"
	"calserver/internal/config"
	"calserver/internal/dav"
	"calserver/internal/logging"
	"calserver/internal/server"
	"calserver/internal/storage"
	"calserver/internal/version"
)

func main() {
	resolve := config.SharedServerFlags("Postgres DSN of the shared sabre database (required)")
	addr := flag.String("addr", "127.0.0.1:8081", "listen address (Basic auth resends the password: loopback/LAN or behind TLS only)")
	authFile := flag.String("admin-auth", "", "htpasswd file with bcrypt entries (htpasswd -B) for admin logins (required)")
	readonly := flag.Bool("readonly", true, "disable write actions (Phase C); reads only")
	showVersion := version.Flags()
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String("calserver-admin"))
		return
	}

	m, err := resolve()
	if err != nil {
		slog.Error("loading config", "err", err)
		os.Exit(1)
	}
	if err := logging.Setup(m.LogLevel); err != nil {
		slog.Error("configuring logging", "err", err)
		os.Exit(1)
	}
	cfg := m.Server
	if cfg.Realm == "" {
		cfg.Realm = "SabreDAV"
	}

	if m.DSN == "" {
		slog.Error("a Postgres DSN is required (-db); the admin panel debugs real databases")
		os.Exit(1)
	}
	if *authFile == "" {
		slog.Error("-admin-auth is required: an htpasswd file with bcrypt entries (htpasswd -B)")
		os.Exit(1)
	}
	creds, err := admin.LoadHTPasswd(*authFile)
	if err != nil {
		slog.Error("loading admin credentials", "err", err)
		os.Exit(1)
	}

	db, err := storage.OpenPool(m.DSN, m.DBPool)
	if err != nil {
		slog.Error("opening database", "err", err)
		os.Exit(1)
	}
	if err := db.Ping(); err != nil {
		slog.Error("connecting to database", "err", err)
		os.Exit(1)
	}

	// The synthetic-request servers differ from the live config in two
	// deliberate ways. BaseURI is forced to "/": admin paths are
	// server-root-relative, and privileges/ACLs don't depend on the mount
	// point. AutoProvision is forced off for the simulator so that a mere
	// GET /simulate can never write to the live database — only the
	// explicitly gated provision action gets the provisioning composition.
	simCfg := cfg
	simCfg.BaseURI = "/"
	simCfg.AutoProvision = false
	provisionCfg := cfg
	provisionCfg.BaseURI = "/"
	provisionCfg.AutoProvision = true

	calendars := &storage.CalDAV{DB: db}
	app := &admin.App{
		Store:        &storage.AdminQueries{DB: db},
		Principals:   &storage.Principals{DB: db},
		Calendars:    calendars,
		AddressBooks: &storage.CardDAV{DB: db},
		Scheduling:   calendars,
		Writes:       &storage.AdminWrites{DB: db},
		NewSimulator: func(principal string) *dav.Server {
			return server.NewLoggedIn(db, simCfg, principal)
		},
		NewProvisioner: func(principal string) *dav.Server {
			return server.NewLoggedIn(db, provisionCfg, principal)
		},
		Config:   cfg,
		ReadOnly: *readonly,
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           app.AuthenticatedHandler(creds),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	// Same line calserver logs: drift shows as a string diff (§2).
	slog.Info("effective config", "config", cfg.Canonical())
	ver, commit, built := version.Info()
	slog.Info("admin panel listening", "addr", *addr, "readonly", *readonly,
		"version", ver, "commit", commit, "built", built)
	if err := httpSrv.ListenAndServe(); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
