package server

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"calendsd/internal/storage"
)

// stalledPrincipals is a Principals store whose every query hangs until its
// context ends: a listener that accepts and never answers pgx's startup.
func stalledPrincipals(t *testing.T) *storage.Principals {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	db, err := storage.Open("postgres://u@" + ln.Addr().String() + "/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &storage.Principals{DB: db}
}

// The memo expires (audit 6.7): calctl deletes accounts from another process,
// so a principal confirmed long ago is re-checked against the database. A
// stale entry must hit the (here stalled) lookup instead of answering from
// memory; a fresh one answers at once.
func TestProvisionMemoExpires(t *testing.T) {
	p := &provisioner{
		principals: stalledPrincipals(t),
		seen: map[string]time.Time{
			"principals/fresh@example.org": time.Now(),
			"principals/stale@example.org": time.Now().Add(-seenTTL - time.Minute),
		},
	}
	if err := p.ensure(context.Background(), "principals/fresh@example.org", "fresh@example.org"); err != nil {
		t.Fatalf("fresh memo: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := p.ensure(ctx, "principals/stale@example.org", "stale@example.org"); err == nil {
		t.Fatal("stale memo answered without consulting the database")
	}
}

// One user's stalled first login must not hold up anyone else's: before, a
// single mutex was held across the lookup, so every login queued behind it.
func TestProvisionStallDoesNotBlockOthers(t *testing.T) {
	p := &provisioner{
		principals: stalledPrincipals(t),
		seen:       map[string]time.Time{"principals/bob@example.org": time.Now()},
	}
	stalled := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { stalled <- p.ensure(ctx, "principals/alice@example.org", "alice@example.org") }()
	time.Sleep(50 * time.Millisecond) // let alice's lookup start and stall

	done := make(chan error, 1)
	go func() { done <- p.ensure(context.Background(), "principals/bob@example.org", "bob@example.org") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bob: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("bob's login waited on alice's stalled provisioning")
	}

	// A second request for alice waits on the first, but only as long as its
	// own context allows.
	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	if err := p.ensure(short, "principals/alice@example.org", "alice@example.org"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("concurrent alice: err = %v, want deadline exceeded", err)
	}

	cancel()
	if err := <-stalled; err == nil {
		t.Error("stalled alice: ensure succeeded against a dead database")
	}
	if _, memoized := p.seen["principals/alice@example.org"]; memoized {
		t.Error("a failed provisioning was memoized as seen")
	}
}

// A panic in create still reaches the caller (WrapRecover answers it), but
// gives up the in-flight slot: the next login for the same principal runs
// create again instead of waiting out its deadline on the panicked attempt.
func TestProvisionPanicReleasesSlot(t *testing.T) {
	p := &provisioner{} // nil principals: create panics
	ensure := func(ctx context.Context) (panicked bool, err error) {
		defer func() { panicked = recover() != nil }()
		return false, p.ensure(ctx, "principals/u@example.org", "u@example.org")
	}
	if panicked, _ := ensure(context.Background()); !panicked {
		t.Fatal("create's panic was swallowed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	panicked, err := ensure(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("second ensure waited on the panicked attempt's slot")
	}
	if !panicked {
		t.Fatalf("second ensure did not reach create: err=%v", err)
	}
	if _, memoized := p.seen["principals/u@example.org"]; memoized {
		t.Fatal("a panicked create was memoized as provisioned")
	}
}
