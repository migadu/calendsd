package server

import (
	"context"
	"strings"
	"sync"
	"time"

	"calendsd/internal/storage"
)

// provisioner creates a principal on a user's first successful login, for auth
// backends (like rcptd) that authenticate against an external identity source
// and therefore meet users the database has never seen. It creates only the
// principal triple; the default calendar and address book are left to lazy
// AutoProvision (pair -rcptd-url with -auto-provision), so a user who later
// deletes all their calendars still gets one back.
//
// It memoizes principals seen this process and serializes the check-and-create
// per principal, so concurrent first requests for the same user can't race
// into a duplicate-key error. The serialization is deliberately per principal
// and the database work runs outside the shared lock: after a restart every
// user's first request comes through here, and one stalled lookup must not
// queue every other user's login behind it.
type provisioner struct {
	principals *storage.Principals
	writes     *storage.AdminWrites
	realm      string

	mu       sync.Mutex
	seen     map[string]time.Time // principal → when it was last confirmed to exist
	inflight map[string]chan struct{}
}

// seenTTL bounds the memo: a principal deleted by calctl (a separate process,
// so no in-process invalidation is possible) is re-checked against the
// database within this long, the same window as the rcptd credential cache
// (audit 6.7).
const seenTTL = 5 * time.Minute

// ensure makes sure principal exists, creating it (and its users row) when this
// is the login's first sighting. email seeds the principal's address and
// display name. Idempotent and safe under concurrency; ctx bounds both the
// database work and the wait for a concurrent ensure of the same principal.
func (p *provisioner) ensure(ctx context.Context, principal, email string) error {
	for {
		p.mu.Lock()
		if at, ok := p.seen[principal]; ok && time.Since(at) < seenTTL {
			p.mu.Unlock()
			return nil
		}
		if wait, busy := p.inflight[principal]; busy {
			p.mu.Unlock()
			select {
			case <-wait:
				continue // re-check: the other attempt may have failed
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		done := make(chan struct{})
		if p.inflight == nil {
			p.inflight = map[string]chan struct{}{}
		}
		p.inflight[principal] = done
		p.mu.Unlock()

		return p.createHeld(ctx, principal, email, done)
	}
}

// createHeld runs create while holding principal's in-flight slot, and gives
// the slot up in a defer: a panic in create (recovered by WrapRecover) must
// not leave the slot taken, or every later login for this principal would
// wait out its request deadline on a done that never closes.
func (p *provisioner) createHeld(ctx context.Context, principal, email string, done chan struct{}) error {
	var err error
	finished := false // still false here means create panicked
	defer func() {
		p.mu.Lock()
		delete(p.inflight, principal)
		if finished && err == nil {
			if p.seen == nil {
				p.seen = map[string]time.Time{}
			}
			p.seen[principal] = time.Now()
		}
		p.mu.Unlock()
		close(done)
	}()
	err = p.create(ctx, principal, email)
	finished = true
	return err
}

// create looks principal up and inserts it when absent. Callers hold the
// principal's in-flight slot, so no other ensure races it in this process.
func (p *provisioner) create(ctx context.Context, principal, email string) error {
	existing, err := p.principals.PrincipalByPath(ctx, principal)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}

	username := strings.TrimPrefix(principal, "principals/")
	// The users.digesta1 column is unused for rcptd logins (rcptd owns the
	// password), but it's NOT NULL, so store an unguessable random value that
	// no md5(user:realm:pass) can equal — Digest auth simply never matches.
	if err := p.writes.CreateUser(ctx, username, storage.BlockedDigestA1(), email, email); err != nil {
		// Another process (or a racing first login on another host) may have
		// created it between our check and insert; treat "now exists" as
		// success rather than a hard error.
		if again, e2 := p.principals.PrincipalByPath(ctx, principal); e2 == nil && again != nil {
			return nil
		}
		return err
	}
	return nil
}
