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
	msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
	if dav := resp.Header.Get("DAV"); dav != "" {
		msg += fmt.Sprintf(" (DAV: %s)", dav)
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

	counts, err := (&storage.AdminQueries{DB: db}).Counts()
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
		Message: fmt.Sprintf("%d principals, %d calendars, %d objects, %d cards",
			counts.Principals, counts.Calendars, counts.CalendarObjects, counts.Cards),
		Duration: d.Truncate(time.Millisecond).String(),
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
