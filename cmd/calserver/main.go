// Command calserver runs the Go CalDAV server.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"calserver/internal/dav"
	"calserver/internal/dav/mock"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	baseURI := flag.String("base-uri", "/", "URI prefix the server is mounted at")
	flag.Parse()

	// The storage-backed tree lands in phase 3; until then the server
	// exposes an empty in-memory root.
	srv := dav.NewServer(*baseURI, mock.NewCollection("root"))

	slog.Info("listening", "addr", *addr, "base_uri", *baseURI)
	if err := http.ListenAndServe(*addr, srv); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
