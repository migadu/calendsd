package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"calserver/internal/storage"
)

// handleStats prints the admin dashboard's overview from the command line:
// per-table row counts plus the newest change-log rows (ADMIN-PLAN.md §3.1).
func handleStats(ctx context.Context, args []string) {
	flags := flag.NewFlagSet("stats", flag.ExitOnError)
	jsonOutput := flags.Bool("json", false, "Output as JSON")
	dsnOverride := dbFlag(flags)
	changes := flags.Int("changes", 10, "Recent change-log rows to show per change table (0 disables)")
	flags.Parse(args)

	q := openAdminQueries(ctx, *dsnOverride)
	counts, err := q.Counts(ctx)
	if err != nil {
		fatalf("Failed to count tables: %v", err)
	}
	var recent []storage.AdminChange
	if *changes > 0 {
		recent, err = q.RecentChanges(ctx, *changes)
		if err != nil {
			fatalf("Failed to load recent changes: %v", err)
		}
	}

	if *jsonOutput {
		printJSON(struct {
			Counts        *storage.AdminCounts  `json:"counts"`
			RecentChanges []storage.AdminChange `json:"recentChanges"`
		}{counts, recent})
		return
	}

	fmt.Println("Row Counts")
	fmt.Println(strings.Repeat("=", 70))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, c := range []struct {
		label string
		n     int
	}{
		{"principals", counts.Principals},
		{"users", counts.Users},
		{"calendars", counts.Calendars},
		{"calendar instances", counts.CalendarInstances},
		{"calendar objects", counts.CalendarObjects},
		{"address books", counts.AddressBooks},
		{"cards", counts.Cards},
		{"scheduling objects", counts.SchedulingObjects},
		{"property rows", counts.PropertyRows},
	} {
		fmt.Fprintf(w, "%s\t%d\n", c.label, c.n)
	}
	w.Flush()

	if *changes <= 0 {
		return
	}
	fmt.Println()
	fmt.Println("Recent Changes")
	fmt.Println(strings.Repeat("=", 70))
	if len(recent) == 0 {
		fmt.Println("(none)")
		return
	}
	w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KIND\tCOLLECTION\tPRINCIPAL\tOBJECT\tOP\tTOKEN\tMODIFIED")
	for _, ch := range recent {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			ch.Kind, ch.CollectionURI, ch.PrincipalURI, ch.ObjectURI,
			operationName(ch.Operation), ch.SyncToken, modifiedTime(ch.LastModified))
	}
	w.Flush()
}

// operationName translates sabre's change-log operation codes.
func operationName(op int) string {
	switch op {
	case 1:
		return "add"
	case 2:
		return "modify"
	case 3:
		return "delete"
	default:
		return fmt.Sprintf("op=%d", op)
	}
}

// modifiedTime renders the joined lastmodified; deletes have none (the
// change tables carry no timestamp of their own).
func modifiedTime(unix *int64) string {
	if unix == nil {
		return "-"
	}
	return time.Unix(*unix, 0).Format("2006-01-02 15:04:05")
}
