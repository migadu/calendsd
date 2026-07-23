// Command calserver runs the Go CalDAV server against the shared sabre
// Postgres database.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

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
	flag.Parse()

	var srv *dav.Server
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
		srv = server.New(db, server.Config{BaseURI: *baseURI, Realm: *realm})
	} else {
		srv = dav.NewServer(*baseURI, mock.NewCollection("root"))
	}

	slog.Info("listening", "addr", *addr, "base_uri", *baseURI, "db", *dsn != "")
	if err := http.ListenAndServe(*addr, srv); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
