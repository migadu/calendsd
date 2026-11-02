package storage

import (
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"

	"github.com/migadu/calendsd/internal/dav"
	davxml "github.com/migadu/calendsd/internal/dav/xml"
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

var _ dav.PropPrefetcher = (*PropertyStorage)(nil)

func (p *PropertyStorage) PropFind(ctx context.Context, path string, pf *dav.PropFind) error {
	rows, err := p.DB.QueryContext(ctx,
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
		if prop, ok := storedProp(name, value, valuetype.Int64); ok {
			pf.Set(prop.Name, prop.Value, http.StatusOK)
		}
	}
	return rows.Err()
}

// PropFindMany reads the stored properties of many paths in one query.
// Implements dav.PropPrefetcher, for the Depth:1 listings and reports that
// otherwise read propertystorage once per node.
func (p *PropertyStorage) PropFindMany(ctx context.Context, paths []string) (map[string][]dav.StoredProp, error) {
	rows, err := p.DB.QueryContext(ctx,
		`SELECT path, name, value, valuetype FROM propertystorage WHERE path = ANY($1)`, paths)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]dav.StoredProp{}
	for rows.Next() {
		var path, name string
		var value []byte
		var valuetype sql.NullInt64
		if err := rows.Scan(&path, &name, &value, &valuetype); err != nil {
			return nil, err
		}
		if prop, ok := storedProp(name, value, valuetype.Int64); ok {
			out[path] = append(out[path], prop)
		}
	}
	return out, rows.Err()
}

// storedProp turns a propertystorage row into the property a PropFind
// serves: false for a name that cannot round-trip (not clark) or a value
// type this server does not read (PHP-serialized objects, which sabre owns,
// PLAN.md §5).
func storedProp(name string, value []byte, valuetype int64) (dav.StoredProp, bool) {
	xmlName, err := davxml.ParseClark(name)
	if err != nil {
		return dav.StoredProp{}, false
	}
	switch valuetype {
	case vtString:
		return dav.StoredProp{Name: xmlName, Value: string(value)}, true
	case vtXML:
		return dav.StoredProp{Name: xmlName, Value: davxml.Raw(value)}, true
	}
	return dav.StoredProp{}, false
}

func (p *PropertyStorage) PropPatch(ctx context.Context, path string, pp *dav.PropPatch) error {
	pp.HandleRemaining(func(values map[xmlName]any) (map[xmlName]int, bool) {
		// PROPPATCH is all-or-nothing (RFC 4918 §9.2): apply every property
		// write in one transaction so a failure midway leaves nothing
		// half-committed.
		err := withTx(ctx, p.DB, func(tx *sql.Tx) error {
			for name, value := range values {
				clark := davxml.Clark(name)
				if value == nil {
					if _, err := tx.ExecContext(ctx,
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
				if _, err := tx.ExecContext(ctx, `
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

func (p *PropertyStorage) Delete(ctx context.Context, path string) error {
	_, err := p.DB.ExecContext(ctx,
		`DELETE FROM propertystorage WHERE path = $1 OR path LIKE $2 ESCAPE '\'`,
		path, escapeLike(path)+"/%")
	return err
}

// Move renames the exact path and all children, like sabre's PDO backend, in
// one statement, so a failure cannot leave the property subtree half-renamed.
func (p *PropertyStorage) Move(ctx context.Context, source, destination string) error {
	_, err := p.DB.ExecContext(ctx, `
UPDATE propertystorage SET path = $2 || substr(path, length($1) + 1)
 WHERE path = $1 OR path LIKE $3 ESCAPE '\'`,
		source, destination, escapeLike(source)+"/%")
	return err
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
