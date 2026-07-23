// Command calserver runs the Go CalDAV server.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"calserver/internal/dav"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	baseURI := flag.String("base-uri", "/", "URI prefix the server is mounted at")
	flag.Parse()

	srv := dav.NewServer(*baseURI)

	slog.Info("listening", "addr", *addr, "base_uri", *baseURI)
	if err := http.ListenAndServe(*addr, srv); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
