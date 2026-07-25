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

	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	"calserver/internal/server"
	"calserver/internal/storage"
)

func main() {
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

	var srv http.Handler
	if *dsn != "" {
		db, err := storage.Open(*dsn)
		if err != nil {
			slog.Error("opening database", "err", err)
			os.Exit(1)
		}
		if err := db.Ping(); err != nil {
			slog.Error("connecting to database", "err", err)
			os.Exit(1)
		}
		var autoPrefixes []string
		if *autoSchedule != "" {
			autoPrefixes = strings.Split(*autoSchedule, ",")
		}
		srv = server.New(db, server.Config{
			BaseURI: *baseURI, Realm: *realm,
			IMipSMTP: *imipSMTP, IMipSender: *imipSender,
			Browser: *browser, AutoSchedulePrefixes: autoPrefixes,
			PrincipalsReadable: *principalsReadable,
			AutoProvision:      *autoProvision,
			PerUserProps:       *perUserProps,
			ExpandGroups:       *expandGroups,
		})
	} else {
		// Dead-property capable in-memory root, so WebDAV suites
		// (litmus props class) can exercise PROPPATCH.
		srv = dav.NewServer(*baseURI, mock.NewPropCollection("root", nil))
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB of request headers
	}

	tls := *tlsCert != "" || *tlsKey != ""
	if tls && (*tlsCert == "" || *tlsKey == "") {
		slog.Error("TLS requires both -tls-cert and -tls-key")
		os.Exit(1)
	}

	slog.Info("listening", "addr", *addr, "base_uri", *baseURI, "db", *dsn != "", "tls", tls)
	var err error
	if tls {
		err = httpSrv.ListenAndServeTLS(*tlsCert, *tlsKey)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
