// Command calctl is the calserver management CLI, following the mqctl /
// rcptctl convention in the sibling services: a small subcommand tool that
// reads the same config.toml as the server and answers operational questions
// (starting with connectivity health) from the command line.
package main

import (
	"fmt"
	"os"
	"strings"

	"calserver/internal/config"
	"calserver/internal/version"
)

// globalConfig holds the loaded config file; all-zero when no file was found
// (calserver itself runs fine on flags alone, so calctl falls back to the
// built-in defaults instead of failing).
var globalConfig config.Config

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	if os.Args[1] == "-v" || os.Args[1] == "-version" || os.Args[1] == "--version" || os.Args[1] == "version" {
		printVersion()
		os.Exit(0)
	}

	if os.Args[1] == "help" || os.Args[1] == "--help" || os.Args[1] == "-h" {
		printUsage()
		os.Exit(0)
	}

	// Find --config flag and command, collect subcommand args.
	command := ""
	configPath := ""
	var subArgs []string
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "--config" && i+1 < len(os.Args) {
			configPath = os.Args[i+1]
			i++
		} else if !strings.HasPrefix(os.Args[i], "-") && command == "" {
			command = os.Args[i]
		} else {
			subArgs = append(subArgs, os.Args[i])
		}
	}

	if command == "" {
		printUsage()
		os.Exit(1)
	}

	if configPath == "" {
		for _, p := range []string{
			"/etc/calserver/config.toml",
			"/usr/local/etc/calserver/config.toml",
			"config.toml",
		} {
			if _, err := os.Stat(p); err == nil {
				configPath = p
				break
			}
		}
	}
	if configPath != "" {
		cfg, err := config.Load(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to load config %s: %v\n", configPath, err)
			os.Exit(1)
		}
		globalConfig = *cfg
	}

	switch command {
	case "health":
		handleHealth(subArgs)
	case "stats":
		handleStats(subArgs)
	case "doctor":
		handleDoctor(subArgs)
	case "principal", "principals":
		handlePrincipal(subArgs)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printVersion() {
	fmt.Println(version.String("calctl"))
}

func printUsage() {
	fmt.Print(`calctl — calserver management CLI

Usage:
  calctl [--config PATH] <command> [options]

Global Options:
  --config PATH    Path to calserver config file (default: first of
                   /etc/calserver/config.toml,
                   /usr/local/etc/calserver/config.toml, ./config.toml;
                   built-in defaults when none exists)

Commands:
  health [--json] [--url URL] [--db DSN]   Check DAV server and database connectivity
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
`)
}
