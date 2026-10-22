// Package pgdial parses a Postgres DSN into a pgx connection config with the
// failover-oriented defaults every calendsd connection shares: the pool, the
// WebDAV-Push LISTEN connection and the job queue's LISTEN connection.
//
// The DSN may name several hosts ("host=a,b,c port=5433"), and pgx tries them
// in order — each with its own connect_timeout — so a dead node costs one
// timeout, not the connection. A hostname resolving to several addresses is
// tried address by address the same way.
package pgdial

import (
	"net"
	"time"

	"github.com/jackc/pgx/v5"
)

// DefaultConnectTimeout bounds each connection attempt when the DSN sets no
// connect_timeout. Without it a black-holed host stalls the attempt for the
// OS's TCP connect timeout (over a minute) before pgx moves to the next host.
const DefaultConnectTimeout = 5 * time.Second

// keepAlive detects a peer that vanished without closing the connection (a
// node powered off, a partitioned link) on an idle connection — chiefly the
// LISTEN connections, which are idle by design and would otherwise silently
// miss push and job wakeups: about Idle + Interval*Count, so ~1m, against
// ~2.5m for Go's default (15s idle, 15s interval, 9 probes) that pgx uses.
// It does not bound a query already in flight to a dead peer; that waits on
// TCP retransmission or the caller's context.
var keepAlive = net.KeepAliveConfig{
	Enable:   true,
	Idle:     30 * time.Second,
	Interval: 10 * time.Second,
	Count:    3,
}

// Config parses dsn and applies the defaults above. Values the DSN sets
// itself (connect_timeout) win.
func Config(dsn string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = DefaultConnectTimeout
	}
	d := &net.Dialer{KeepAliveConfig: keepAlive}
	cfg.DialFunc = d.DialContext
	return cfg, nil
}
