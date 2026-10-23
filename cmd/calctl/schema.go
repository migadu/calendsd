package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"calendsd/internal/schema"
)

// handleSchema runs the schema migrations embedded in this binary
// (internal/schema/migrations) that the database has not had yet. The deploy
// runs it before restarting the server, so a release that changes the schema
// never starts against a database without the change.
func handleSchema(ctx context.Context, args []string) {
	if len(args) == 0 || args[0] != "apply" {
		fatalf("Usage: calctl schema apply [--db DSN] [--timeout DUR] [--lock-timeout DUR]")
	}
	flags := flag.NewFlagSet("schema apply", flag.ExitOnError)
	dsnOverride := dbFlag(flags)
	timeout := flags.Duration("timeout", 2*time.Minute, "Give up on the whole run after this long")
	lockTimeout := flags.Duration("lock-timeout", 5*time.Second, "Fail instead of waiting longer than this for any one table lock, which would block writes meanwhile (0 = wait)")
	flags.Parse(args[1:])

	q := openAdminQueries(ctx, *dsnOverride)
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	applied, err := schema.Apply(ctx, q.DB, *lockTimeout)
	for _, m := range applied {
		fmt.Printf("applied %04d_%s\n", m.Version, m.Name)
	}
	if err != nil {
		fatalf("Applying the schema failed, the failed migration changed nothing: %v", err)
	}
	if err := q.SchemaPresent(ctx); err != nil {
		fatalf("Schema applied, but a table is still missing: %v", err)
	}
	fmt.Printf("calendsd schema is up to date (version %d).\n", schema.Latest())
}
