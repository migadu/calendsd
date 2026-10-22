// Read and write primitives behind the webmail contacts API
// (docs/snappymail-calserver-contacts.md § The API). These are the queries the
// API needs that the CardDAV backend does not already express: every card a
// principal owns across all their books in one round trip, a card by its
// (book uri, card uri) path, and the contact_frecency table that SnappyMail's
// IncFrec / GetSuggestions map onto.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Contacts reads a principal's cards across all their address books.
type Contacts struct {
	DB *sql.DB
}

// cardETagExpr is how every read here surfaces an etag: the stored one, or the
// md5 of the data for a legacy row whose etag column is NULL — so a client
// never sees an empty etag and a conditional update against such a row can
// still succeed (UpdateCardIfMatch compares the same expression).
const cardETagExpr = `coalesce(c.etag, md5(c.carddata))`

// ContactCard is one stored card with the book it lives in. ID is the cards
// row's serial, which is what the API hands out as the contact id: SnappyMail's
// own action layer runs contact ids through intval() before they reach any
// driver (Actions\Contacts::DoContactsDelete), so the id has to be a number.
// ETag is the bare md5 as the column stores it — not the quoted form CardInfo
// carries — because the API compares it against If-Match after unquoting and
// hands it to the client bare.
type ContactCard struct {
	ID           int64
	BookID       string
	BookURI      string
	URI          string
	Data         []byte
	ETag         string
	LastModified int64
}

// AddressBook is one of a principal's books, by id and uri.
type AddressBook struct {
	ID          string
	URI         string
	DisplayName string
}

// Books lists the principal's address books. Like AddressBooksForUser, but
// without the DAV property map — the API only needs id, uri and name.
func (c *Contacts) Books(ctx context.Context, principalURI string) ([]AddressBook, error) {
	rows, err := c.DB.QueryContext(ctx,
		`SELECT id, uri, coalesce(displayname, '') FROM addressbooks WHERE principaluri = $1 ORDER BY id`,
		principalURI)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AddressBook
	for rows.Next() {
		var id int64
		var b AddressBook
		if err := rows.Scan(&id, &b.URI, &b.DisplayName); err != nil {
			return nil, err
		}
		b.ID = strconv.FormatInt(id, 10)
		out = append(out, b)
	}
	return out, rows.Err()
}

// Cards returns every card the principal owns, across all books, in a
// deterministic (book uri, card uri) order. One round trip; the API filters,
// sorts and pages in Go (Phase 1 of the indexing plan: the p99 user has 446
// cards, and nothing here is worse than what the DAV REPORT path already does).
func (c *Contacts) Cards(ctx context.Context, principalURI string) ([]ContactCard, error) {
	rows, err := c.DB.QueryContext(ctx, `
SELECT c.id, a.id, a.uri, c.uri, c.carddata, `+cardETagExpr+`, coalesce(c.lastmodified, 0)
  FROM cards c JOIN addressbooks a ON a.id = c.addressbookid
 WHERE a.principaluri = $1
 ORDER BY a.uri, c.uri`, principalURI)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactCard
	for rows.Next() {
		var bookID int64
		var card ContactCard
		if err := rows.Scan(&card.ID, &bookID, &card.BookURI, &card.URI, &card.Data, &card.ETag, &card.LastModified); err != nil {
			return nil, err
		}
		card.BookID = strconv.FormatInt(bookID, 10)
		out = append(out, card)
	}
	return out, rows.Err()
}

// CardCount is the number of cards the principal owns across all books — the
// guard the suggestions path uses to decide whether scanning card content per
// keystroke is affordable.
func (c *Contacts) CardCount(ctx context.Context, principalURI string) (int, error) {
	var n int
	err := c.DB.QueryRowContext(ctx, `
SELECT count(*) FROM cards c JOIN addressbooks a ON a.id = c.addressbookid
 WHERE a.principaluri = $1`, principalURI).Scan(&n)
	return n, err
}

// CardByID returns one card by its row id, or nil when it does not exist or
// belongs to another principal. The principal is part of the lookup, which is
// what makes "a handler physically cannot read another user's cards" true at
// the storage layer rather than by convention.
func (c *Contacts) CardByID(ctx context.Context, principalURI string, id int64) (*ContactCard, error) {
	var bookID int64
	card := ContactCard{ID: id}
	err := c.DB.QueryRowContext(ctx, `
SELECT a.id, a.uri, c.uri, c.carddata, `+cardETagExpr+`, coalesce(c.lastmodified, 0)
  FROM cards c JOIN addressbooks a ON a.id = c.addressbookid
 WHERE a.principaluri = $1 AND c.id = $2`,
		principalURI, id).Scan(&bookID, &card.BookURI, &card.URI, &card.Data, &card.ETag, &card.LastModified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	card.BookID = strconv.FormatInt(bookID, 10)
	return &card, nil
}

// CardByPath returns one card by its (book uri, card uri) path, or nil when
// either half does not exist for this principal — the create path's conflict
// check, where the id does not exist yet.
func (c *Contacts) CardByPath(ctx context.Context, principalURI, bookURI, uri string) (*ContactCard, error) {
	var bookID int64
	card := ContactCard{BookURI: bookURI, URI: uri}
	err := c.DB.QueryRowContext(ctx, `
SELECT c.id, a.id, c.carddata, `+cardETagExpr+`, coalesce(c.lastmodified, 0)
  FROM cards c JOIN addressbooks a ON a.id = c.addressbookid
 WHERE a.principaluri = $1 AND a.uri = $2 AND c.uri = $3`,
		principalURI, bookURI, uri).Scan(&card.ID, &bookID, &card.Data, &card.ETag, &card.LastModified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	card.BookID = strconv.FormatInt(bookID, 10)
	return &card, nil
}

// CardByURI finds a card by its uri in any of the principal's books — the fast
// path for "by uid", since both the migration and the API name a card
// <uid>.vcf. With the same uri in several books (duplicate UIDs across books
// exist after the migration), the copy in preferBook wins, then the book
// whose uri sorts first; so the answer is deterministic and, for webmail,
// the webmail copy. Nil when absent.
func (c *Contacts) CardByURI(ctx context.Context, principalURI, uri, preferBook string) (*ContactCard, error) {
	var bookID int64
	card := ContactCard{URI: uri}
	err := c.DB.QueryRowContext(ctx, `
SELECT c.id, a.id, a.uri, c.carddata, `+cardETagExpr+`, coalesce(c.lastmodified, 0)
  FROM cards c JOIN addressbooks a ON a.id = c.addressbookid
 WHERE a.principaluri = $1 AND c.uri = $2
 ORDER BY (a.uri = $3) DESC, a.uri LIMIT 1`,
		principalURI, uri, preferBook).Scan(&card.ID, &bookID, &card.BookURI, &card.Data, &card.ETag, &card.LastModified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	card.BookID = strconv.FormatInt(bookID, 10)
	return &card, nil
}

// Frecency is the per-correspondent usage count behind "most used" address
// suggestions: SnappyMail's prop_frec, which has no home in sabre's schema
// (docs/snappymail-calserver-contacts.md § Suggestions, frecency, and the
// indexing question). frec is owned here and only ever incremented; seed_frec
// is owned by the importer and replaced on every run; ranking reads the sum.
type Frecency struct {
	DB *sql.DB
}

// FrecencyEntry is one address to bump, as parsed out of what SnappyMail's
// IncFrec hands over ("Name <addr>" strings run through an address parser).
type FrecencyEntry struct {
	Email string // lowercased by Bump
	Name  string // may be empty; never overwrites a stored name with ""
}

// Suggestion is one ranked (email, name) pair for autocomplete.
type Suggestion struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	// Score is frec + seed_frec; zero for a suggestion that came from a card
	// rather than the frecency table. Not serialized: the client wants the
	// order, not the number.
	Score int `json:"-"`
}

// Bump increments the count for each address, creating the row on first
// sight with the parsed display name. The name is only ever improved, never
// blanked: an address mailed once with a name and later without keeps it.
// Addresses over the column width are dropped (nothing that long is an
// address anyone will type), names over theirs are truncated. Returns how many
// rows were written.
func (f *Frecency) Bump(ctx context.Context, principalURI string, entries []FrecencyEntry) (int, error) {
	type agg struct {
		name string
		n    int
	}
	merged := map[string]*agg{}
	for _, e := range entries {
		email := strings.ToLower(strings.TrimSpace(e.Email))
		if email == "" || !strings.Contains(email, "@") || utf8.RuneCountInString(email) > frecencyEmailMax {
			continue
		}
		a := merged[email]
		if a == nil {
			a = &agg{}
			merged[email] = a
		}
		a.n++
		if name := truncateRunes(strings.TrimSpace(e.Name), frecencyNameMax); name != "" {
			a.name = name
		}
	}
	if len(merged) == 0 {
		return 0, nil
	}
	emails := make([]string, 0, len(merged))
	for email := range merged {
		emails = append(emails, email)
	}
	sort.Strings(emails) // a fixed lock order, so two concurrent bumps cannot deadlock

	return len(merged), withTx(ctx, f.DB, func(tx *sql.Tx) error {
		for _, email := range emails {
			a := merged[email]
			if _, err := tx.ExecContext(ctx, `
INSERT INTO contact_frecency (principal, email, name, frec, last_used)
VALUES ($1, $2, nullif($3, ''), $4, now())
ON CONFLICT (principal, email) DO UPDATE
   SET frec      = contact_frecency.frec + excluded.frec,
       last_used = now(),
       name      = coalesce(excluded.name, contact_frecency.name)`,
				principalURI, email, a.name, a.n); err != nil {
				return fmt.Errorf("bumping frecency for %s: %w", email, err)
			}
		}
		return nil
	})
}

// Suggest returns the principal's most-used addresses matching q (a
// case-insensitive substring of the address or the name; empty q matches
// everything), highest count first. It reads the indexed table only — the
// caller unions in card-derived pairs when the book is small enough to scan.
func (f *Frecency) Suggest(ctx context.Context, principalURI, q string, limit int) ([]Suggestion, error) {
	if limit <= 0 {
		limit = 20
	}
	pattern := "%" + escapeLike(strings.ToLower(strings.TrimSpace(q))) + "%"
	rows, err := f.DB.QueryContext(ctx, `
SELECT email, coalesce(name, ''), frec + seed_frec AS score
  FROM contact_frecency
 WHERE principal = $1 AND (email LIKE $2 OR lower(coalesce(name, '')) LIKE $2)
 ORDER BY score DESC, email
 LIMIT $3`, principalURI, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Suggestion
	for rows.Next() {
		var s Suggestion
		if err := rows.Scan(&s.Email, &s.Name, &s.Score); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
