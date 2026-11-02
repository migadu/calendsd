package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/migadu/calendsd/internal/jobs"
)

// handleJobs dispatches the generic `calctl jobs` subcommands: read-only
// list/show and the guarded state mutations retry/cancel/reschedule/delete. It
// operates on the durable job table (docs/design/JOB-QUEUE-PLAN.md) by id and never touches
// the encryption key or the credential — only queue state (redacted to presence).
func handleJobs(ctx context.Context, args []string) {
	if len(args) == 0 {
		fatalf("Usage: calctl jobs <list|show|retry|cancel|reschedule|delete> [options]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		jobsList(ctx, rest)
	case "show":
		jobsShow(ctx, rest)
	case "retry":
		jobsRetry(ctx, rest)
	case "cancel":
		jobsCancel(ctx, rest)
	case "reschedule":
		jobsReschedule(ctx, rest)
	case "delete", "rm":
		jobsDelete(ctx, rest)
	default:
		fatalf("Unknown jobs subcommand: %s (want list, show, retry, cancel, reschedule, or delete)", sub)
	}
}

func jobsOps(ctx context.Context, dsnOverride string) *jobs.Ops {
	return &jobs.Ops{DB: openAdminQueries(ctx, dsnOverride).DB}
}

func jobsList(ctx context.Context, args []string) {
	flags := flag.NewFlagSet("jobs list", flag.ExitOnError)
	state := flags.String("state", "", "Filter by state (available|executing|completed|discarded)")
	worker := flags.String("worker", "", "Filter by worker (e.g. imip_send)")
	queue := flags.String("queue", "", "Filter by queue")
	recipient := flags.String("recipient", "", "Filter by recipient substring")
	failed := flags.Bool("failed", false, "Only failed/retried jobs (discarded, or available with a prior attempt)")
	limit := flags.Int("limit", 100, "Max rows")
	jsonOut := flags.Bool("json", false, "Output as JSON")
	dsn := dbFlag(flags)
	flags.Parse(args)

	rows, err := jobsOps(ctx, *dsn).List(jobs.ListFilter{
		State: *state, Worker: *worker, Queue: *queue,
		Recipient: *recipient, Failed: *failed, Limit: *limit,
	})
	if err != nil {
		fatalf("Failed to list jobs: %v", err)
	}
	if *jsonOut {
		printJSON(rows)
		return
	}
	if len(rows) == 0 {
		fmt.Println("(no jobs)")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tWORKER\tRECIPIENT\tATT\tDUE\tCRED\tERROR")
	for _, r := range rows {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d/%d\t%s\t%s\t%s\n",
			r.ID, r.State, r.Worker, dash(r.Recipient), r.Attempt, r.MaxAttempts,
			relTime(r.ScheduledAt), credCol(r.HasCred, r.KeyID), truncate(r.LastError, 40))
	}
	w.Flush()
}

func jobsShow(ctx context.Context, args []string) {
	id, rest := takeID(args)
	flags := flag.NewFlagSet("jobs show", flag.ExitOnError)
	jsonOut := flags.Bool("json", false, "Output as JSON")
	fullBody := flags.Bool("full-body", false, "Include the framed RFC822 body (attendee-visible event content)")
	dsn := dbFlag(flags)
	flags.Parse(rest)

	j, err := jobsOps(ctx, *dsn).Get(id)
	if err != nil {
		fatalf("Failed to load job %d: %v", id, err)
	}
	if j == nil {
		fatalf("No job with id %d.", id)
	}
	if *jsonOut {
		printJSON(j)
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	row := func(k, v string) { fmt.Fprintf(w, "%s\t%s\n", k, v) }
	row("id", fmt.Sprintf("%d", j.ID))
	row("queue", j.Queue)
	row("worker", j.Worker)
	row("state", j.State)
	row("attempt", fmt.Sprintf("%d/%d", j.Attempt, j.MaxAttempts))
	row("scheduled_at", j.ScheduledAt.Format(time.RFC3339)+" ("+relTime(j.ScheduledAt)+")")
	row("inserted_at", j.InsertedAt.Format(time.RFC3339))
	if j.CompletedAt != nil {
		row("completed_at", j.CompletedAt.Format(time.RFC3339))
	}
	row("credential", credDetail(j.HasCred, j.KeyID))
	if j.LastError != "" {
		row("last_error", j.LastError)
	}
	w.Flush()

	// args, pretty-printed. Never holds a secret (D8); message_b64 is gated behind
	// --full-body because it is attendee-visible content, not because it's secret.
	fmt.Println("\nargs:")
	printArgs(j.Args, *fullBody)
}

func jobsRetry(ctx context.Context, args []string) {
	id, rest := takeID(args)
	flags := flag.NewFlagSet("jobs retry", flag.ExitOnError)
	dsn := dbFlag(flags)
	flags.Parse(rest)

	if err := jobsOps(ctx, *dsn).Retry(id); err != nil {
		reportMutationErr("retry", id, err)
	}
	fmt.Printf("Job %d reset to run now (state=available, attempt cleared).\n", id)
}

func jobsCancel(ctx context.Context, args []string) {
	id, rest := takeID(args)
	flags := flag.NewFlagSet("jobs cancel", flag.ExitOnError)
	dsn := dbFlag(flags)
	flags.Parse(rest)

	if err := jobsOps(ctx, *dsn).Cancel(id); err != nil {
		reportMutationErr("cancel", id, err)
	}
	fmt.Printf("Job %d cancelled (state=discarded); any retained credential was dropped.\n", id)
}

func jobsReschedule(ctx context.Context, args []string) {
	id, rest := takeID(args)
	flags := flag.NewFlagSet("jobs reschedule", flag.ExitOnError)
	at := flags.String("at", "", "New run time (RFC3339, e.g. 2026-08-10T15:04:05Z), or a +duration (e.g. +30m)")
	dsn := dbFlag(flags)
	flags.Parse(rest)
	if *at == "" {
		fatalf("reschedule requires --at <RFC3339 timestamp | +duration>.")
	}
	when := parseAt(*at)

	if err := jobsOps(ctx, *dsn).Reschedule(id, when); err != nil {
		reportMutationErr("reschedule", id, err)
	}
	fmt.Printf("Job %d rescheduled for %s.\n", id, when.Format(time.RFC3339))
}

func jobsDelete(ctx context.Context, args []string) {
	id, rest := takeID(args)
	flags := flag.NewFlagSet("jobs delete", flag.ExitOnError)
	confirm := flags.Bool("confirm", false, "Actually delete the row (required — this is irreversible)")
	dsn := dbFlag(flags)
	flags.Parse(rest)
	if !*confirm {
		fatalf("This hard-deletes job %d (and any retained credential). Re-run with --confirm.", id)
	}
	if err := jobsOps(ctx, *dsn).Delete(id); err != nil {
		reportMutationErr("delete", id, err)
	}
	fmt.Printf("Job %d deleted.\n", id)
}

// --- iMIP wrappers (D5b) ---------------------------------------------------

// handleImip dispatches the iMIP-semantic recovery commands, which sit above the
// generic jobs layer.
func handleImip(ctx context.Context, args []string) {
	if len(args) == 0 {
		fatalf("Usage: calctl imip <pending|resend> [options]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "pending":
		imipPending(ctx, rest)
	case "resend":
		imipResend(ctx, rest)
	default:
		fatalf("Unknown imip subcommand: %s (want pending or resend)", sub)
	}
}

// imipPending surfaces the iMIP sends an operator cares about: failed or
// still-retrying jobs. A per-user job with no credential (cred=no) can only be
// recovered by the organizer re-sending from their client.
func imipPending(ctx context.Context, args []string) {
	flags := flag.NewFlagSet("imip pending", flag.ExitOnError)
	jsonOut := flags.Bool("json", false, "Output as JSON")
	limit := flags.Int("limit", 100, "Max rows")
	dsn := dbFlag(flags)
	flags.Parse(args)

	rows, err := jobsOps(ctx, *dsn).List(jobs.ListFilter{Worker: "imip_send", Failed: true, Limit: *limit})
	if err != nil {
		fatalf("Failed to list pending iMIP sends: %v", err)
	}
	if *jsonOut {
		printJSON(rows)
		return
	}
	if len(rows) == 0 {
		fmt.Println("(no failed or retrying iMIP sends)")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tRECIPIENT\tMETHOD\tATT\tCRED\tERROR")
	for _, r := range rows {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d/%d\t%s\t%s\n",
			r.ID, r.State, dash(r.Recipient), dash(r.Method),
			r.Attempt, r.MaxAttempts, yesNo(r.HasCred), truncate(r.LastError, 40))
	}
	w.Flush()
	fmt.Println("\nResend a metered send with `calctl imip resend <id>` (needs cred=yes).")
}

// imipResend is the domain wrapper over `jobs retry`: it re-runs a stuck iMIP
// send. For a per-user send (username set) it needs a still-decryptable
// credential — the worker would only fail-safe discard otherwise — so it refuses
// when none survives, pointing the operator at the organizer's own client.
func imipResend(ctx context.Context, args []string) {
	id, rest := takeID(args)
	flags := flag.NewFlagSet("imip resend", flag.ExitOnError)
	relay := flags.Bool("relay", false, "Override: deliver via the server relay, UNMETERED (drops the credential). For a per-user send whose credential is gone.")
	dsn := dbFlag(flags)
	flags.Parse(rest)

	ops := jobsOps(ctx, *dsn)
	j, err := ops.Get(id)
	if err != nil {
		fatalf("Failed to load job %d: %v", id, err)
	}
	if j == nil {
		fatalf("No job with id %d.", id)
	}
	if j.Worker != "imip_send" {
		fatalf("Job %d is worker %q, not imip_send. Use `calctl jobs retry`.", id, j.Worker)
	}

	if *relay {
		// The deliberate unmetered override: relay via the server identity. Logged
		// here and reflected in the job's outcome metric when the worker sends it.
		if err := ops.RelayOverride(id); err != nil {
			reportMutationErr("resend --relay", id, err)
		}
		fmt.Printf("Job %d re-queued to send via the server RELAY (UNMETERED — does not count against %s's limits).\n",
			id, dash(j.Username))
		return
	}

	// Metered path. A per-user send (username set) with no credential can't be
	// delivered as the user — the worker fail-safe discards. Metered recovery is the
	// organizer's own client re-issuing the invite (their live request carries the
	// password), or `--relay` to send it unmetered via the server.
	if j.Username != "" && !j.HasCred {
		fmt.Fprintf(os.Stderr,
			"Job %d is a per-user send whose credential is gone (purged, or the key rotated).\n"+
				"It can't be re-sent AS the user from here without bypassing metering. Either have %s\n"+
				"re-issue the invitation from their calendar client, or override with\n"+
				"`calctl imip resend %d --relay` to send it UNMETERED via the server.\n", id, j.Username, id)
		os.Exit(1)
	}
	if err := ops.Retry(id); err != nil {
		reportMutationErr("resend", id, err)
	}
	fmt.Printf("Job %d re-queued for delivery.\n", id)
}

// --- helpers ---------------------------------------------------------------

// takeID pulls the leading positional job id and returns it with the remaining
// (flag) args, matching the `calctl <verb> <id> [flags]` convention (the flag
// package stops parsing at a positional, so the id must come off first).
func takeID(args []string) (int64, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fatalf("A job id is required as the first argument.")
	}
	id, ok := parseJobID(args[0])
	if !ok {
		fatalf("Invalid job id %q (want a positive integer).", args[0])
	}
	return id, args[1:]
}

// parseJobID accepts a positive decimal id and nothing else. Not fmt.Sscan:
// it stops at the first non-digit and honours base prefixes, so "12abc" or
// "0x1f" would silently address some other job.
func parseJobID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0
}

// parseAt accepts an RFC3339 timestamp or a leading-'+' Go duration relative to now.
func parseAt(s string) time.Time {
	if strings.HasPrefix(s, "+") {
		d, err := time.ParseDuration(s[1:])
		if err != nil {
			fatalf("Invalid duration %q: %v", s, err)
		}
		return time.Now().Add(d)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		fatalf("Invalid --at %q (want RFC3339 or +duration): %v", s, err)
	}
	return t
}

func reportMutationErr(action string, id int64, err error) {
	switch {
	case errors.Is(err, jobs.ErrNoJob):
		fmt.Fprintf(os.Stderr, "No job with id %d.\n", id)
	case errors.Is(err, jobs.ErrJobCompleted):
		fmt.Fprintf(os.Stderr, "Job %d is already completed; %s would resend it. Delete it and re-enqueue via the app instead.\n", id, action)
	default:
		fmt.Fprintf(os.Stderr, "Failed to %s job %d: %v\n", action, id, err)
	}
	os.Exit(1)
}

// printArgs pretty-prints the job args, replacing the (large, non-secret) body
// with its size unless --full-body was passed.
func printArgs(raw json.RawMessage, fullBody bool) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		fmt.Println("  " + string(raw))
		return
	}
	if b, ok := m["message_b64"].(string); ok && !fullBody {
		m["message_b64"] = fmt.Sprintf("<%d bytes; --full-body to show>", len(b))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // read '<16 bytes...>' cleanly, not '<'
	enc.SetIndent("  ", "  ")
	enc.Encode(m)
	fmt.Print("  " + buf.String())
}

func credCol(has bool, keyID *int16) string {
	if !has {
		return "no"
	}
	if keyID != nil {
		return fmt.Sprintf("yes(k%d)", *keyID)
	}
	return "yes"
}

func credDetail(has bool, keyID *int16) string {
	if !has {
		return "none"
	}
	if keyID != nil {
		return fmt.Sprintf("present (key_id=%d), encrypted — never shown", *keyID)
	}
	return "present, encrypted — never shown"
}

func relTime(t time.Time) string {
	d := time.Until(t)
	if d >= 0 {
		if d < time.Minute {
			return "now"
		}
		return "in " + humanDur(d)
	}
	return humanDur(-d) + " ago"
}

func humanDur(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return fmt.Sprintf("%ds", d/time.Second)
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	// Count runes, not bytes: a byte cut can split a multi-byte character
	// and print invalid UTF-8 into the table.
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
