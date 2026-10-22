package logging

import (
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"": slog.LevelInfo, "debug": slog.LevelDebug, " INFO ": slog.LevelInfo,
		"Warn": slog.LevelWarn, "error": slog.LevelError,
	} {
		if got, err := parseLevel(in); err != nil || got != want {
			t.Errorf("parseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseLevel("verbose"); err == nil {
		t.Error("parseLevel(verbose) = nil error, want an unknown-level error")
	}
}
