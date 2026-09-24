package main

// `calctl contacts` — the calserver half of the webmail contacts migration
// (docs/snappymail-calserver-contacts.md). The other half is
// scripts/snappymail-export-contacts.php in the ansible repo, which produces
// the bundle this reads.
//
// Every stage is a pure function of the bundle plus a fixed configuration, so a
// re-run is either a no-op or a repair, never a duplication. The decisions all
// live in storage.ContactImport; this file is the bundle reader, the worker
// pool and the report.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"calserver/internal/storage"
)

func handleContacts(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: calctl contacts <import|summary> [options]")
		os.Exit(1)
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "import":
		handleContactsImport(rest)
	case "summary":
		handleContactsSummary(rest)
	default:
		fmt.Fprintf(os.Stderr, "Unknown contacts subcommand: %s (want import or summary)\n", sub)
		os.Exit(1)
	}
}

// importOptions is the parsed command line, kept in one place so the worker
// pool and the per-principal call cannot disagree about a flag.
type importOptions struct {
	bundleDir      string
	only           stringList
	shards         stringList
	bookURI        string
	bookName       string
	workers        int
	limit          int
	resume         bool
	dryRun         bool
	forceOverwrite bool
	forceVersion   bool
	verifyShards   bool
	reportPath     string
	jsonOutput     bool
}

// stringList collects a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func handleContactsImport(args []string) {
	var opts importOptions
	flags := flag.NewFlagSet("contacts import", flag.ExitOnError)
	flags.StringVar(&opts.bundleDir, "bundle", "", "Path to the export bundle directory (required)")
	dsnOverride := flags.String("db", "", "Postgres DSN (default: [database] dsn)")
	flags.Var(&opts.only, "only", "Import just this principal; repeatable (the canary)")
	flags.Var(&opts.shards, "shard", "Import just this shard, e.g. 07f; repeatable")
	flags.StringVar(&opts.bookURI, "book-uri", "webmail", "Address book uri every migrated card lands in")
	flags.StringVar(&opts.bookName, "book-name", "Webmail contacts", "Address book display name")
	flags.IntVar(&opts.workers, "workers", 4, "Principals imported concurrently")
	flags.IntVar(&opts.limit, "limit", 0, "Stop after this many principals (0 = no limit)")
	flags.BoolVar(&opts.resume, "resume", true, "Skip a principal whose ledger row already matches the bundle; --resume=false re-verifies card by card instead")
	flags.BoolVar(&opts.dryRun, "dry-run", false, "Decide everything, write nothing, print the decision counts")
	flags.BoolVar(&opts.forceOverwrite, "force-overwrite", false, "Let the bundle win where a DAV client also changed the book since the last run (never deletes)")
	flags.BoolVar(&opts.forceVersion, "force-version", false, "Accept a bundle whose exporter version differs from the one the ledger recorded")
	flags.BoolVar(&opts.verifyShards, "verify-shards", false, "Re-checksum every shard file against the manifest before importing")
	flags.StringVar(&opts.reportPath, "report", "", "Write per-principal outcomes to this file as NDJSON")
	flags.BoolVar(&opts.jsonOutput, "json", false, "Print the run summary as JSON")
	flags.Parse(args)

	if opts.bundleDir == "" {
		fmt.Fprintln(os.Stderr, "A bundle is required: pass --bundle DIR.")
		os.Exit(1)
	}
	if opts.workers < 1 {
		opts.workers = 1
	}

	b, err := openBundle(opts.bundleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	db := openImportDB(*dsnOverride, opts.workers)
	defer db.Close()
	importer := &storage.ContactImport{DB: db}

	run, err := newImportRun(b, importer, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	defer run.close()

	if err := run.execute(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		run.print()
		os.Exit(1)
	}
	run.print()

	// Exit status: 1 when something went wrong (a principal's transaction
	// failed, a shard could not be read); 2 when nothing broke but the run did
	// not do what was asked — principals were refused, or an --only address
	// was not in the bundle. Both matter to a cutover script: a canary
	// re-import that visits nobody because of a typo must not look like
	// success, and neither must a bulk run that refused a thousand people.
	missing := run.unvisitedOnly()
	if len(missing) > 0 {
		fmt.Printf("\nNOT IN THE BUNDLE (%d) — check the spelling, and that the export selected them:\n", len(missing))
		for _, p := range missing {
			fmt.Printf("  %s\n", p)
		}
	}
	switch {
	case run.failed > 0 || len(run.shardErrors) > 0:
		os.Exit(1)
	case run.counts[storage.ActionRefused] > 0 || len(missing) > 0:
		os.Exit(2)
	}
}

// openImportDB takes its own pool rather than inheriting storage.DefaultPool,
// which is sized for a CLI doing one query. One connection per worker plus a
// little headroom, and never more than the default cap, so a parallel import
// cannot crowd out the live calserver on the shared Postgres.
func openImportDB(dsnOverride string, workers int) *sql.DB {
	dsn := dsnFor(dsnOverride)
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "A Postgres DSN is required: pass --db or set [database] dsn in the config file.")
		os.Exit(1)
	}
	maxOpen := min(workers+2, storage.DefaultPool.MaxOpen)
	db, err := storage.OpenPool(dsn, storage.Pool{MaxOpen: maxOpen, MaxIdle: workers})
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
	return db
}

// importRun holds one invocation's state: the shard list, the report file and
// the running tallies.
type importRun struct {
	bundle   *bundle
	importer *storage.ContactImport
	opts     importOptions
	runID    string
	shards   []string
	only     map[string]bool
	// onlySeen records which --only addresses were actually found in the
	// bundle, so a misspelled canary address is reported rather than quietly
	// importing nothing.
	onlySeen map[string]bool

	mu       sync.Mutex
	counts   map[storage.ImportAction]int
	cards    int
	written  int
	skipped  int
	orphans  int
	failed   int
	visited  int
	refusals []storage.ImportResult
	// shardErrors are shards that could not be read at all — a truncated
	// export, a damaged file, a sentinel that does not match its records. Every
	// principal in such a shard is simply not visited, so these have to reach
	// the exit status or the run looks clean while missing thousands of people.
	shardErrors map[string]error
	report      *os.File
	started     time.Time
}

func newImportRun(b *bundle, importer *storage.ContactImport, opts importOptions) (*importRun, error) {
	// The run id ties this invocation's ledger rows together. It is minted here
	// rather than taken from the manifest so two imports of the same bundle
	// (the canary, then the bulk run) stay distinguishable in contact_import.
	runID, err := storage.NewRunID()
	if err != nil {
		return nil, err
	}
	run := &importRun{
		bundle: b, importer: importer, opts: opts, runID: runID,
		counts: map[storage.ImportAction]int{}, shardErrors: map[string]error{},
		started: time.Now(),
	}
	if len(opts.only) > 0 {
		run.only = map[string]bool{}
		run.onlySeen = map[string]bool{}
		for _, p := range opts.only {
			run.only[strings.ToLower(strings.TrimSpace(p))] = true
		}
	}
	switch {
	case len(opts.shards) > 0:
		run.shards = opts.shards
	case run.only != nil:
		// A principal's records live in exactly one shard, and the shard is a
		// function of its address — so a canary import can open one 6 MB file
		// instead of reading the whole 1.5 GB bundle to find one mailbox. The
		// doc has the canary re-imported right before its plugin is flipped, so
		// this is the difference between that step taking a second and a minute.
		run.shards = shardsFor(run.only)
	default:
		if run.shards, err = b.shardNames(); err != nil {
			return nil, err
		}
	}
	if opts.reportPath != "" {
		f, err := os.Create(opts.reportPath)
		if err != nil {
			return nil, fmt.Errorf("opening report: %w", err)
		}
		run.report = f
	}
	return run, nil
}

// shardsFor returns the shards the given principals hash into, matching the
// exporter's shardFor(): the first byte of sha256(principal), three decimal
// digits. Keep the two in step — a mismatch here does not corrupt anything, it
// just makes --only silently import nothing.
func shardsFor(principals map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for principal := range principals {
		sum := sha256.Sum256([]byte(principal))
		name := fmt.Sprintf("%03d", sum[0])
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func (r *importRun) close() {
	if r.report != nil {
		r.report.Close()
	}
}

// execute walks the shards, one worker per shard. A shard owns whole
// principals, so workers never touch the same principal — and the advisory
// lock in ImportPrincipal makes that safe even if they did.
func (r *importRun) execute(ctx context.Context) error {
	if r.opts.verifyShards {
		// Both kinds: a damaged frecency shard is less serious than a damaged
		// cards shard, but "re-checksum every shard file" has to mean every one
		// or the flag is misleading.
		for _, shard := range r.shards {
			for _, kind := range []string{"cards", "frecency"} {
				if err := r.bundle.verifyShard(kind, shard); err != nil {
					if kind == "frecency" && errors.Is(err, os.ErrNotExist) {
						continue // a principal with no frecency rows has no shard
					}
					return err
				}
			}
		}
	}

	work := make(chan string)
	var wg sync.WaitGroup
	for range r.opts.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A worker never leaves this loop early. An unreadable shard is
			// recorded and the next one is picked up, for two reasons: 255 good
			// shards should not be abandoned because one is damaged, and a
			// worker that returns while the producer is still sending leaves the
			// producer blocked on an unbuffered channel with no receiver — which
			// is a batch job that hangs instead of failing, the worst outcome
			// available.
			for shard := range work {
				if err := r.importShard(ctx, shard); err != nil {
					r.recordShardError(shard, err)
				}
			}
		}()
	}
	for _, shard := range r.shards {
		if r.reachedLimit() {
			break
		}
		work <- shard
	}
	close(work)
	wg.Wait()
	return nil
}

func (r *importRun) reachedLimit() bool {
	if r.opts.limit <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.visited >= r.opts.limit
}

func (r *importRun) importShard(ctx context.Context, shard string) error {
	frecency, err := r.bundle.readFrecencyShard(shard)
	if err != nil {
		return err
	}
	return r.bundle.scanCardShard(shard, func(pr principalRecords) error {
		if r.only != nil {
			if !r.only[pr.Principal] {
				return nil
			}
			r.markOnlySeen(pr.Principal)
		}
		if r.reachedLimit() {
			return nil
		}
		req := storage.ImportRequest{
			RunID:          r.runID,
			Principal:      pr.Principal,
			SourceUsers:    pr.Users,
			BookURI:        r.opts.bookURI,
			BookName:       r.opts.bookName,
			BookDesc:       r.bookDescription(),
			Digest:         pr.Digest,
			ExporterVer:    r.bundle.manifest.ExporterVersion,
			ExportEpoch:    r.bundle.manifest.ExportEpoch,
			Cards:          toImportCards(pr.Cards),
			Frecency:       toSeedFrecency(frecency[pr.Principal]),
			DryRun:         r.opts.dryRun,
			ForceOverwrite: r.opts.forceOverwrite,
			ForceVersion:   r.opts.forceVersion,
			Reverify:       !r.opts.resume,
		}
		res, err := r.importer.ImportPrincipal(ctx, req)
		if err != nil {
			// One principal's failure is not the run's: record it and carry on,
			// since its transaction rolled back whole and a later run will pick
			// it up unchanged.
			res = storage.ImportResult{
				Principal: pr.Principal, Action: storage.ImportAction("error"),
				Reason: err.Error(), CardsTotal: len(pr.Cards),
			}
			r.record(res, true)
			return nil
		}
		r.record(res, false)
		return nil
	})
}

func (r *importRun) bookDescription() string {
	if r.bundle.manifest.ExportEpoch == 0 {
		return "Contacts migrated from webmail"
	}
	return "Contacts migrated from webmail on " +
		time.Unix(r.bundle.manifest.ExportEpoch, 0).UTC().Format("2006-01-02")
}

func toImportCards(recs []cardRecord) []storage.ImportCard {
	out := make([]storage.ImportCard, 0, len(recs))
	for _, rec := range recs {
		out = append(out, storage.ImportCard{
			URI: rec.URI, Data: []byte(rec.Card), LastModified: rec.MTime,
		})
	}
	return out
}

func toSeedFrecency(recs []frecencyRecord) []storage.SeedFrecency {
	if len(recs) == 0 {
		return nil
	}
	out := make([]storage.SeedFrecency, 0, len(recs))
	for _, rec := range recs {
		out = append(out, storage.SeedFrecency{Email: rec.Email, Name: rec.Name, Count: rec.Frec})
	}
	return out
}

func (r *importRun) markOnlySeen(principal string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onlySeen[principal] = true
}

// unvisitedOnly lists the --only addresses no shard contained, sorted. Empty
// when --only was not given.
func (r *importRun) unvisitedOnly() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for p := range r.only {
		if !r.onlySeen[p] {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func (r *importRun) recordShardError(shard string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shardErrors[shard] = err
}

func (r *importRun) record(res storage.ImportResult, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.visited++
	r.counts[res.Action]++
	r.cards += res.CardsTotal
	r.written += res.CardsCreated + res.CardsUpdated
	r.skipped += res.CardsSkipped
	r.orphans += len(res.Orphans)
	if failed {
		r.failed++
	}
	if res.Action == storage.ActionRefused || failed {
		if len(r.refusals) < 50 {
			r.refusals = append(r.refusals, res)
		}
	}
	if r.report != nil {
		line, err := json.Marshal(res)
		if err == nil {
			r.report.Write(append(line, '\n'))
		}
	}
}

func (r *importRun) print() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.opts.jsonOutput {
		out := map[string]any{
			"runId": r.runID, "dryRun": r.opts.dryRun,
			"principals": r.visited, "cards": r.cards,
			"cardsWritten": r.written, "cardsSkipped": r.skipped,
			"orphans": r.orphans, "errors": r.failed,
			"elapsedSeconds":   time.Since(r.started).Seconds(),
			"actions":          actionCounts(r.counts),
			"unreadableShards": shardErrorStrings(r.shardErrors),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if r.opts.dryRun {
		fmt.Fprintln(w, "DRY RUN — nothing was written\t")
	}
	fmt.Fprintf(w, "run id\t%s\n", r.runID)
	fmt.Fprintf(w, "principals\t%d\n", r.visited)
	for _, action := range sortedActions(r.counts) {
		fmt.Fprintf(w, "  %s\t%d\n", action, r.counts[storage.ImportAction(action)])
	}
	fmt.Fprintf(w, "cards in bundle\t%d\n", r.cards)
	fmt.Fprintf(w, "cards written\t%d\n", r.written)
	fmt.Fprintf(w, "cards unchanged\t%d\n", r.skipped)
	fmt.Fprintf(w, "cards present here but not in the bundle\t%d\n", r.orphans)
	fmt.Fprintf(w, "elapsed\t%s\n", time.Since(r.started).Round(time.Millisecond))
	w.Flush()

	if len(r.shardErrors) > 0 {
		fmt.Printf("\nUNREADABLE SHARDS (%d) — every principal in them was skipped:\n", len(r.shardErrors))
		for _, shard := range sortedKeys(r.shardErrors) {
			fmt.Printf("  %s: %v\n", shard, r.shardErrors[shard])
		}
	}
	if len(r.refusals) > 0 {
		fmt.Printf("\nRefused (%d shown):\n", len(r.refusals))
		for _, res := range r.refusals {
			fmt.Printf("  %-40s %s\n", res.Principal, res.Reason)
		}
	}
	if r.orphans > 0 && r.opts.reportPath == "" {
		fmt.Println("\nSome cards exist here but not in the bundle. The importer never " +
			"deletes; re-run with --report FILE to list them per principal.")
	}
}

func shardErrorStrings(errs map[string]error) map[string]string {
	out := map[string]string{}
	for shard, err := range errs {
		out[shard] = err.Error()
	}
	return out
}

func sortedKeys(errs map[string]error) []string {
	out := make([]string, 0, len(errs))
	for shard := range errs {
		out = append(out, shard)
	}
	sort.Strings(out)
	return out
}

func actionCounts(counts map[storage.ImportAction]int) map[string]int {
	out := map[string]int{}
	for action, n := range counts {
		out[string(action)] = n
	}
	return out
}

func sortedActions(counts map[storage.ImportAction]int) []string {
	out := make([]string, 0, len(counts))
	for action := range counts {
		out = append(out, string(action))
	}
	sort.Strings(out)
	return out
}

// handleContactsSummary reads a past run back out of the ledger, and runs the
// one consistency check nothing else will do for you: an address book whose
// principal does not exist is silent, because addressbooks.principaluri has no
// foreign key — webmail serves it correctly while DAV discovery 404s and the
// user sees nothing on their phone.
func handleContactsSummary(args []string) {
	flags := flag.NewFlagSet("contacts summary", flag.ExitOnError)
	runID := flags.String("run-id", "", "Restrict the totals to one run")
	dsnOverride := flags.String("db", "", "Postgres DSN (default: [database] dsn)")
	jsonOutput := flags.Bool("json", false, "Output as JSON")
	flags.Parse(args)

	db := openImportDB(*dsnOverride, 1)
	defer db.Close()
	importer := &storage.ContactImport{DB: db}
	ctx := context.Background()

	summary, err := importer.Summary(ctx, *runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read the ledger: %v\n", err)
		os.Exit(1)
	}
	orphans, err := importer.OrphanBooks(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to check for orphaned address books: %v\n", err)
		os.Exit(1)
	}

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{"summary": summary, "orphanedAddressBooks": orphans})
	} else {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		if *runID != "" {
			fmt.Fprintf(w, "run id\t%s\n", summary.RunID)
		}
		fmt.Fprintf(w, "principals imported\t%d\n", summary.Principals)
		fmt.Fprintf(w, "cards\t%d\n", summary.Cards)
		fmt.Fprintf(w, "cards written\t%d\n", summary.Written)
		fmt.Fprintf(w, "cards unchanged\t%d\n", summary.Skipped)
		fmt.Fprintf(w, "address books with no principal\t%d\n", len(orphans))
		w.Flush()
		for _, uri := range orphans {
			fmt.Printf("  orphan: %s\n", uri)
		}
	}
	if len(orphans) > 0 {
		os.Exit(1)
	}
}
