// Package server composes the full calserver DAV server from a shared sabre
// Postgres database: tree (principals + calendars), auth, ACL, and CalDAV
// plugins. Mirrors scripts/sabre-server.php so both servers expose the same
// feature set during parity testing.
package server

import (
	"database/sql"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	"calserver/internal/storage"
)

// Config carries server composition options.
type Config struct {
	BaseURI string
	// Realm must match the realm baked into users.digesta1 (PLAN.md §5).
	Realm string
}

// New builds the storage-backed server.
func New(db *sql.DB, cfg Config) *dav.Server {
	if cfg.Realm == "" {
		cfg.Realm = "SabreDAV"
	}

	principals := &storage.Principals{DB: db}
	calendars := &storage.CalDAV{DB: db}
	users := &storage.Users{DB: db}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, calendars, "calendars"))

	srv := dav.NewServer(cfg.BaseURI, root)
	srv.AddPlugin(auth.NewPlugin(
		&auth.DigestBackend{Realm: cfg.Realm, HashA1: users.HashA1},
		&auth.BasicBackend{Realm: cfg.Realm, Validate: func(username, password string) bool {
			// Basic verifies against the same digesta1 hash.
			hash, ok := users.HashA1(cfg.Realm, username)
			return ok && auth.DigestA1(username, cfg.Realm, password) == hash
		}},
	))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&dav.SyncPlugin{})
	return srv
}
