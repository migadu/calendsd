// Package logging configures the process-wide slog logger from a level name,
// so calserver, calctl, and calclient share one way to turn a
// config/flag/env string into a verbosity. Without this every binary used
// slog's built-in default (a text handler pinned at Info on stderr) with no
// way to raise or lower it.
package logging

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// parseLevel maps a level name (debug, info, warn, error; any case) to an
// slog.Level via slog's own parser. An empty string means the default
// (info); any other unrecognised name is an error rather than a silent
// fallback, so a typo in the config fails loudly.
func parseLevel(name string) (slog.Level, error) {
	lvl := slog.LevelInfo
	if name = strings.TrimSpace(name); name == "" {
		return lvl, nil
	}
	if err := lvl.UnmarshalText([]byte(name)); err != nil {
		return 0, fmt.Errorf("unknown log level %q (want debug, info, warn, or error)", name)
	}
	return lvl, nil
}

// Setup installs a process-wide slog text handler on stderr at the requested
// level. Precedence: the level argument (resolved by the caller from an
// explicit -log-level flag then [logging].level) wins; when it is empty the
// $CALSERVER_LOG_LEVEL environment variable applies, letting an operator raise
// verbosity without editing the config file or redeploying; failing both, the
// default is info.
func Setup(level string) error {
	if level == "" {
		level = os.Getenv("CALSERVER_LOG_LEVEL")
	}
	lvl, err := parseLevel(level)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
	return nil
}
