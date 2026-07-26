package server

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"

	"calserver/internal/storage"
)

// provisioner creates a principal on a user's first successful login, for auth
// backends (like rcptd) that authenticate against an external identity source
// and therefore meet users the database has never seen. It creates only the
// principal triple; the default calendar and address book are left to lazy
// AutoProvision (pair -rcptd-url with -auto-provision), so a user who later
// deletes all their calendars still gets one back.
//
// It memoizes principals seen this process and serializes the check-and-create
// under one mutex, so concurrent first requests for the same user can't race
// into a duplicate-key error.
type provisioner struct {
	principals *storage.Principals
	writes     *storage.AdminWrites
	realm      string

	mu   sync.Mutex
	seen map[string]bool
}

// ensure makes sure principal exists, creating it (and its users row) when this
// is the login's first sighting. email seeds the principal's address and
// display name. Idempotent and safe under concurrency.
func (p *provisioner) ensure(principal, email string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen[principal] {
		return nil
	}
	existing, err := p.principals.PrincipalByPath(principal)
	if err != nil {
		return err
	}
	if existing != nil {
		p.seen[principal] = true
		return nil
	}

	username := strings.TrimPrefix(principal, "principals/")
	// The users.digesta1 column is unused for rcptd logins (rcptd owns the
	// password), but it's NOT NULL, so store an unguessable random value that
	// no md5(user:realm:pass) can equal — Digest auth simply never matches.
	if err := p.writes.CreateUser(username, blockedDigestA1(), email, email); err != nil {
		// A racing request may have created it between our check and insert;
		// treat "now exists" as success rather than a hard error.
		if again, e2 := p.principals.PrincipalByPath(principal); e2 == nil && again != nil {
			p.seen[principal] = true
			return nil
		}
		return err
	}
	p.seen[principal] = true
	return nil
}

// blockedDigestA1 returns a random 32-hex string shaped like a digesta1 hash
// but corresponding to no password.
func blockedDigestA1() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("server: reading CSPRNG for blocked digesta1: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
