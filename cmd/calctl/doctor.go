package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

// handleDoctor runs the read-only invariant checks (ADMIN-PLAN.md §3.7) and
// reports every finding. Exit code 1 when anything is wrong, so it can sit
// in monitoring next to `calctl health`.
func handleDoctor(args []string) {
	flags := flag.NewFlagSet("doctor", flag.ExitOnError)
	jsonOutput := flags.Bool("json", false, "Output as JSON")
	dsnOverride := dbFlag(flags)
	scanCap := flags.Int("scan-cap", 0, "Max objects/cards to deep-scan per table (0 = default 5000)")
	flags.Parse(args)

	q := openAdminQueries(*dsnOverride)
	report, err := q.Doctor(*scanCap)
	if err != nil {
		fatalf("Doctor run failed: %v", err)
	}

	if *jsonOutput {
		printJSON(report)
	} else {
		fmt.Println("Doctor Report")
		fmt.Println(strings.Repeat("=", 70))
		if len(report.Findings) == 0 {
			fmt.Println("No findings.")
		} else {
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "CHECK\tSUBJECT\tDETAIL")
			for _, f := range report.Findings {
				fmt.Fprintf(w, "%s\t%s\t%s\n", f.Check, f.Subject, f.Detail)
			}
			w.Flush()
		}
		fmt.Printf("\nScanned %d calendar objects, %d cards.\n",
			report.ObjectsScanned, report.CardsScanned)
		if report.Truncated {
			fmt.Println("Deep scan truncated by the scan cap — raise --scan-cap for full coverage.")
		}
	}

	if len(report.Findings) > 0 {
		os.Exit(1)
	}
}
