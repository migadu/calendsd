// Package logging configures the process-wide slog logger from a level name,
// so calserver, calserver-admin, and calclient share one way to turn a
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

// levels maps the accepted names to slog levels. Case-insensitive.
var levels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// ParseLevel maps a level name to an slog.Level. An empty string means the
// default (info); any other unrecognised name is an error rather than a silent
// fallback, so a typo in the config fails loudly.
func ParseLevel(name string) (slog.Level, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return slog.LevelInfo, nil
	}
	lvl, ok := levels[name]
	if !ok {
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
	lvl, err := ParseLevel(level)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
	return nil
}
