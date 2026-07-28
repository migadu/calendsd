package main

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"calserver/internal/auth"
	davxml "calserver/internal/dav/xml"
	"calserver/internal/storage"
)

// handlePrincipal dispatches the principal subcommands. Reads go through the
// same backends the server uses; writes go through the storage.AdminWrites
// carve-out, exactly like the admin panel's Phase C endpoints.
func handlePrincipal(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: calctl principal <list|add|edit|remove|auth> [options]")
		os.Exit(1)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		handlePrincipalList(rest)
	case "add":
		handlePrincipalAdd(rest)
	case "edit":
		handlePrincipalEdit(rest)
	case "remove":
		handlePrincipalRemove(rest)
	case "auth":
		handlePrincipalAuth(rest)
	default:
		fmt.Fprintf(os.Stderr, "Unknown principal subcommand: %s (want list, add, edit, remove, or auth)\n", sub)
		os.Exit(1)
	}
}

// authOutcome is the JSON shape of `principal auth` (named after rcptctl's
// auth command), mirroring the admin panel's CheckCredentialsResult (§3.2).
type authOutcome struct {
	Found            bool   `json:"found"`
	DigestWellFormed bool   `json:"digestWellFormed"`
	Match            bool   `json:"match"`
	Note             string `json:"note"`
}

// handlePrincipalAuth is the offline credential check the admin panel
// carries for §3.2: digesta1 is an opaque md5, so a realm mismatch shows up
// only by recomputing the digest under the configured realm with a known
// password and comparing. Exit 0 on a match, 1 otherwise.
func handlePrincipalAuth(args []string) {
	username, rest := takeUsername("auth", args)
	flags := flag.NewFlagSet("principal auth", flag.ExitOnError)
	jsonOutput := flags.Bool("json", false, "Output as JSON")
	password := flags.String("password", "", "Password to verify (or use --password-stdin)")
	passwordStdin := flags.Bool("password-stdin", false, "Read the password from the first line of stdin")
	realm := flags.String("realm", realmDefault(), "Auth realm to compute the digest under; must match the server's")
	dsnOverride := flags.String("db", "", "Postgres DSN (default: [database] dsn)")
	flags.Parse(rest)

	pw := resolvePassword(*password, *passwordStdin)
	if pw == "" {
		fmt.Fprintln(os.Stderr, "A password is required: pass --password or --password-stdin.")
		os.Exit(1)
	}
	q := openAdminQueries(*dsnOverride)
	user, err := q.UserForPrincipal("principals/" + username)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to look up user: %v\n", err)
		os.Exit(1)
	}

	res := authOutcome{}
	switch {
	case user == nil:
		res.Note = "no users row — this account cannot log in"
	case !wellFormedDigest(user.DigestA1):
		res.Found = true
		res.Note = "digesta1 is not a 32-char md5 hex — auth always fails"
	default:
		res.Found, res.DigestWellFormed = true, true
		res.Match = subtle.ConstantTimeCompare(
			[]byte(auth.DigestA1(username, *realm, pw)), []byte(user.DigestA1)) == 1
		if res.Match {
			res.Note = "credentials valid under realm " + *realm
		} else {
			res.Note = "no match under realm " + *realm +
				" — wrong password, or the hash was minted under a different realm"
		}
	}

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(res)
	} else {
		fmt.Println(res.Note)
	}
	if !res.Match {
		os.Exit(1)
	}
}

// wellFormedDigest matches the admin panel's check: 32 lowercase hex chars.
func wellFormedDigest(digest string) bool {
	if len(digest) != 32 {
		return false
	}
	for _, c := range digest {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// principalRow is the list/JSON shape: the principals columns plus whether a
// users row exists (groups and resources have none and cannot log in).
type principalRow struct {
	URI         string `json:"uri"`
	DisplayName string `json:"displayName,omitempty"`
	Email       string `json:"email,omitempty"`
	HasUser     bool   `json:"hasUser"`
}

func handlePrincipalList(args []string) {
	flags := flag.NewFlagSet("principal list", flag.ExitOnError)
	jsonOutput := flags.Bool("json", false, "Output as JSON")
	dsnOverride := flags.String("db", "", "Postgres DSN (default: [database] dsn)")
	flags.Parse(args)

	q := openAdminQueries(*dsnOverride)
	principals, err := (&storage.Principals{DB: q.DB}).PrincipalsByPrefix("principals")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list principals: %v\n", err)
		os.Exit(1)
	}
	diags, err := q.AuthDiagnostics()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to cross-check users: %v\n", err)
		os.Exit(1)
	}
	noUser := map[string]bool{}
	for _, p := range diags.PrincipalsWithoutUser {
		noUser[p] = true
	}

	rows := make([]principalRow, 0, len(principals))
	for _, p := range principals {
		row := principalRow{URI: p.URI, HasUser: !noUser[p.URI]}
		if v, ok := p.Props[xml.Name{Space: davxml.NSDav, Local: "displayname"}].(string); ok {
			row.DisplayName = v
		}
		if v, ok := p.Props[xml.Name{Space: davxml.NSSabreDAV, Local: "email-address"}].(string); ok {
			row.Email = v
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].URI < rows[j].URI })

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(rows)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "URI\tDISPLAYNAME\tEMAIL\tUSER")
	for _, r := range rows {
		user := "yes"
		if !r.HasUser {
			user = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.URI, dash(r.DisplayName), dash(r.Email), user)
	}
	w.Flush()
}

func handlePrincipalAdd(args []string) {
	username, rest := takeUsername("add", args)
	flags := flag.NewFlagSet("principal add", flag.ExitOnError)
	email := flags.String("email", "", "Email address")
	name := flags.String("name", "", "Display name")
	password := flags.String("password", "", "Password (or use --password-stdin)")
	passwordStdin := flags.Bool("password-stdin", false, "Read the password from the first line of stdin")
	realm := flags.String("realm", realmDefault(), "Auth realm baked into the digesta1 hash; must match the server's")
	dsnOverride := flags.String("db", "", "Postgres DSN (default: [database] dsn)")
	flags.Parse(rest)

	pw := resolvePassword(*password, *passwordStdin)
	if pw == "" {
		fmt.Fprintln(os.Stderr, "A password is required: pass --password or --password-stdin.")
		os.Exit(1)
	}
	q := openAdminQueries(*dsnOverride)
	writes := &storage.AdminWrites{DB: q.DB}
	if err := writes.CreateUser(username, auth.DigestA1(username, *realm, pw), *email, *name); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create user: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Created user %s and principals/%s (with proxy sub-principals) under realm %s.\n",
		username, username, *realm)
}

func handlePrincipalEdit(args []string) {
	username, rest := takeUsername("edit", args)
	flags := flag.NewFlagSet("principal edit", flag.ExitOnError)
	email := flags.String("email", "", "New email address (empty string clears it)")
	name := flags.String("name", "", "New display name (empty string clears it)")
	password := flags.String("password", "", "New password (or use --password-stdin)")
	passwordStdin := flags.Bool("password-stdin", false, "Read the new password from the first line of stdin")
	realm := flags.String("realm", realmDefault(), "Auth realm baked into the digesta1 hash; must match the server's")
	dsnOverride := flags.String("db", "", "Postgres DSN (default: [database] dsn)")
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
		fmt.Fprintln(os.Stderr, "Nothing to change: pass --email, --name, --password, or --password-stdin.")
		os.Exit(1)
	}

	q := openAdminQueries(*dsnOverride)
	writes := &storage.AdminWrites{DB: q.DB}
	if newEmail != nil || newName != nil {
		if err := writes.UpdatePrincipal("principals/"+username, newEmail, newName); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to update principal: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Updated principals/%s.\n", username)
	}
	if setPassword {
		pw := resolvePassword(*password, *passwordStdin)
		if pw == "" {
			fmt.Fprintln(os.Stderr, "A password is required with --password/--password-stdin.")
			os.Exit(1)
		}
		if err := writes.SetPassword(username, auth.DigestA1(username, *realm, pw)); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to set password: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Password reset for %s under realm %s.\n", username, *realm)
	}
}

func handlePrincipalRemove(args []string) {
	username, rest := takeUsername("remove", args)
	flags := flag.NewFlagSet("principal remove", flag.ExitOnError)
	confirm := flags.Bool("confirm", false, "Actually delete (required)")
	dsnOverride := flags.String("db", "", "Postgres DSN (default: [database] dsn)")
	flags.Parse(rest)

	if !*confirm {
		fmt.Fprintf(os.Stderr, "This deletes the users row and principal triple for %q. Re-run with --confirm.\n", username)
		os.Exit(1)
	}
	q := openAdminQueries(*dsnOverride)
	if err := (&storage.AdminWrites{DB: q.DB}).DeleteUser(username); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to remove: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Removed user %s, principals/%s (with proxy sub-principals), and their group-membership and scheduling rows.\n",
		username, username)
	fmt.Println("Run `calctl doctor` to spot any leftover property rows.")
}

// takeUsername enforces the positional-first convention (flag parsing stops
// at the first non-flag argument, so the username must come right after the
// subcommand).
func takeUsername(sub string, args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(os.Stderr, "Usage: calctl principal %s <username> [options]\n", sub)
		os.Exit(1)
	}
	return args[0], args[1:]
}

// realmDefault mirrors how the server binaries fall back when the config
// file leaves the realm unset.
func realmDefault() string {
	if globalConfig.Server.Realm != "" {
		return globalConfig.Server.Realm
	}
	return "SabreDAV"
}

func resolvePassword(flagValue string, fromStdin bool) string {
	if fromStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintf(os.Stderr, "Failed to read password from stdin: %v\n", err)
			os.Exit(1)
		}
		return strings.TrimRight(line, "\r\n")
	}
	return flagValue
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
