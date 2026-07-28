package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"calserver/internal/storage"
)

const healthTimeout = 5 * time.Second

type checkResult struct {
	Component string `json:"component"`
	Status    string `json:"status"`
	Endpoint  string `json:"endpoint,omitempty"`
	Message   string `json:"message,omitempty"`
	Duration  string `json:"duration"`
}

func handleHealth(args []string) {
	flags := flag.NewFlagSet("health", flag.ExitOnError)
	jsonOutput := flags.Bool("json", false, "Output as JSON")
	urlOverride := flags.String("url", "", "Server base URL to probe (default: derived from [server] addr/base_uri)")
	dsnOverride := flags.String("db", "", "Postgres DSN to check (default: [database] dsn)")
	flags.Parse(args)

	results := []checkResult{
		checkServer(serverURL(*urlOverride)),
		checkDatabase(dsnFor(*dsnOverride)),
	}
	// rcptd auth is optional; only a configured URL makes it a component
	// worth reporting on.
	if rcptdURL := strings.TrimRight(globalConfig.Server.Rcptd.URL, "/"); rcptdURL != "" {
		results = append(results, checkRcptd(rcptdURL, rcptdToken()))
	}

	// Only a hard failure ("unhealthy") fails the command; "degraded"
	// (e.g. no DSN configured — in-memory mode) is a warning.
	hasFailure := false
	for _, r := range results {
		if r.Status == "unhealthy" {
			hasFailure = true
			break
		}
	}

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(results)
	} else {
		printHealthTable(results)
	}

	if hasFailure {
		os.Exit(1)
	}
}

// serverURL derives the probe URL from the config's listen address and mount
// point: a wildcard listen host becomes loopback, and a configured TLS cert
// means the listener speaks HTTPS.
func serverURL(override string) string {
	if override != "" {
		return override
	}
	addr := globalConfig.Server.Addr
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, "8080"
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	scheme := "http"
	if globalConfig.Server.TLS.Cert != "" {
		scheme = "https"
	}
	base := globalConfig.Server.BaseURI
	if base == "" {
		base = "/"
	}
	if !strings.HasPrefix(base, "/") {
		base = "/" + base
	}
	return scheme + "://" + net.JoinHostPort(host, port) + base
}

func dsnFor(override string) string {
	if override != "" {
		return override
	}
	return globalConfig.Database.DSN
}

// checkServer probes the DAV server with an unauthenticated OPTIONS request.
// Any HTTP response proves the listener is up — auth typically answers 401
// before the DAV plugins run, so the status code is not a failure signal.
// When a DAV header does come back it is echoed as extra confirmation.
func checkServer(url string) checkResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, url, nil)
	if err != nil {
		return checkResult{
			Component: "calserver",
			Status:    "unhealthy",
			Endpoint:  url,
			Message:   fmt.Sprintf("invalid url: %v", err),
			Duration:  time.Since(start).Truncate(time.Millisecond).String(),
		}
	}
	resp, err := http.DefaultClient.Do(req)
	d := time.Since(start)
	if err != nil {
		return checkResult{
			Component: "calserver",
			Status:    "unhealthy",
			Endpoint:  url,
			Message:   err.Error(),
			Duration:  d.Truncate(time.Millisecond).String(),
		}
	}
	resp.Body.Close()
	var msg string
	switch {
	case resp.Header.Get("DAV") != "":
		msg = "responding (DAV: " + resp.Header.Get("DAV") + ")"
	case resp.StatusCode == http.StatusUnauthorized:
		// The expected answer to an unauthenticated probe.
		msg = "responding (auth required)"
	default:
		msg = fmt.Sprintf("responding (HTTP %d)", resp.StatusCode)
	}
	return checkResult{
		Component: "calserver",
		Status:    "healthy",
		Endpoint:  url,
		Message:   msg,
		Duration:  d.Truncate(time.Millisecond).String(),
	}
}

// checkDatabase pings the shared Postgres database and, as a schema sanity
// check, runs the admin dashboard's table counts — a reachable server with a
// missing sabre schema is unhealthy, not just slow.
func checkDatabase(dsn string) checkResult {
	start := time.Now()
	if dsn == "" {
		return checkResult{
			Component: "database",
			Status:    "degraded",
			Message:   "not configured (server would run in-memory)",
			Duration:  time.Since(start).Truncate(time.Millisecond).String(),
		}
	}

	db, err := storage.Open(dsn)
	if err != nil {
		return checkResult{
			Component: "database",
			Status:    "unhealthy",
			Message:   err.Error(),
			Duration:  time.Since(start).Truncate(time.Millisecond).String(),
		}
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return checkResult{
			Component: "database",
			Status:    "unhealthy",
			Message:   err.Error(),
			Duration:  time.Since(start).Truncate(time.Millisecond).String(),
		}
	}

	err = (&storage.AdminQueries{DB: db}).SchemaPresent()
	d := time.Since(start)
	if err != nil {
		return checkResult{
			Component: "database",
			Status:    "unhealthy",
			Message:   fmt.Sprintf("schema check failed: %v", err),
			Duration:  d.Truncate(time.Millisecond).String(),
		}
	}
	return checkResult{
		Component: "database",
		Status:    "healthy",
		Message:   "connected, sabre schema present",
		Duration:  d.Truncate(time.Millisecond).String(),
	}
}

// rcptdToken resolves the bearer token the same way SharedServerFlags does:
// config file first, then $RCPTD_AUTH_TOKEN.
func rcptdToken() string {
	if t := globalConfig.Server.Rcptd.Token; t != "" {
		return t
	}
	return os.Getenv("RCPTD_AUTH_TOKEN")
}

// checkRcptd probes the /auth endpoint calserver's rcptd backend actually
// calls, with a dummy address, and maps the statuses the way the backend
// interprets them: 404 (unknown user) proves a working service just as well
// as 200; 401 means our bearer token is rejected; 503 is rcptd's transient
// CDB fault.
func checkRcptd(baseURL, token string) checkResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/auth?email=calctl-health-probe%40invalid", nil)
	if err != nil {
		return checkResult{
			Component: "rcptd",
			Status:    "unhealthy",
			Endpoint:  baseURL,
			Message:   fmt.Sprintf("invalid url: %v", err),
			Duration:  time.Since(start).Truncate(time.Millisecond).String(),
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	d := time.Since(start)
	if err != nil {
		return checkResult{
			Component: "rcptd",
			Status:    "unhealthy",
			Endpoint:  baseURL,
			Message:   err.Error(),
			Duration:  d.Truncate(time.Millisecond).String(),
		}
	}
	resp.Body.Close()

	status, msg := "unhealthy", fmt.Sprintf("unexpected HTTP %d from /auth", resp.StatusCode)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNotFound:
		// 404 = probe address unknown, which proves /auth and its tables
		// work just as well as a 200 would.
		status, msg = "healthy", "auth endpoint responding"
	case http.StatusUnauthorized:
		msg = "bearer token rejected — check [server.rcptd] token / $RCPTD_AUTH_TOKEN"
	case http.StatusServiceUnavailable:
		status, msg = "degraded", "temporarily unavailable (transient table fault)"
	}
	return checkResult{
		Component: "rcptd",
		Status:    status,
		Endpoint:  baseURL,
		Message:   msg,
		Duration:  d.Truncate(time.Millisecond).String(),
	}
}

func printHealthTable(results []checkResult) {
	fmt.Println("Health Check")
	fmt.Println(strings.Repeat("=", 70))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COMPONENT\tSTATUS\tENDPOINT\tMESSAGE\tDURATION")
	for _, r := range results {
		endpoint := r.Endpoint
		if endpoint == "" {
			endpoint = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Component, colorStatus(r.Status), endpoint, r.Message, r.Duration)
	}
	w.Flush()
}

func colorStatus(status string) string {
	// Pad status to fixed visible width so ANSI codes don't break tabwriter alignment.
	padded := fmt.Sprintf("%-9s", status)
	switch status {
	case "healthy":
		return "\033[32m" + padded + "\033[0m"
	case "unhealthy":
		return "\033[31m" + padded + "\033[0m"
	case "degraded":
		return "\033[33m" + padded + "\033[0m"
	default:
		return padded
	}
}
