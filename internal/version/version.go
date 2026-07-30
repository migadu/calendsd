// Package version carries the build identity shared by every calserver
// binary. The Makefile injects the values via -ldflags -X; a plain
// `go build` falls back to the VCS metadata the Go toolchain embeds on its
// own (buildvcs), so `<binary> -version` is never entirely blank.
package version

import (
	"flag"
	"fmt"
	"runtime/debug"
)

// Stamped via -ldflags "-X calserver/internal/version.Version=..." (see the
// Makefile). Version also surfaces as the calserver_build_info metric label.
var (
	Version = "dev" // git describe --always --dirty
	Commit  = ""    // git short hash
	Date    = ""    // build time, UTC RFC 3339
)

// Flags registers the -version/-v pair on the default flag set and returns
// the destination; check it right after flag.Parse.
func Flags() *bool {
	v := flag.Bool("version", false, "print version (git commit and build time) and exit")
	flag.BoolVar(v, "v", false, "shorthand for -version")
	return v
}

// Info returns the resolved (version, commit, date) triple, falling back to
// the toolchain's VCS metadata for whatever the Makefile didn't stamp. Used
// by the -version output and the startup log line.
func Info() (version, commit, date string) {
	commit, date = Commit, Date
	if commit == "" || date == "" {
		vcsCommit, vcsDate := fromBuildInfo()
		if commit == "" {
			commit = vcsCommit
		}
		if date == "" {
			date = vcsDate
		}
	}
	if commit == "" {
		commit = "unknown"
	}
	if date == "" {
		date = "unknown"
	}
	return Version, commit, date
}

// String renders the -version line for the named binary, e.g.
// "calserver 4430a12 (commit 4430a12, built 2026-07-30T06:51:00Z)".
func String(binary string) string {
	version, commit, date := Info()
	return fmt.Sprintf("%s %s (commit %s, built %s)", binary, version, commit, date)
}

// fromBuildInfo reads the revision and commit time the Go toolchain embeds
// into any build made inside a checkout, covering `go build` runs the
// Makefile stamping doesn't reach. The commit time stands in for the build
// time there — close enough to answer "which build is this?".
func fromBuildInfo() (commit, date string) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ""
	}
	var modified bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			commit = s.Value
		case "vcs.time":
			date = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	if modified && commit != "" {
		commit += "-dirty"
	}
	return commit, date
}
