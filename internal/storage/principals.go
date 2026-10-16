// Principal backend over the sabre principals/groupmembers tables.

package storage

import (
	"context"
	"database/sql"
	"encoding/xml"
	"strconv"
	"strings"

	"calserver/internal/acl"
	davxml "calserver/internal/dav/xml"
	"calserver/internal/pathutil"
)

// Principals is the principal backend over the sabre schema. Port of
// DAVACL\PrincipalBackend\PDO.
type Principals struct {
	DB *sql.DB
}

func (p *Principals) PrincipalsByPrefix(prefix string) ([]acl.Principal, error) {
	rows, err := p.DB.Query(`SELECT uri, email, displayname FROM principals`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []acl.Principal
	for rows.Next() {
		principal, err := scanPrincipal(rows)
		if err != nil {
			return nil, err
		}
		if parent, _ := pathutil.Split(principal.URI); parent == prefix {
			out = append(out, *principal)
		}
	}
	return out, rows.Err()
}

func (p *Principals) PrincipalByPath(path string) (*acl.Principal, error) {
	return p.PrincipalByPathContext(context.Background(), path)
}

// PrincipalByPathContext is PrincipalByPath bounded by ctx.
func (p *Principals) PrincipalByPathContext(ctx context.Context, path string) (*acl.Principal, error) {
	rows, err := p.DB.QueryContext(ctx,
		`SELECT uri, email, displayname FROM principals WHERE uri = $1`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanPrincipal(rows)
}

func scanPrincipal(rows *sql.Rows) (*acl.Principal, error) {
	var uri string
	var email, displayname sql.NullString
	if err := rows.Scan(&uri, &email, &displayname); err != nil {
		return nil, err
	}
	principal := acl.Principal{URI: uri, Props: map[xml.Name]any{}}
	if displayname.Valid && displayname.String != "" {
		principal.Props[xml.Name{Space: davxml.NSDav, Local: "displayname"}] = displayname.String
	}
	if email.Valid && email.String != "" {
		principal.Props[xml.Name{Space: davxml.NSSabreDAV, Local: "email-address"}] = email.String
	}
	return &principal, nil
}

func (p *Principals) GroupMemberSet(principal string) ([]string, error) {
	return p.groupQuery(`
SELECT members.uri
  FROM groupmembers
  JOIN principals AS groups ON groupmembers.principal_id = groups.id
  JOIN principals AS members ON groupmembers.member_id = members.id
 WHERE groups.uri = $1`, principal)
}

func (p *Principals) GroupMembership(principal string) ([]string, error) {
	return p.groupQuery(`
SELECT groups.uri
  FROM groupmembers
  JOIN principals AS groups ON groupmembers.principal_id = groups.id
  JOIN principals AS members ON groupmembers.member_id = members.id
 WHERE members.uri = $1`, principal)
}

// SetGroupMemberSet replaces a group's members, transcribing sabre's PDO
// setGroupMemberSet: resolve principal ids, delete the old rows, insert the new
// ones. The group and every member must already exist as principals.
func (p *Principals) SetGroupMemberSet(principal string, members []string) error {
	return withTx(p.DB, func(tx *sql.Tx) error {
		var groupID int64
		if err := tx.QueryRow(`SELECT id FROM principals WHERE uri = $1`, principal).Scan(&groupID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM groupmembers WHERE principal_id = $1`, groupID); err != nil {
			return err
		}
		for _, member := range members {
			var memberID int64
			if err := tx.QueryRow(`SELECT id FROM principals WHERE uri = $1`, member).Scan(&memberID); err != nil {
				return err
			}
			if _, err := tx.Exec(
				`INSERT INTO groupmembers (principal_id, member_id) VALUES ($1, $2)`,
				groupID, memberID); err != nil {
				return err
			}
		}
		return nil
	})
}

// searchColumns maps searchable principal properties to columns.
var searchColumns = map[xml.Name]string{
	{Space: davxml.NSDav, Local: "displayname"}:        "displayname",
	{Space: davxml.NSSabreDAV, Local: "email-address"}: "email",
}

// SearchPrincipals implements a substring search over the searchable
// principal columns. Port of PrincipalBackend\PDO::searchPrincipals.
func (p *Principals) SearchPrincipals(prefix string, search map[xml.Name]string, test string) ([]string, error) {
	if len(search) == 0 {
		return nil, nil
	}
	var clauses []string
	args := []any{prefix + "/%"}
	for name, needle := range search {
		col, ok := searchColumns[name]
		if !ok {
			// Unknown property never matches: allof → no rows, anyof →
			// this term contributes nothing.
			if test != "anyof" {
				return nil, nil
			}
			continue
		}
		args = append(args, "%"+escapeLike(strings.ToLower(needle))+"%")
		clauses = append(clauses, "lower("+col+") LIKE $"+strconv.Itoa(len(args)))
	}
	if len(clauses) == 0 {
		return nil, nil
	}
	joiner := " AND "
	if test == "anyof" {
		joiner = " OR "
	}
	rows, err := p.DB.Query(
		"SELECT uri FROM principals WHERE uri LIKE $1 AND ("+strings.Join(clauses, joiner)+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var uri string
		if err := rows.Scan(&uri); err != nil {
			return nil, err
		}
		out = append(out, uri)
	}
	return out, rows.Err()
}

func (p *Principals) groupQuery(query, principal string) ([]string, error) {
	rows, err := p.DB.Query(query, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var uri string
		if err := rows.Scan(&uri); err != nil {
			return nil, err
		}
		out = append(out, uri)
	}
	return out, rows.Err()
}
