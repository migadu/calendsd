package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"calserver/internal/config"
	"calserver/internal/storage"
)

const healthTimeout = 5 * time.Second

// healthResult is one line of the health report, shaped like calctl's so the
// --json output is interchangeable between the two tools.
type healthResult struct {
	Component string `json:"component"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	Duration  string `json:"duration"`
}

// handleHealth runs `calserver-admin health`: it checks the one dependency the
// panel has — the shared sabre database — and exits. It deliberately does NOT
// require -admin-auth: a health probe reads the database, it does not serve the
// panel, so demanding the htpasswd file would only get in the way (e.g. of a
// monitoring cron on the server host). The DSN is resolved from -db, else the
// same config file the panel would load.
func handleHealth(args []string) {
	flags := flag.NewFlagSet("health", flag.ExitOnError)
	configPath := flags.String("config", "", "path to a TOML config file (default: the well-known locations)")
	dsnOverride := flags.String("db", "", "Postgres DSN to check (default: [database] dsn)")
	jsonOutput := flags.Bool("json", false, "output as JSON")
	flags.Parse(args)

	dsn, err := resolveHealthDSN(*dsnOverride, *configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	result := checkDatabase(dsn)
	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(result)
	} else {
		fmt.Printf("%-10s %-10s %s (%s)\n", result.Component, result.Status, result.Message, result.Duration)
	}
	if result.Status == "unhealthy" {
		os.Exit(1)
	}
}

// resolveHealthDSN mirrors SharedServerFlags precedence for just the DSN: an
// explicit -db wins, otherwise the [database] dsn from -config or the first
// existing well-known config path.
func resolveHealthDSN(dsnOverride, configPath string) (string, error) {
	if dsnOverride != "" {
		return dsnOverride, nil
	}
	path := configPath
	if path == "" {
		for _, p := range config.DefaultConfigPaths {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
	}
	if path == "" {
		return "", nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return "", fmt.Errorf("loading config %s: %w", path, err)
	}
	return cfg.Database.DSN, nil
}

// checkDatabase pings the shared Postgres database and confirms the sabre
// schema is present — a reachable server with a missing schema is unhealthy,
// not merely slow. An empty DSN is degraded (the admin panel needs a real DB;
// it has no in-memory mode), which is a warning rather than a hard failure.
func checkDatabase(dsn string) healthResult {
	start := time.Now()
	since := func() string { return time.Since(start).Truncate(time.Millisecond).String() }

	if dsn == "" {
		return healthResult{Component: "database", Status: "degraded",
			Message: "no DSN configured (set -db or [database] dsn)", Duration: since()}
	}

	db, err := storage.Open(dsn)
	if err != nil {
		return healthResult{Component: "database", Status: "unhealthy", Message: err.Error(), Duration: since()}
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return healthResult{Component: "database", Status: "unhealthy", Message: err.Error(), Duration: since()}
	}

	if err := (&storage.AdminQueries{DB: db}).SchemaPresent(); err != nil {
		return healthResult{Component: "database", Status: "unhealthy",
			Message: fmt.Sprintf("schema check failed: %v", err), Duration: since()}
	}
	return healthResult{Component: "database", Status: "healthy",
		Message: "connected, sabre and calserver schema present", Duration: since()}
}
