// Command calendsd runs the Go CalDAV server against the shared sabre
// Postgres database.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"calendsd/internal/config"
	"calendsd/internal/dav"
	"calendsd/internal/dav/mock"
	"calendsd/internal/jobs"
	"calendsd/internal/logging"
	"calendsd/internal/metrics"
	"calendsd/internal/push"
	"calendsd/internal/server"
	"calendsd/internal/storage"
	"calendsd/internal/version"
)

func main() {
	resolve := config.SharedServerFlags("Postgres DSN of the shared sabre database (e.g. postgres://localhost/calserver); empty runs an in-memory server")
	addr := flag.String("addr", ":8080", "listen address")
	metricsAddr := flag.String("metrics-addr", "", "listen address for Prometheus /metrics and /debug/pprof (plain HTTP, no auth — bind loopback or a firewalled interface, e.g. 127.0.0.1:9464); empty disables")
	apiAddr := flag.String("api-addr", "", "listen address for the webmail contacts JSON API (plain HTTP, Basic auth with mailbox credentials via the same backend as DAV; loopback only, e.g. 127.0.0.1:8081); empty disables. Needs -db")
	tlsCert := flag.String("tls-cert", "", "path to a TLS certificate; enables HTTPS (Basic/Digest send credentials, so terminate TLS here or in front)")
	tlsKey := flag.String("tls-key", "", "path to the TLS private key (required with -tls-cert)")
	showVersion := version.Flags()
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String("calendsd"))
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
	if m.ConfigPath != "" {
		slog.Info("loaded config", "path", m.ConfigPath)
	}
	addrV := m.Str("addr", *addr, m.File.Server.Addr)
	metricsAddrV := m.Str("metrics-addr", *metricsAddr, m.File.Server.MetricsAddr)
	apiAddrV := m.Str("api-addr", *apiAddr, m.File.Server.APIAddr)
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
	var contactsAPI http.Handler
	var db *sql.DB
	if m.DSN != "" {
		db, err = storage.OpenPool(m.DSN, m.DBPool)
		if err != nil {
			slog.Error("opening database", "err", err)
			os.Exit(1)
		}
		if err := db.Ping(); err != nil {
			slog.Error("connecting to database", "err", err)
			os.Exit(1)
		}
		set.RegisterDBStats(db)
		set.RegisterJobDepth(db)
		// Same line `calctl panel` logs: drift shows as a string diff.
		slog.Info("effective config", "config", m.Server.Canonical())
		built := server.Build(db, m.Server, m.DSN)
		srv, pushWorker, jobQueue = built.Handler, built.Worker, built.Queue
		if apiAddrV != "" {
			contactsAPI = built.ContactsAPI(db)
		}
	} else {
		// Dead-property capable in-memory root, so WebDAV suites
		// (litmus props class) can exercise PROPPATCH.
		davSrv := dav.NewServer(m.Server.BaseURI, mock.NewPropCollection("root", nil))
		davSrv.Metrics = set
		srv = server.WrapMetrics(server.WrapHealth(server.WrapRecover(davSrv), nil), set, davSrv.BaseURI)
	}

	if metricsAddrV != "" {
		go serveMetrics(metricsAddrV, set)
	}

	// background tracks what must finish before the process exits once ctx
	// is cancelled: the contacts API's drain, the push worker, the job queue.
	var background sync.WaitGroup

	// The webmail contacts API: its own loopback listener, sharing the DAV
	// server's database and credential backend, never mounted on the DAV
	// port. Needs the database — there is nothing to serve in-memory.
	switch {
	case apiAddrV != "" && contactsAPI == nil:
		slog.Warn("api-addr set but no database configured; the contacts API is not started", "addr", apiAddrV)
	case contactsAPI != nil:
		background.Add(1)
		go func() {
			defer background.Done()
			serveContactsAPI(ctx, apiAddrV, contactsAPI)
		}()
	}

	// Deliver WebDAV-Push notifications in the background (advertisement and
	// registration already work through the plugin; this drives delivery).
	if pushWorker != nil {
		background.Add(1)
		go func() {
			defer background.Done()
			pushWorker.Run(ctx)
		}()
	}

	// Prune sync change rows older than [retention].change_log_days.
	if db != nil {
		if retention := config.ChangeLogRetention(m.File.Retention.ChangeLogDays); retention > 0 {
			background.Add(1)
			go func() {
				defer background.Done()
				storage.RunChangeLogPruner(ctx, db, retention, time.Hour)
			}()
		}
	}

	// Process durable scheduling sends (per-user retries + relay-native sends).
	// Start spawns its own goroutines and returns; the shared ctx stops it on
	// shutdown. Without this, enqueued sends would never run.
	if jobQueue != nil {
		jobQueue.Start(ctx)
		background.Add(1)
		go func() {
			defer background.Done()
			<-ctx.Done()
			jobQueue.Wait()
		}()
	}

	httpSrv := &http.Server{
		Addr:              addrV,
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
		// 300 s rather than 60: a whole-calendar export or a large import
		// PUT over a slow link was cut off mid-body. The per-request
		// deadline the server installs (server.RequestDeadline) is what
		// bounds the work behind a connection; these only bound the bytes.
		ReadTimeout:    server.RequestDeadline,
		WriteTimeout:   server.RequestDeadline,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MiB of request headers
	}

	tls := tlsCertV != "" || tlsKeyV != ""
	if tls && (tlsCertV == "" || tlsKeyV == "") {
		slog.Error("TLS requires both a certificate and a key (tls-cert/tls-key)")
		os.Exit(1)
	}

	// Shut the HTTP server down when a termination signal cancels ctx, so the
	// listener stops and the push worker's ctx (shared) unwinds its LISTEN
	// connection cleanly. ListenAndServe returns as soon as Shutdown starts;
	// shutDone closes once in-flight requests have drained.
	shutDone := make(chan struct{})
	go func() {
		defer close(shutDone)
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
	<-shutDone
	waitBackground(&background, 30*time.Second)
}

// waitBackground waits for the background services to finish after ctx is
// cancelled — running jobs settle, the push worker closes its connection —
// but no longer than limit, so one stuck send cannot hold the process.
func waitBackground(wg *sync.WaitGroup, limit time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		slog.Warn("shutdown: background work still running; exiting anyway", "waited", limit)
	}
}

// serveContactsAPI runs the contacts API listener until ctx is cancelled,
// then shuts it down gracefully, returning once in-flight requests have
// drained. It exits the process on a bind failure, the
// same posture as the metrics listener: a configured listener that silently
// is not there is worse than a crash at startup.
func serveContactsAPI(ctx context.Context, addr string, h http.Handler) {
	asrv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	shutDone := make(chan struct{})
	go func() {
		defer close(shutDone)
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := asrv.Shutdown(shutCtx); err != nil {
			slog.Warn("contacts api shutdown", "err", err)
		}
	}()
	slog.Info("contacts api listening", "addr", addr)
	if err := asrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("contacts api server exited", "err", err)
		os.Exit(1)
	}
	<-shutDone
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
