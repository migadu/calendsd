package storage

import (
	"database/sql"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"

	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
)

type xmlName = xml.Name

// Property storage value types, shared with sabre's
// PropertyStorage\Backend\PDO (PLAN.md §5). Type 3 (PHP-serialized objects)
// is language-specific: never written, treated as opaque on read.
const (
	vtString = 1
	vtXML    = 2
	vtObject = 3
)

// PropertyStorage persists arbitrary dead properties in the shared
// propertystorage table. Port of sabre's PropertyStorage\Backend\PDO,
// limited to the cross-readable value types 1 (string) and 2 (XML).
type PropertyStorage struct {
	DB *sql.DB
}

var _ dav.PropStorageBackend = (*PropertyStorage)(nil)

func (p *PropertyStorage) PropFind(path string, pf *dav.PropFind) error {
	rows, err := p.DB.Query(
		`SELECT name, value, valuetype FROM propertystorage WHERE path = $1`, path)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var value []byte
		var valuetype sql.NullInt64
		if err := rows.Scan(&name, &value, &valuetype); err != nil {
			return err
		}
		xmlName, err := davxml.ParseClark(name)
		if err != nil {
			continue // non-clark names cannot round-trip; skip
		}
		switch valuetype.Int64 {
		case vtString:
			pf.Set(xmlName, string(value), http.StatusOK)
		case vtXML:
			pf.Set(xmlName, davxml.Raw(value), http.StatusOK)
		case vtObject:
			// PHP-serialized: sabre owns those rows (PLAN.md §5).
		}
	}
	return rows.Err()
}

func (p *PropertyStorage) PropPatch(path string, pp *dav.PropPatch) error {
	pp.HandleRemaining(func(values map[xmlName]any) (map[xmlName]int, bool) {
		// PROPPATCH is all-or-nothing (RFC 4918 §9.2): apply every property
		// write in one transaction so a failure midway leaves nothing
		// half-committed.
		err := withTx(p.DB, func(tx *sql.Tx) error {
			for name, value := range values {
				clark := davxml.Clark(name)
				if value == nil {
					if _, err := tx.Exec(
						`DELETE FROM propertystorage WHERE path = $1 AND name = $2`,
						path, clark); err != nil {
						return err
					}
					continue
				}
				var stored []byte
				valuetype := vtString
				switch v := value.(type) {
				case string:
					stored = []byte(v)
				case []byte:
					// A raw XML fragment from the PROPPATCH body.
					stored, valuetype = v, vtXML
				default:
					return fmt.Errorf("unsupported property value type %T", value)
				}
				if _, err := tx.Exec(`
INSERT INTO propertystorage (path, name, valuetype, value) VALUES ($1, $2, $3, $4)
ON CONFLICT (path, name) DO UPDATE SET valuetype = $3, value = $4`,
					path, clark, valuetype, stored); err != nil {
					return err
				}
			}
			return nil
		})
		return nil, err == nil
	})
	return nil
}

func (p *PropertyStorage) Delete(path string) error {
	_, err := p.DB.Exec(
		`DELETE FROM propertystorage WHERE path = $1 OR path LIKE $2 ESCAPE '\'`,
		path, escapeLike(path)+"/%")
	return err
}

// Move renames the exact path and all children, like sabre's PDO backend, in
// one statement, so a failure cannot leave the property subtree half-renamed.
func (p *PropertyStorage) Move(source, destination string) error {
	_, err := p.DB.Exec(`
UPDATE propertystorage SET path = $2 || substr(path, length($1) + 1)
 WHERE path = $1 OR path LIKE $3 ESCAPE '\'`,
		source, destination, escapeLike(source)+"/%")
	return err
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
