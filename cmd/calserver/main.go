// Command calserver runs the Go CalDAV server against the shared sabre
// Postgres database.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"calserver/internal/config"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	"calserver/internal/server"
	"calserver/internal/storage"
)

func main() {
	resolve := config.SharedServerFlags("Postgres DSN of the shared sabre database (e.g. postgres://localhost/calserver); empty runs an in-memory server")
	addr := flag.String("addr", ":8080", "listen address")
	tlsCert := flag.String("tls-cert", "", "path to a TLS certificate; enables HTTPS (Basic/Digest send credentials, so terminate TLS here or in front)")
	tlsKey := flag.String("tls-key", "", "path to the TLS private key (required with -tls-cert)")
	flag.Parse()

	m, err := resolve()
	if err != nil {
		slog.Error("loading config", "err", err)
		os.Exit(1)
	}
	addrV := m.Str("addr", *addr, m.File.Server.Addr)
	tlsCertV := m.Str("tls-cert", *tlsCert, m.File.Server.TLS.Cert)
	tlsKeyV := m.Str("tls-key", *tlsKey, m.File.Server.TLS.Key)

	var srv http.Handler
	if m.DSN != "" {
		db, err := storage.Open(m.DSN)
		if err != nil {
			slog.Error("opening database", "err", err)
			os.Exit(1)
		}
		if err := db.Ping(); err != nil {
			slog.Error("connecting to database", "err", err)
			os.Exit(1)
		}
		// Same line calserver-admin logs: drift shows as a string diff.
		slog.Info("effective config", "config", m.Server.Canonical())
		srv = server.New(db, m.Server)
	} else {
		// Dead-property capable in-memory root, so WebDAV suites
		// (litmus props class) can exercise PROPPATCH.
		srv = dav.NewServer(m.Server.BaseURI, mock.NewPropCollection("root", nil))
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

	slog.Info("listening", "addr", addrV, "base_uri", m.Server.BaseURI, "db", m.DSN != "", "tls", tls)
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
