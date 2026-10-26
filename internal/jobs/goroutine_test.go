package jobs

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// A loop that panics is logged and restarted rather than crashing the
// process; one that returns normally is not restarted.
func TestGoRunRestartsPanickedLoop(t *testing.T) {
	prev := loopRestartDelay
	loopRestartDelay = time.Millisecond
	defer func() { loopRestartDelay = prev }()

	q := &Queue{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	runs := 0
	q.goRun(context.Background(), "test", func() {
		runs++
		if runs < 3 {
			panic("boom")
		}
	})
	q.Wait()
	if runs != 3 {
		t.Fatalf("loop ran %d times, want 3", runs)
	}
}

// Cancelling ctx ends the restart pause, so Wait does not hang on a loop
// that keeps panicking.
func TestGoRunStopsOnCancelAfterPanic(t *testing.T) {
	prev := loopRestartDelay
	loopRestartDelay = time.Hour
	defer func() { loopRestartDelay = prev }()

	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	q.goRun(ctx, "test", func() { panic("boom") })
	cancel()
	q.Wait()
}
