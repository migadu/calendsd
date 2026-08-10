// Command calserver runs the Go CalDAV server against the shared sabre
// Postgres database.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"calserver/internal/config"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	"calserver/internal/jobs"
	"calserver/internal/logging"
	"calserver/internal/metrics"
	"calserver/internal/push"
	"calserver/internal/server"
	"calserver/internal/storage"
	"calserver/internal/version"
)

func main() {
	resolve := config.SharedServerFlags("Postgres DSN of the shared sabre database (e.g. postgres://localhost/calserver); empty runs an in-memory server")
	addr := flag.String("addr", ":8080", "listen address")
	metricsAddr := flag.String("metrics-addr", "", "listen address for Prometheus /metrics and /debug/pprof (plain HTTP, no auth — bind loopback or a firewalled interface, e.g. 127.0.0.1:9464); empty disables")
	tlsCert := flag.String("tls-cert", "", "path to a TLS certificate; enables HTTPS (Basic/Digest send credentials, so terminate TLS here or in front)")
	tlsKey := flag.String("tls-key", "", "path to the TLS private key (required with -tls-cert)")
	showVersion := version.Flags()
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String("calserver"))
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
	addrV := m.Str("addr", *addr, m.File.Server.Addr)
	metricsAddrV := m.Str("metrics-addr", *metricsAddr, m.File.Server.MetricsAddr)
	tlsCertV := m.Str("tls-cert", *tlsCert, m.File.Server.TLS.Cert)
	tlsKeyV := m.Str("tls-key", *tlsKey, m.File.Server.TLS.Key)

	// A nil Set is valid everywhere and records nothing, so all wiring below
	// is unconditional.
	var set *metrics.Set
	if metricsAddrV != "" {
		// version.Version surfaces as the calserver_build_info metric.
		set = metrics.New(version.Version)
		m.Server.Metrics = set
	}

	// A signal-cancelled context drives graceful shutdown of the HTTP server
	// and the WebDAV-Push worker (which holds a LISTEN connection).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var srv http.Handler
	var pushWorker *push.Worker
	var jobQueue *jobs.Queue
	if m.DSN != "" {
		db, err := storage.OpenPool(m.DSN, m.DBPool)
		if err != nil {
			slog.Error("opening database", "err", err)
			os.Exit(1)
		}
		if err := db.Ping(); err != nil {
			slog.Error("connecting to database", "err", err)
			os.Exit(1)
		}
		set.RegisterDBStats(db)
		// Same line calserver-admin logs: drift shows as a string diff.
		slog.Info("effective config", "config", m.Server.Canonical())
		srv, pushWorker, jobQueue = server.NewWithBackground(db, m.Server, m.DSN)
	} else {
		// Dead-property capable in-memory root, so WebDAV suites
		// (litmus props class) can exercise PROPPATCH.
		davSrv := dav.NewServer(m.Server.BaseURI, mock.NewPropCollection("root", nil))
		davSrv.Metrics = set
		srv = server.WrapMetrics(davSrv, set, davSrv.BaseURI)
	}

	if metricsAddrV != "" {
		go serveMetrics(metricsAddrV, set)
	}

	// Deliver WebDAV-Push notifications in the background (advertisement and
	// registration already work through the plugin; this drives delivery).
	if pushWorker != nil {
		go pushWorker.Run(ctx)
	}

	// Process durable scheduling sends (per-user retries + relay-native sends).
	// Start spawns its own goroutines and returns; the shared ctx stops it on
	// shutdown. Without this, enqueued sends would never run.
	if jobQueue != nil {
		jobQueue.Start(ctx)
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

	// Shut the HTTP server down when a termination signal cancels ctx, so the
	// listener stops and the push worker's ctx (shared) unwinds its LISTEN
	// connection cleanly.
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutCtx); err != nil {
			slog.Warn("graceful shutdown", "err", err)
		}
	}()

	ver, commit, built := version.Info()
	slog.Info("listening", "addr", addrV, "base_uri", m.Server.BaseURI, "db", m.DSN != "", "tls", tls,
		"version", ver, "commit", commit, "built", built)
	if tls {
		err = httpSrv.ListenAndServeTLS(tlsCertV, tlsKeyV)
	} else {
		err = httpSrv.ListenAndServe()
	}
	// A clean Shutdown returns ErrServerClosed from ListenAndServe; that is the
	// expected exit on SIGINT/SIGTERM, not a failure.
	if err != nil && err != http.ErrServerClosed {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}

// serveMetrics runs the metrics/pprof listener. It exits the process on
// failure rather than log-and-continue: a monitoring endpoint that silently
// fails to come up is the one failure monitoring must not have.
func serveMetrics(addr string, set *metrics.Set) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", set.Handler())
	// pprof answers the CPU questions a histogram can't; same privacy
	// posture as /metrics, zero series cardinality.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	msrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}
	slog.Info("metrics listening", "addr", addr)
	if err := msrv.ListenAndServe(); err != nil {
		slog.Error("metrics server exited", "err", err)
		os.Exit(1)
	}
}
