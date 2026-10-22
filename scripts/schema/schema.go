// Package schema embeds pgsql.calserver.sql, so every binary carries the
// extension schema its own code expects. `calctl schema apply` loads it (the
// deploy runs that before restarting the server), and the health checks know
// from it which tables must exist. There is no second copy to keep in sync.
package schema

import (
	_ "embed"
	"regexp"
)

// Calendsd is the text of pgsql.calserver.sql. Every statement in it is
// conditional, so applying it again is a no-op.
//
//go:embed pgsql.calserver.sql
var Calendsd string

var createTable = regexp.MustCompile(`(?m)^CREATE TABLE\s+(?:IF NOT EXISTS\s+)?(\w+)`)

// Tables lists the tables pgsql.calserver.sql creates, in file order.
func Tables() []string {
	var tables []string
	for _, m := range createTable.FindAllStringSubmatch(Calendsd, -1) {
		tables = append(tables, m[1])
	}
	return tables
}
