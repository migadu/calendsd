// Command calserver runs the Go CalDAV server against the shared sabre
// Postgres database.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"

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
		})
	} else {
		// Dead-property capable in-memory root, so WebDAV suites
		// (litmus props class) can exercise PROPPATCH.
		srv = dav.NewServer(*baseURI, mock.NewPropCollection("root", nil))
	}

	slog.Info("listening", "addr", *addr, "base_uri", *baseURI, "db", *dsn != "")
	if err := http.ListenAndServe(*addr, srv); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
