package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"calendsd/internal/storage"
)

// openAdminQueries connects to the shared Postgres database and returns the
// read-only admin query bundle. Unlike health (where a missing DSN is a
// reportable state), the data commands cannot do anything without one, so
// failures exit directly.
func openAdminQueries(ctx context.Context, dsnOverride string) *storage.AdminQueries {
	dsn := dsnFor(dsnOverride)
	if dsn == "" {
		fatalf("A Postgres DSN is required: pass --db or set [database] dsn in the config file.")
	}
	db, err := storage.Open(dsn)
	if err != nil {
		fatalf("Failed to open database: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fatalf("Failed to connect to database: %v", err)
	}
	return &storage.AdminQueries{DB: db}
}

// fatalf prints an error line to stderr and exits 1, the failure path of
// every calctl command.
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// printJSON writes v to stdout as indented JSON, the --json output of every
// command.
func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fatalf("Failed to write JSON: %v", err)
	}
}

// dbFlag registers the --db override every database command takes; resolve
// it with dsnFor.
func dbFlag(flags *flag.FlagSet) *string {
	return flags.String("db", "", "Postgres DSN (default: [database] dsn)")
}

// dash stands in for an empty table cell.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
