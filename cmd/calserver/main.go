// Command calserver runs the Go CalDAV server against the shared sabre
// Postgres database.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"calserver/internal/config"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	"calserver/internal/server"
	"calserver/internal/storage"
)

func main() {
	configPath := flag.String("config", "", "path to a TOML config file (see config.toml.example); command-line flags override its values")
	addr := flag.String("addr", ":8080", "listen address")
	baseURI := flag.String("base-uri", "/", "URI prefix the server is mounted at")
	dsn := flag.String("db", "", "Postgres DSN of the shared sabre database (e.g. postgres://localhost/calserver); empty runs an in-memory server")
	realm := flag.String("realm", "SabreDAV", "auth realm; must match the realm baked into users.digesta1")
	imipSMTP := flag.String("imip-smtp", "", "SMTP host:port for emailing invites to external attendees (empty disables iMip)")
	imipSender := flag.String("imip-sender", "", "From address for iMip invitation emails")
	browser := flag.Bool("browser", false, "serve HTML indexes for GET on collections (like sabre's Browser plugin; default matches sabre without it: 501)")
	autoSchedule := flag.String("auto-schedule-prefixes", "", "comma-separated principal-name prefixes (e.g. resource,location) whose scheduling messages are auto-accepted Calendar-Server style; empty disables")
	principalsReadable := flag.Bool("principals-readable", false, "grant authenticated users read access to principal resources (Calendar-Server behavior)")
	autoProvision := flag.Bool("auto-provision", false, "recreate the default calendar/addressbook when a principal has none (Calendar-Server behavior)")
	perUserProps := flag.Bool("per-user-props", false, "store non-owner property writes per user and overlay them on reads (Calendar-Server behavior)")
	expandGroups := flag.Bool("expand-groups", false, "expand group ATTENDEEs into per-member invitations (Calendar-Server behavior)")
	tlsCert := flag.String("tls-cert", "", "path to a TLS certificate; enables HTTPS (Basic/Digest send credentials, so terminate TLS here or in front)")
	tlsKey := flag.String("tls-key", "", "path to the TLS private key (required with -tls-cert)")
	flag.Parse()

	// A config file supplies defaults; any flag given on the command line
	// overrides its value (tracked via flag.Visit).
	var file config.Config
	if *configPath != "" {
		c, err := config.Load(*configPath)
		if err != nil {
			slog.Error("loading config", "err", err)
			os.Exit(1)
		}
		file = *c
	}
	setFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	str := func(name, flagVal, fileVal string) string {
		if setFlags[name] || fileVal == "" {
			return flagVal
		}
		return fileVal
	}
	boolean := func(name string, flagVal, fileVal bool) bool {
		if setFlags[name] {
			return flagVal
		}
		return fileVal
	}

	addrV := str("addr", *addr, file.Server.Addr)
	baseURIV := str("base-uri", *baseURI, file.Server.BaseURI)
	dsnV := str("db", *dsn, file.Database.DSN)
	realmV := str("realm", *realm, file.Server.Realm)
	tlsCertV := str("tls-cert", *tlsCert, file.Server.TLS.Cert)
	tlsKeyV := str("tls-key", *tlsKey, file.Server.TLS.Key)

	autoPrefixes := file.CalendarServer.AutoSchedulePrefixes
	if setFlags["auto-schedule-prefixes"] || (autoPrefixes == nil && *autoSchedule != "") {
		autoPrefixes = nil
		if *autoSchedule != "" {
			autoPrefixes = strings.Split(*autoSchedule, ",")
		}
	}

	var srv http.Handler
	if dsnV != "" {
		db, err := storage.Open(dsnV)
		if err != nil {
			slog.Error("opening database", "err", err)
			os.Exit(1)
		}
		if err := db.Ping(); err != nil {
			slog.Error("connecting to database", "err", err)
			os.Exit(1)
		}
		srv = server.New(db, server.Config{
			BaseURI: baseURIV, Realm: realmV,
			IMipSMTP:             str("imip-smtp", *imipSMTP, file.Scheduling.IMip.SMTP),
			IMipSender:           str("imip-sender", *imipSender, file.Scheduling.IMip.Sender),
			Browser:              boolean("browser", *browser, file.CalendarServer.Browser),
			AutoSchedulePrefixes: autoPrefixes,
			PrincipalsReadable:   boolean("principals-readable", *principalsReadable, file.CalendarServer.PrincipalsReadable),
			AutoProvision:        boolean("auto-provision", *autoProvision, file.CalendarServer.AutoProvision),
			PerUserProps:         boolean("per-user-props", *perUserProps, file.CalendarServer.PerUserProps),
			ExpandGroups:         boolean("expand-groups", *expandGroups, file.CalendarServer.ExpandGroups),
		})
	} else {
		// Dead-property capable in-memory root, so WebDAV suites
		// (litmus props class) can exercise PROPPATCH.
		srv = dav.NewServer(baseURIV, mock.NewPropCollection("root", nil))
	}

	httpSrv := &http.Server{
		Addr:              addrV,
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB of request headers
	}

	tls := tlsCertV != "" || tlsKeyV != ""
	if tls && (tlsCertV == "" || tlsKeyV == "") {
		slog.Error("TLS requires both a certificate and a key (tls-cert/tls-key)")
		os.Exit(1)
	}

	slog.Info("listening", "addr", addrV, "base_uri", baseURIV, "db", dsnV != "", "tls", tls)
	var err error
	if tls {
		err = httpSrv.ListenAndServeTLS(tlsCertV, tlsKeyV)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
