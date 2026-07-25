package storage

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"strconv"
	"strings"

	"calserver/internal/carddav"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
)

// CardDAV is the address book backend over the sabre schema. Port of
// CardDAV\Backend\PDO.
type CardDAV struct {
	DB *sql.DB
}

var _ carddav.Backend = (*CardDAV)(nil)

func (b *CardDAV) AddressBooksForUser(principalURI string) ([]carddav.AddressBookInfo, error) {
	rows, err := b.DB.Query(
		`SELECT id, uri, displayname, description, synctoken FROM addressbooks WHERE principaluri = $1`,
		principalURI)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []carddav.AddressBookInfo
	for rows.Next() {
		var id, synctoken int64
		var uri string
		var displayname, description sql.NullString
		if err := rows.Scan(&id, &uri, &displayname, &description, &synctoken); err != nil {
			return nil, err
		}
		info := carddav.AddressBookInfo{
			ID: strconv.FormatInt(id, 10), URI: uri, PrincipalURI: principalURI,
			Props: map[xml.Name]any{},
		}
		token := strconv.FormatInt(synctoken, 10)
		info.Props[xml.Name{Space: davxml.NSCalendarServer, Local: "getctag"}] = syncTokenPrefix + token
		info.Props[xml.Name{Space: davxml.NSSabreDAV, Local: "sync-token"}] = token
		if displayname.Valid && displayname.String != "" {
			info.Props[xml.Name{Space: davxml.NSDav, Local: "displayname"}] = displayname.String
		}
		if description.Valid && description.String != "" {
			info.Props[xml.Name{Space: carddav.NSCardDAV, Local: "addressbook-description"}] = description.String
		}
		out = append(out, info)
	}
	return out, rows.Err()
}

func (b *CardDAV) CreateAddressBook(principalURI, uri string, props map[xml.Name]any) (string, error) {
	displayname := stringProp(props, xml.Name{Space: davxml.NSDav, Local: "displayname"})
	description := stringProp(props, xml.Name{Space: carddav.NSCardDAV, Local: "addressbook-description"})
	var id int64
	err := b.DB.QueryRow(`
INSERT INTO addressbooks (principaluri, uri, displayname, description, synctoken)
VALUES ($1, $2, $3, $4, 1) RETURNING id`,
		principalURI, uri, displayname, description).Scan(&id)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(id, 10), nil
}

func stringProp(props map[xml.Name]any, name xml.Name) any {
	if v, ok := props[name].(string); ok {
		return v
	}
	return nil
}

func (b *CardDAV) DeleteAddressBook(id string) error {
	tx, err := b.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM cards WHERE addressbookid = $1`,
		`DELETE FROM addressbookchanges WHERE addressbookid = $1`,
		`DELETE FROM addressbooks WHERE id = $1`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// propertyMapAB maps address book properties to columns.
var abPropertyColumns = map[xml.Name]string{
	{Space: davxml.NSDav, Local: "displayname"}:                  "displayname",
	{Space: carddav.NSCardDAV, Local: "addressbook-description"}: "description",
}

func (b *CardDAV) UpdateAddressBook(id string, pp *dav.PropPatch) {
	var supported []xml.Name
	for name := range abPropertyColumns {
		supported = append(supported, name)
	}
	pp.Handle(supported, func(values map[xml.Name]any) (map[xml.Name]int, bool) {
		var assignments []string
		var args []any
		for name, value := range values {
			col, ok := abPropertyColumns[name]
			if !ok {
				continue
			}
			args = append(args, toNullString(value))
			assignments = append(assignments, col+" = $"+strconv.Itoa(len(args)))
		}
		if len(assignments) == 0 {
			return nil, true
		}
		args = append(args, id)
		if _, err := b.DB.Exec(
			"UPDATE addressbooks SET "+strings.Join(assignments, ", ")+" WHERE id = $"+strconv.Itoa(len(args)),
			args...); err != nil {
			return nil, false
		}
		return nil, true
	})
}

const cardColumns = `uri, carddata, etag, size`

func (b *CardDAV) Cards(addressBookID string) ([]carddav.CardInfo, error) {
	rows, err := b.DB.Query(`SELECT `+cardColumns+` FROM cards WHERE addressbookid = $1`, addressBookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []carddav.CardInfo
	for rows.Next() {
		c, err := scanCard(rows, addressBookID)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (b *CardDAV) Card(addressBookID, uri string) (*carddav.CardInfo, error) {
	rows, err := b.DB.Query(
		`SELECT `+cardColumns+` FROM cards WHERE addressbookid = $1 AND uri = $2`, addressBookID, uri)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanCard(rows, addressBookID)
}

func scanCard(rows *sql.Rows, addressBookID string) (*carddav.CardInfo, error) {
	var uri string
	var etag sql.NullString
	var size sql.NullInt64
	var data []byte
	if err := rows.Scan(&uri, &data, &etag, &size); err != nil {
		return nil, err
	}
	return &carddav.CardInfo{
		URI: uri, AddressBookID: addressBookID, CardData: data,
		ETag: `"` + etag.String + `"`, Size: size.Int64,
	}, nil
}

func (b *CardDAV) CreateCard(addressBookID, uri string, data []byte) (string, error) {
	etag := cardETag(data)
	tx, err := b.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
INSERT INTO cards (addressbookid, uri, carddata, lastmodified, etag, size)
VALUES ($1, $2, $3, extract(epoch from now())::int, $4, $5)`,
		addressBookID, uri, data, etag, len(data)); err != nil {
		return "", err
	}
	if err := abAddChange(tx, addressBookID, uri, 1); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return `"` + etag + `"`, nil
}

func (b *CardDAV) UpdateCard(addressBookID, uri string, data []byte) (string, error) {
	etag := cardETag(data)
	tx, err := b.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
UPDATE cards SET carddata = $1, lastmodified = extract(epoch from now())::int, etag = $2, size = $3
 WHERE addressbookid = $4 AND uri = $5`,
		data, etag, len(data), addressBookID, uri); err != nil {
		return "", err
	}
	if err := abAddChange(tx, addressBookID, uri, 2); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return `"` + etag + `"`, nil
}

func (b *CardDAV) DeleteCard(addressBookID, uri string) error {
	tx, err := b.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`DELETE FROM cards WHERE addressbookid = $1 AND uri = $2`, addressBookID, uri); err != nil {
		return err
	}
	if err := abAddChange(tx, addressBookID, uri, 3); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *CardDAV) ChangesForAddressBook(addressBookID, syncToken string, syncLevel, limit int) (*dav.ChangeSet, error) {
	// Same shape as the calendar sync transcription.
	if syncToken == "" {
		var current sql.NullInt64
		if err := b.DB.QueryRow(`SELECT synctoken FROM addressbooks WHERE id = $1`, addressBookID).Scan(&current); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil // unknown address book: no change info
			}
			return nil, err // surface real DB errors instead of masking them
		}
		out := &dav.ChangeSet{SyncToken: strconv.FormatInt(current.Int64, 10)}
		rows, err := b.DB.Query(`SELECT uri FROM cards WHERE addressbookid = $1`, addressBookID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var uri string
			if err := rows.Scan(&uri); err != nil {
				return nil, err
			}
			out.Added = append(out.Added, uri)
		}
		return out, rows.Err()
	}
	return nil, nil
}

// abAddChange records an address book change and bumps the synctoken under a
// row lock (same anti-race treatment as calendars).
func abAddChange(tx *sql.Tx, addressBookID, uri string, operation int) error {
	var token int64
	if err := tx.QueryRow(
		`SELECT synctoken FROM addressbooks WHERE id = $1 FOR UPDATE`, addressBookID).Scan(&token); err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO addressbookchanges (uri, synctoken, addressbookid, operation)
VALUES ($1, $2, $3, $4)`, uri, token, addressBookID, operation); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE addressbooks SET synctoken = $1 WHERE id = $2`, token+1, addressBookID)
	return err
}

func cardETag(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}
