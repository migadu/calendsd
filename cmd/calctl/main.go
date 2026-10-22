// Command calctl is the calendsd management CLI, following the mqctl /
// rcptctl convention in the sibling services: a small subcommand tool that
// reads the same config.toml as the server and answers operational questions
// (starting with connectivity health) from the command line.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"calendsd/internal/config"
	"calendsd/internal/version"
)

// globalConfig holds the loaded config file; all-zero when no file was found
// (calendsd itself runs fine on flags alone, so calctl falls back to the
// built-in defaults instead of failing).
var globalConfig config.Config

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	if os.Args[1] == "-v" || os.Args[1] == "-version" || os.Args[1] == "--version" {
		printVersion()
		os.Exit(0)
	}

	configPath, command, subArgs := parseGlobal(flag.ExitOnError, os.Args[1:])
	if command == "" {
		printUsage()
		os.Exit(1)
	}

	if command == "help" {
		printUsage()
		os.Exit(0)
	}
	if command == "version" {
		printVersion()
		os.Exit(0)
	}

	if configPath == "" {
		configPath = config.FindDefaultConfig()
	}
	if configPath != "" {
		cfg, err := config.Load(configPath)
		if err != nil {
			fatalf("Failed to load config %s: %v", configPath, err)
		}
		globalConfig = *cfg
	}

	// Ctrl-C or SIGTERM cancels whatever the command is waiting on.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "health":
		handleHealth(ctx, subArgs)
	case "panel":
		handlePanel(subArgs, configPath)
	case "stats":
		handleStats(ctx, subArgs)
	case "doctor":
		handleDoctor(ctx, subArgs)
	case "principal", "principals":
		handlePrincipal(ctx, subArgs)
	case "jobs":
		handleJobs(ctx, subArgs)
	case "imip":
		handleImip(ctx, subArgs)
	case "contacts":
		handleContacts(ctx, subArgs)
	case "schema":
		handleSchema(ctx, subArgs)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

// parseGlobal splits the command line into calctl's global flags, the
// command, and the subcommand's own arguments. Global flags come before the
// command and everything after it belongs to the subcommand, so a subcommand
// flag is never taken for a global one, nor a flag's value for the command.
func parseGlobal(onError flag.ErrorHandling, args []string) (configPath, command string, subArgs []string) {
	global := flag.NewFlagSet("calctl", onError)
	global.Usage = printUsage
	path := global.String("config", "", "path to the calendsd config file")
	if err := global.Parse(args); err != nil {
		return "", "", nil
	}
	if global.NArg() == 0 {
		return *path, "", nil
	}
	return *path, global.Arg(0), global.Args()[1:]
}

func printVersion() {
	fmt.Println(version.String("calctl"))
}

func printUsage() {
	fmt.Print(`calctl — calendsd management CLI

Usage:
  calctl [--config PATH] <command> [options]

Global Options:
  --config PATH    Path to calendsd config file (default: first of
                   /etc/calendsd/config.toml,
                   /usr/local/etc/calendsd/config.toml, the pre-rename
                   /etc/calserver and /usr/local/etc/calserver paths,
                   ./config.toml;
                   built-in defaults when none exists)

Commands:
  health [--json] [--url URL] [--db DSN] [--timeout D]
                                           Check calendsd and everything it depends on: DAV and
                                           contacts-API listeners, the database (each node of a
                                           multi-host DSN), the daemon's pool, the job queue, rcptd
  panel [-addr A] [-admin-auth FILE] [-readonly=false] [calendsd flags]
                                           Serve the admin panel (default 127.0.0.1:8092; Basic auth
                                           from [admin].auth_file, an htpasswd -B file)
  stats [--json] [--db DSN] [--changes N]  Show row counts and recent change-log activity
  doctor [--json] [--db DSN] [--scan-cap N] Run read-only consistency checks (exit 1 on findings)
  principal list [--json]                  List principals with email/displayname and login capability
  principal add <user> [--email E] [--name N] (--password P | --password-stdin) [--realm R]
                                           Create a user plus principal triple (like the admin panel)
  principal edit <user> [--email E] [--name N] [--password P | --password-stdin] [--realm R]
                                           Update email/displayname (empty clears) or reset the password
  principal remove <user> --confirm [--cascade]
                                           Immediately delete the users row and principal triple
                                           (refuses while it holds collections unless --cascade)
  principal delete <user> [--in DAYS] [--reason R] --confirm
                                           Soft-delete: schedule for purge after a retention window;
                                           the account cannot log in meanwhile (restore to undo)
  principal restore <user>                 Cancel a pending soft-delete
  principal purge-expired [--dry-run]      Permanently remove accounts past their retention window
                                           (run from cron); --dry-run lists without deleting
  principal auth <user> (--password P | --password-stdin) [--realm R] [--json]
                                           Verify a password against the stored digesta1 (exit 0 on
                                           match; detects hashes minted under a different realm)
  jobs list [--state S] [--worker W] [--queue Q] [--recipient R] [--failed] [--limit N] [--json]
                                           List durable background jobs (credential shown as presence only)
  jobs show <id> [--full-body] [--json]    Show one job in full (args pretty-printed; never the secret)
  jobs retry <id>                          Reset a job to run now (state=available, attempt cleared)
  jobs cancel <id>                         Stop delivery (state=discarded) and drop any retained credential
  jobs reschedule <id> --at <ts|+dur>      Move an available job's next-run time
  jobs delete <id> --confirm               Hard-delete the row (irreversible)
  imip pending [--limit N] [--json]        List failed/retrying iMIP sends (cred=no needs an organizer resend)
  imip resend <id> [--relay]               Re-queue a stuck iMIP send (metered; --relay = unmetered via the server)
  contacts import --bundle DIR [--only ADDR] [--shard NNN] [--workers N] [--dry-run]
                  [--resume=false] [--force-overwrite] [--force-version] [--report FILE]
                                           Import a webmail contacts export bundle (see
                                           docs/snappymail-calserver-contacts.md); re-runnable,
                                           never deletes, --dry-run decides without writing.
                                           Exit 1 on errors or unreadable shards, 2 when
                                           principals were refused or an --only address was
                                           not in the bundle
  contacts summary [--run-id UUID] [--json] Totals from the import ledger, plus the orphaned
                                           address book check (exit 1 on findings)
  schema apply [--db DSN] [--timeout DUR] [--lock-timeout DUR]
                                           Apply the calendsd extension schema built into this
                                           binary; re-runnable, all or nothing (the deploy runs it)
  version                                  Show version information

Examples:
  calctl health
  calctl health --json
  calctl health --url http://127.0.0.1:8080/ --db postgres://localhost/calserver
  calctl stats
  calctl stats --changes 25 --json
  calctl doctor
  calctl doctor --scan-cap 50000
  calctl principal list
  calctl principal add bob --email bob@example.org --name "Bob" --password-stdin < pw.txt
  calctl principal edit bob --name "Robert" --email ""
  calctl principal remove bob --confirm
  calctl principal delete bob --in 30 --confirm
  calctl principal restore bob
  calctl principal purge-expired --dry-run
  calctl jobs list --failed
  calctl jobs show 42
  calctl imip pending
  calctl imip resend 42
  calctl contacts import --bundle /var/tmp/contacts-bundle --dry-run
  calctl contacts import --bundle /var/tmp/contacts-bundle --only alice@example.org
  calctl contacts summary
  calctl schema apply
`)
}
