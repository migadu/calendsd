package storage

// Per-recipient-domain notification policy (the domain_scheduling_policy
// table, internal/schema/migrations/0002_extension.sql). A domain operator opts its users
// in to calendsd emails that are not part of CalDAV itself; an absent row
// means every setting is at its default ("off"). share_notify drives the
// calendar-share notice (docs/share-invite-email.md); local_notify is reserved
// for docs/local-attendee-email.md.

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	"github.com/migadu/calendsd/internal/caldav"
)

// DomainPolicy reads domain_scheduling_policy.
type DomainPolicy struct {
	DB *sql.DB
}

var _ caldav.SharePolicy = (*DomainPolicy)(nil)

// ShareNotify returns the domain's share_notify setting, "off" when the
// domain has no row.
func (d *DomainPolicy) ShareNotify(ctx context.Context, domain string) (string, error) {
	var v string
	err := d.DB.QueryRowContext(ctx,
		`SELECT share_notify FROM domain_scheduling_policy WHERE domain = $1`,
		strings.ToLower(domain)).Scan(&v)
	if err == sql.ErrNoRows {
		return "off", nil
	}
	return v, err
}

// CachedSharePolicy memoizes a SharePolicy per domain for TTL. Policies change
// rarely and correctness does not depend on instant propagation, so a change
// takes effect within one TTL. Errors are not cached: the next lookup retries.
type CachedSharePolicy struct {
	Source caldav.SharePolicy
	TTL    time.Duration

	mu      sync.Mutex
	entries map[string]policyEntry
	now     func() time.Time // tests
}

type policyEntry struct {
	value   string
	expires time.Time
}

// ShareNotify implements caldav.SharePolicy.
func (c *CachedSharePolicy) ShareNotify(ctx context.Context, domain string) (string, error) {
	domain = strings.ToLower(domain)
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	c.mu.Lock()
	if e, ok := c.entries[domain]; ok && now().Before(e.expires) {
		c.mu.Unlock()
		return e.value, nil
	}
	c.mu.Unlock()

	v, err := c.Source.ShareNotify(ctx, domain)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]policyEntry{}
	}
	c.entries[domain] = policyEntry{value: v, expires: now().Add(c.TTL)}
	c.mu.Unlock()
	return v, nil
}
