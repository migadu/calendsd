// Principal backend over the sabre principals/groupmembers tables.

package storage

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"strconv"
	"strings"

	"calendsd/internal/acl"
	davxml "calendsd/internal/dav/xml"
)

// Principals is the principal backend over the sabre schema. Port of
// DAVACL\PrincipalBackend\PDO.
type Principals struct {
	DB *sql.DB
}

// PrincipalsByPrefix lists the principals directly under prefix (one more
// path segment, so not the proxy sub-principals), filtered in SQL rather
// than by reading the whole table.
func (p *Principals) PrincipalsByPrefix(ctx context.Context, prefix string) ([]acl.Principal, error) {
	under, deeper := "%", "%/%" // prefix "": URIs with no slash at all
	if prefix != "" {
		under = escapeLike(prefix) + "/%"
		deeper = under + "/%"
	}
	rows, err := p.DB.QueryContext(ctx, `SELECT uri, email, displayname FROM principals
 WHERE uri LIKE $1 ESCAPE '\' AND uri NOT LIKE $2 ESCAPE '\'`, under, deeper)
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
		out = append(out, *principal)
	}
	return out, rows.Err()
}

func (p *Principals) PrincipalByPath(ctx context.Context, path string) (*acl.Principal, error) {
	principal, err := scanPrincipal(p.DB.QueryRowContext(ctx,
		`SELECT uri, email, displayname FROM principals WHERE uri = $1`, path))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return principal, err
}

func scanPrincipal(row rowScanner) (*acl.Principal, error) {
	var uri string
	var email, displayname sql.NullString
	if err := row.Scan(&uri, &email, &displayname); err != nil {
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

func (p *Principals) GroupMemberSet(ctx context.Context, principal string) ([]string, error) {
	return p.groupQuery(ctx, `
SELECT members.uri
  FROM groupmembers
  JOIN principals AS groups ON groupmembers.principal_id = groups.id
  JOIN principals AS members ON groupmembers.member_id = members.id
 WHERE groups.uri = $1`, principal)
}

func (p *Principals) GroupMembership(ctx context.Context, principal string) ([]string, error) {
	return p.groupQuery(ctx, `
SELECT groups.uri
  FROM groupmembers
  JOIN principals AS groups ON groupmembers.principal_id = groups.id
  JOIN principals AS members ON groupmembers.member_id = members.id
 WHERE members.uri = $1`, principal)
}

// SetGroupMemberSet replaces a group's members, transcribing sabre's PDO
// setGroupMemberSet: resolve principal ids, delete the old rows, insert the new
// ones. The group and every member must already exist as principals.
func (p *Principals) SetGroupMemberSet(ctx context.Context, principal string, members []string) error {
	return withTx(ctx, p.DB, func(tx *sql.Tx) error {
		var groupID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM principals WHERE uri = $1`, principal).Scan(&groupID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM groupmembers WHERE principal_id = $1`, groupID); err != nil {
			return err
		}
		for _, member := range members {
			var memberID int64
			if err := tx.QueryRowContext(ctx, `SELECT id FROM principals WHERE uri = $1`, member).Scan(&memberID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
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
func (p *Principals) SearchPrincipals(ctx context.Context, prefix string, search map[xml.Name]string, test string) ([]string, error) {
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
	rows, err := p.DB.QueryContext(ctx,
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

func (p *Principals) groupQuery(ctx context.Context, query, principal string) ([]string, error) {
	rows, err := p.DB.QueryContext(ctx, query, principal)
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
