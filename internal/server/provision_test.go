package server

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"calserver/internal/storage"
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

// One user's stalled first login must not hold up anyone else's: before, a
// single mutex was held across the lookup, so every login queued behind it.
func TestProvisionStallDoesNotBlockOthers(t *testing.T) {
	p := &provisioner{
		principals: stalledPrincipals(t),
		seen:       map[string]bool{"principals/bob@example.org": true},
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
	if p.seen["principals/alice@example.org"] {
		t.Error("a failed provisioning was memoized as seen")
	}
}
