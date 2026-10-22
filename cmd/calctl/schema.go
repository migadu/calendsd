package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"calserver/internal/storage"
	"calserver/scripts/schema"
)

// handleSchema applies the calserver extension schema embedded in this
// binary (scripts/schema/pgsql.calserver.sql). The deploy runs it before
// restarting the server, so a release that adds a table never starts against
// a database without it.
func handleSchema(args []string) {
	if len(args) == 0 || args[0] != "apply" {
		fatalf("Usage: calctl schema apply [--db DSN] [--timeout DUR] [--lock-timeout DUR]")
	}
	flags := flag.NewFlagSet("schema apply", flag.ExitOnError)
	dsnOverride := dbFlag(flags)
	timeout := flags.Duration("timeout", 2*time.Minute, "Give up on the whole run after this long")
	lockTimeout := flags.Duration("lock-timeout", 5*time.Second, "Fail instead of waiting longer than this for any one table lock, which would block writes meanwhile (0 = wait)")
	flags.Parse(args[1:])

	q := openAdminQueries(*dsnOverride)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := storage.ApplySchema(ctx, q.DB, *lockTimeout); err != nil {
		fatalf("Applying the calserver schema failed, nothing was changed: %v", err)
	}
	if err := q.SchemaPresent(); err != nil {
		fatalf("Schema applied, but a table is still missing: %v", err)
	}
	fmt.Printf("calserver schema is up to date (%d extension tables).\n", len(schema.Tables()))
}
