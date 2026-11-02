package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/admin"
	"github.com/migadu/calendsd/internal/auth"
	"github.com/migadu/calendsd/internal/config"
	"github.com/migadu/calendsd/internal/server"
	"github.com/migadu/calendsd/internal/storage"
)

// handlePrincipal dispatches the principal subcommands. Reads go through the
// same backends the server uses; writes go through the storage.AdminWrites
// carve-out, exactly like the admin panel's Phase C endpoints.
func handlePrincipal(ctx context.Context, args []string) {
	if len(args) == 0 {
		fatalf("Usage: calctl principal <list|add|edit|remove|auth> [options]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		handlePrincipalList(ctx, rest)
	case "add":
		handlePrincipalAdd(ctx, rest)
	case "edit":
		handlePrincipalEdit(ctx, rest)
	case "remove":
		handlePrincipalRemove(ctx, rest)
	case "delete":
		handlePrincipalDelete(ctx, rest)
	case "restore":
		handlePrincipalRestore(ctx, rest)
	case "purge-expired":
		handlePrincipalPurgeExpired(ctx, rest)
	case "auth":
		handlePrincipalAuth(ctx, rest)
	default:
		fatalf("Unknown principal subcommand: %s (want list, add, edit, remove, delete, restore, purge-expired, or auth)", sub)
	}
}

// handlePrincipalAuth is the offline credential check the admin panel
// carries for §3.2: digesta1 is an opaque md5, so a realm mismatch shows up
// only by recomputing the digest under the configured realm with a known
// password and comparing. Exit 0 on a match, 1 otherwise.
func handlePrincipalAuth(ctx context.Context, args []string) {
	username, rest := takeUsername("auth", args)
	flags := flag.NewFlagSet("principal auth", flag.ExitOnError)
	jsonOutput := flags.Bool("json", false, "Output as JSON")
	password := flags.String("password", "", "Password to verify (or use --password-stdin)")
	passwordStdin := flags.Bool("password-stdin", false, "Read the password from the first line of stdin")
	realm := flags.String("realm", realmDefault(), "Auth realm to compute the digest under; must match the server's")
	dsnOverride := dbFlag(flags)
	flags.Parse(rest)

	pw := resolvePassword(*password, *passwordStdin)
	if pw == "" {
		fatalf("A password is required: pass --password or --password-stdin.")
	}
	q := openAdminQueries(ctx, *dsnOverride)
	user, err := q.UserForPrincipal(ctx, "principals/"+username)
	if err != nil {
		fatalf("Failed to look up user: %v", err)
	}

	res := admin.CheckCredentials(user, username, pw, *realm)
	if *jsonOutput {
		printJSON(res)
	} else {
		fmt.Println(res.Note)
	}
	if !res.Match {
		os.Exit(1)
	}
}

func handlePrincipalList(ctx context.Context, args []string) {
	flags := flag.NewFlagSet("principal list", flag.ExitOnError)
	jsonOutput := flags.Bool("json", false, "Output as JSON")
	limit := flags.Int("limit", acl.DefaultPageSize, "Principals per page")
	after := flags.String("after", "", "List the principals after this URI (the cursor the previous page printed)")
	all := flags.Bool("all", false, "Stream every principal, page by page, instead of one page")
	search := flags.String("search", "", "Only principals whose name or email starts with this")
	dsnOverride := dbFlag(flags)
	flags.Parse(args)

	q := openAdminQueries(ctx, *dsnOverride)
	diags, err := q.AuthDiagnostics(ctx)
	if err != nil {
		fatalf("Failed to cross-check users: %v", err)
	}
	deletions, err := (&storage.AdminWrites{DB: q.DB}).Deletions(ctx)
	if err != nil {
		fatalf("Failed to list pending deletions: %v", err)
	}
	purgeDate := map[string]string{}
	for _, d := range deletions {
		purgeDate[d.Principal] = d.PurgeAfter.Format("2006-01-02")
	}

	var jsonRows []admin.PrincipalRow
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if !*jsonOutput {
		fmt.Fprintln(w, "URI\tDISPLAYNAME\tEMAIL\tUSER\tDELETION")
	}
	next, err := forEachPrincipalPage(ctx, &storage.Principals{DB: q.DB}, *search, *after, *limit, *all,
		func(principals []acl.Principal) {
			rows := admin.PrincipalRows(principals, diags)
			for i := range rows {
				rows[i].Deletion = purgeDate[rows[i].URI]
			}
			if *jsonOutput {
				jsonRows = append(jsonRows, rows...)
				return
			}
			for _, r := range rows {
				user := "yes"
				if !r.HasUser {
					user = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.URI, dash(r.DisplayName), dash(r.Email), user, dash(r.Deletion))
			}
			w.Flush() // a page at a time, so --all streams
		})
	if err != nil {
		fatalf("Failed to list principals: %v", err)
	}
	if *jsonOutput {
		if jsonRows == nil {
			jsonRows = []admin.PrincipalRow{}
		}
		printJSON(jsonRows)
	}
	if next != "" {
		fmt.Fprintf(os.Stderr, "more principals follow: --after %q (or --all)\n", next)
	}
}

// forEachPrincipalPage lists principals in pages of limit, in URI order,
// starting after the cursor, and hands each page to emit: one page, or with
// all every page until the listing is exhausted. It returns the cursor of
// the page that would follow, "" when none does.
func forEachPrincipalPage(ctx context.Context, backend acl.Backend, search, after string, limit int, all bool, emit func([]acl.Principal)) (next string, err error) {
	for {
		page, err := acl.PagePrincipals(ctx, backend, "principals",
			acl.PageRequest{Search: search, After: after, Limit: limit})
		if err != nil {
			return "", err
		}
		if len(page) > 0 {
			emit(page)
		}
		if len(page) < limit {
			return "", nil
		}
		after = page[len(page)-1].URI
		if !all {
			return after, nil
		}
	}
}

func handlePrincipalAdd(ctx context.Context, args []string) {
	username, rest := takeUsername("add", args)
	flags := flag.NewFlagSet("principal add", flag.ExitOnError)
	email := flags.String("email", "", "Email address")
	name := flags.String("name", "", "Display name")
	password := flags.String("password", "", "Password (or use --password-stdin)")
	passwordStdin := flags.Bool("password-stdin", false, "Read the password from the first line of stdin")
	realm := flags.String("realm", realmDefault(), "Auth realm baked into the digesta1 hash; must match the server's")
	dsnOverride := dbFlag(flags)
	flags.Parse(rest)

	pw := resolvePassword(*password, *passwordStdin)
	if pw == "" {
		fatalf("A password is required: pass --password or --password-stdin.")
	}
	q := openAdminQueries(ctx, *dsnOverride)
	writes := &storage.AdminWrites{DB: q.DB}
	if err := writes.CreateUser(ctx, username, auth.DigestA1(username, *realm, pw), *email, *name); err != nil {
		fatalf("Failed to create user: %v", err)
	}
	fmt.Printf("Created user %s and principals/%s (with proxy sub-principals) under realm %s.\n",
		username, username, *realm)
}

func handlePrincipalEdit(ctx context.Context, args []string) {
	username, rest := takeUsername("edit", args)
	flags := flag.NewFlagSet("principal edit", flag.ExitOnError)
	email := flags.String("email", "", "New email address (empty string clears it)")
	name := flags.String("name", "", "New display name (empty string clears it)")
	password := flags.String("password", "", "New password (or use --password-stdin)")
	passwordStdin := flags.Bool("password-stdin", false, "Read the new password from the first line of stdin")
	realm := flags.String("realm", realmDefault(), "Auth realm baked into the digesta1 hash; must match the server's")
	dsnOverride := dbFlag(flags)
	flags.Parse(rest)

	// Only flags given explicitly change anything, so an omitted --email is
	// "leave alone" while --email "" is "clear".
	var newEmail, newName *string
	setPassword := *passwordStdin
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "email":
			newEmail = email
		case "name":
			newName = name
		case "password":
			setPassword = true
		}
	})
	if newEmail == nil && newName == nil && !setPassword {
		fatalf("Nothing to change: pass --email, --name, --password, or --password-stdin.")
	}

	q := openAdminQueries(ctx, *dsnOverride)
	writes := &storage.AdminWrites{DB: q.DB}
	if newEmail != nil || newName != nil {
		if err := writes.UpdatePrincipal(ctx, "principals/"+username, newEmail, newName); err != nil {
			fatalf("Failed to update principal: %v", err)
		}
		fmt.Printf("Updated principals/%s.\n", username)
	}
	if setPassword {
		pw := resolvePassword(*password, *passwordStdin)
		if pw == "" {
			fatalf("A password is required with --password/--password-stdin.")
		}
		if err := writes.SetPassword(ctx, username, auth.DigestA1(username, *realm, pw)); err != nil {
			fatalf("Failed to set password: %v", err)
		}
		fmt.Printf("Password reset for %s under realm %s.\n", username, *realm)
	}
}

func handlePrincipalRemove(ctx context.Context, args []string) {
	username, rest := takeUsername("remove", args)
	flags := flag.NewFlagSet("principal remove", flag.ExitOnError)
	confirm := flags.Bool("confirm", false, "Actually delete (required)")
	cascade := flags.Bool("cascade", false, "Also delete the user's owned calendars and address books (otherwise removal refuses while any collection remains)")
	dsnOverride := dbFlag(flags)
	flags.Parse(rest)

	if !*confirm {
		what := "the users row and principal triple"
		if *cascade {
			what = "all owned calendars and address books, plus the users row and principal triple"
		}
		fatalf("This deletes %s for %q. Re-run with --confirm.", what, username)
	}
	writes := &storage.AdminWrites{DB: openAdminQueries(ctx, *dsnOverride).DB}
	remove := writes.DeleteUser
	if *cascade {
		remove = writes.DeleteUserCascade
	}
	if err := remove(ctx, username); err != nil {
		fatalf("Failed to remove: %v", err)
	}
	if *cascade {
		fmt.Printf("Removed user %s, principals/%s (with proxy sub-principals), and all owned calendars/address books.\n",
			username, username)
	} else {
		fmt.Printf("Removed user %s, principals/%s (with proxy sub-principals), and their group-membership and scheduling rows.\n",
			username, username)
	}
	fmt.Println("Run `calctl doctor` to spot any leftover property rows.")
}

// handlePrincipalDelete schedules a soft-delete: the account can no longer log
// in, but its data is kept until the retention window elapses and
// `purge-expired` removes it. Restore with `principal restore` meanwhile.
func handlePrincipalDelete(ctx context.Context, args []string) {
	username, rest := takeUsername("delete", args)
	flags := flag.NewFlagSet("principal delete", flag.ExitOnError)
	in := flags.Int("in", 0, "Days to keep the account before purge (default: [retention].deleted_days, then 30)")
	reason := flags.String("reason", "", "Optional note stored with the tombstone")
	confirm := flags.Bool("confirm", false, "Actually schedule the deletion (required)")
	dsnOverride := dbFlag(flags)
	flags.Parse(rest)

	days := config.RetentionDays(*in)
	if *in == 0 {
		days = config.RetentionDays(globalConfig.Retention.DeletedDays)
	}
	if !*confirm {
		fmt.Fprintf(os.Stderr, "This schedules %q for deletion in %d day(s); it cannot log in meanwhile. Re-run with --confirm (restore with `calctl principal restore %s`).\n",
			username, days, username)
		os.Exit(1)
	}
	q := openAdminQueries(ctx, *dsnOverride)
	if err := (&storage.AdminWrites{DB: q.DB}).ScheduleDelete(ctx, username, days, *reason); err != nil {
		fatalf("Failed to schedule deletion: %v", err)
	}
	fmt.Printf("Scheduled %s for deletion in %d day(s). It can no longer log in; restore with `calctl principal restore %s`.\n",
		username, days, username)
}

// handlePrincipalRestore cancels a pending soft-delete.
func handlePrincipalRestore(ctx context.Context, args []string) {
	username, rest := takeUsername("restore", args)
	flags := flag.NewFlagSet("principal restore", flag.ExitOnError)
	dsnOverride := dbFlag(flags)
	flags.Parse(rest)

	q := openAdminQueries(ctx, *dsnOverride)
	if err := (&storage.AdminWrites{DB: q.DB}).CancelDelete(ctx, username); err != nil {
		fatalf("Failed to restore: %v", err)
	}
	fmt.Printf("Restored %s; the account can log in again.\n", username)
}

// handlePrincipalPurgeExpired runs the retention sweep — the command cron runs.
// --dry-run lists what would be purged without touching anything.
func handlePrincipalPurgeExpired(ctx context.Context, args []string) {
	flags := flag.NewFlagSet("principal purge-expired", flag.ExitOnError)
	dryRun := flags.Bool("dry-run", false, "List accounts due for purge without deleting them")
	dsnOverride := dbFlag(flags)
	flags.Parse(args)

	writes := &storage.AdminWrites{DB: openAdminQueries(ctx, *dsnOverride).DB}
	if *dryRun {
		due, err := writes.ExpiredDeletions(ctx)
		if err != nil {
			fatalf("Failed to list expired deletions: %v", err)
		}
		for _, d := range due {
			fmt.Printf("would purge %s (scheduled %s, due %s)\n",
				d.Username, d.DeletedAt.Format("2006-01-02"), d.PurgeAfter.Format("2006-01-02"))
		}
		fmt.Printf("%d account(s) due for purge.\n", len(due))
		return
	}
	purged, err := writes.PurgeExpired(ctx)
	if err != nil {
		fatalf("Purge failed after removing %d account(s): %v", len(purged), err)
	}
	for _, d := range purged {
		fmt.Printf("purged %s\n", d.Username)
	}
	fmt.Printf("Purged %d expired account(s).\n", len(purged))
}

// takeUsername enforces the positional-first convention (flag parsing stops
// at the first non-flag argument, so the username must come right after the
// subcommand).
func takeUsername(sub string, args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fatalf("Usage: calctl principal %s <username> [options]", sub)
	}
	return args[0], args[1:]
}

// realmDefault mirrors how the server binaries fall back when the config
// file leaves the realm unset.
func realmDefault() string {
	if globalConfig.Server.Realm != "" {
		return globalConfig.Server.Realm
	}
	return server.DefaultRealm
}

func resolvePassword(flagValue string, fromStdin bool) string {
	if fromStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			fatalf("Failed to read password from stdin: %v", err)
		}
		return strings.TrimRight(line, "\r\n")
	}
	if flagValue != "" {
		// Kept for compatibility, but an argv password is visible to every
		// process on the host (ps) and lands in the shell history (audit 6.11).
		fmt.Fprintln(os.Stderr, "Warning: --password exposes the password to other processes and the shell history; prefer --password-stdin.")
	}
	return flagValue
}
