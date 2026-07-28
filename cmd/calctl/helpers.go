package main

import (
	"context"
	"fmt"
	"os"

	"calserver/internal/storage"
)

// openAdminQueries connects to the shared Postgres database and returns the
// read-only admin query bundle. Unlike health (where a missing DSN is a
// reportable state), the data commands cannot do anything without one, so
// failures exit directly.
func openAdminQueries(dsnOverride string) *storage.AdminQueries {
	dsn := dsnFor(dsnOverride)
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "A Postgres DSN is required: pass --db or set [database] dsn in the config file.")
		os.Exit(1)
	}
	db, err := storage.Open(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to database: %v\n", err)
		os.Exit(1)
	}
	return &storage.AdminQueries{DB: db}
}
