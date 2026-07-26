package storage

import (
	"database/sql"
	"encoding/xml"
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
		tx, err := p.DB.Begin()
		if err != nil {
			return nil, false
		}
		defer tx.Rollback()
		for name, value := range values {
			clark := davxml.Clark(name)
			if value == nil {
				if _, err := tx.Exec(
					`DELETE FROM propertystorage WHERE path = $1 AND name = $2`,
					path, clark); err != nil {
					return nil, false
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
				return nil, false
			}
			if _, err := tx.Exec(`
INSERT INTO propertystorage (path, name, valuetype, value) VALUES ($1, $2, $3, $4)
ON CONFLICT (path, name) DO UPDATE SET valuetype = $3, value = $4`,
				path, clark, valuetype, stored); err != nil {
				return nil, false
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, false
		}
		return nil, true
	})
	return nil
}

func (p *PropertyStorage) Delete(path string) error {
	_, err := p.DB.Exec(
		`DELETE FROM propertystorage WHERE path = $1 OR path LIKE $2 ESCAPE '\'`,
		path, escapeLike(path)+"/%")
	return err
}

func (p *PropertyStorage) Move(source, destination string) error {
	// Sabre's PDO backend renames the exact path and all children. Do the
	// select and every rename in one transaction so a failure mid-way cannot
	// leave the property subtree half-renamed.
	tx, err := p.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		`SELECT id, path FROM propertystorage WHERE path = $1 OR path LIKE $2 ESCAPE '\'`,
		source, escapeLike(source)+"/%")
	if err != nil {
		return err
	}
	type rename struct {
		id      int64
		newPath string
	}
	var renames []rename
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return err
		}
		renames = append(renames, rename{id, destination + strings.TrimPrefix(path, source)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range renames {
		if _, err := tx.Exec(
			`UPDATE propertystorage SET path = $1 WHERE id = $2`, r.newPath, r.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
