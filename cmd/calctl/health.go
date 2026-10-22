package main

import (
	"context"
	"flag"
	"os"
	"time"

	"calendsd/internal/health"
)

// healthTimeout bounds the one-shot database work of the read-only commands
// (stats, doctor, contacts), the same budget a health check gets.
const healthTimeout = health.DefaultTimeout

func handleHealth(ctx context.Context, args []string) {
	flags := flag.NewFlagSet("health", flag.ExitOnError)
	jsonOutput := flags.Bool("json", false, "Output as JSON")
	urlOverride := flags.String("url", "", "Server base URL to probe (default: derived from [server] addr/base_uri)")
	dsnOverride := flags.String("db", "", "Postgres DSN to check (default: [database] dsn)")
	timeout := flags.Duration("timeout", health.DefaultTimeout, "Per-check timeout")
	flags.Parse(args)

	opts := health.OptionsFromConfig(globalConfig)
	if *urlOverride != "" {
		opts.ServerURL = *urlOverride
	}
	opts.DSN = dsnFor(*dsnOverride)
	opts.Timeout = *timeout

	// Checks run concurrently, each under its own timeout; the outer bound
	// only guards against a check that ignores its context.
	ctx, cancel := context.WithTimeout(ctx, 3**timeout+5*time.Second)
	defer cancel()
	results := health.Run(ctx, opts)

	if *jsonOutput {
		printJSON(results)
	} else {
		health.PrintTable(os.Stdout, results)
	}
	// Only "unhealthy" fails the command; "degraded" is a warning.
	if health.Failed(results) {
		os.Exit(1)
	}
}

func dsnFor(override string) string {
	if override != "" {
		return override
	}
	return globalConfig.Database.DSN
}
