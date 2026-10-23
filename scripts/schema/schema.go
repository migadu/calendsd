// Package schema embeds the numbered migrations in migrations/, so every
// binary carries the schema its own code expects. `calctl schema apply` runs
// the ones a database has not had yet (the deploy runs it before restarting
// the server), and the health checks know from them which tables must exist
// and which version to expect. There is no second copy to keep in sync.
//
// A migration is NNNN_name.sql. It runs once, in its own transaction, and is
// recorded in schema_migrations. 0001 and 0002 are the baseline: the sabre
// tables and the extension schema as they stood before the runner. Both are
// conditional throughout, so they run as no-ops on a database that already
// has them. Later migrations need not be: each one runs exactly once.
package schema

import (
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

//go:embed migrations/*.sql
var files embed.FS

// Migration is one numbered schema change.
type Migration struct {
	Version int
	Name    string // the file name without the number and extension
	SQL     string
}

var fileName = regexp.MustCompile(`^(\d{4})_(\w+)\.sql$`)

// Migrations returns the embedded migrations in version order.
var Migrations = sync.OnceValue(func() []Migration {
	ms, err := load(files)
	if err != nil {
		panic(err) // the embedded files are fixed at build time; the test catches this
	}
	return ms
})

func load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, err
	}
	var ms []Migration
	for _, e := range entries { // ReadDir sorts by name, so by version
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration %s: name is not NNNN_name.sql", e.Name())
		}
		version, _ := strconv.Atoi(m[1])
		if version != len(ms)+1 {
			return nil, fmt.Errorf("migration %s: want version %04d, numbers must be consecutive", e.Name(), len(ms)+1)
		}
		text, err := fs.ReadFile(fsys, "migrations/"+e.Name())
		if err != nil {
			return nil, err
		}
		ms = append(ms, Migration{Version: version, Name: m[2], SQL: string(text)})
	}
	return ms, nil
}

// Latest is the version a database is at once every migration has run.
func Latest() int { return len(Migrations()) }

var createTable = regexp.MustCompile(`(?m)^CREATE TABLE\s+(?:IF NOT EXISTS\s+)?(\w+)`)

// Tables lists the tables the migrations create, in order, without
// duplicates.
func Tables() []string {
	var tables []string
	seen := map[string]bool{}
	for _, m := range Migrations() {
		for _, t := range createTable.FindAllStringSubmatch(m.SQL, -1) {
			if name := strings.ToLower(t[1]); !seen[name] {
				seen[name] = true
				tables = append(tables, name)
			}
		}
	}
	return tables
}
