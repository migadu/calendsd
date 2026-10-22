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
	"calserver/internal/metrics"
)

// CardDAV is the address book backend over the sabre schema. Port of
// CardDAV\Backend\PDO.
type CardDAV struct {
	DB *sql.DB
	// Metrics times changes_for_addressbook, the one expensive named
	// operation here. A nil Set records nothing.
	Metrics *metrics.Set
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
		// sabre's CardDAV PDO backend serves getctag as the bare synctoken
		// (CardDAV/Backend/PDO.php), unlike CalDAV which prefixes it with the
		// sync-URL. Match that so address-book ctags are identical to a stock
		// sabre instance sharing the same database.
		info.Props[xml.Name{Space: davxml.NSCalendarServer, Local: "getctag"}] = token
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
	return withTx(b.DB, func(tx *sql.Tx) error {
		return deleteAddressBookRows(tx, id)
	})
}

// deleteAddressBookRows removes an address book with its cards and change
// log. Shared by DeleteAddressBook and AdminWrites.DeleteUserCascade so the
// two delete paths cannot drift.
func deleteAddressBookRows(tx *sql.Tx, id string) error {
	for _, q := range []string{
		`DELETE FROM cards WHERE addressbookid = $1`,
		`DELETE FROM addressbookchanges WHERE addressbookid = $1`,
		`DELETE FROM addressbooks WHERE id = $1`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
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
		err := withTx(b.DB, func(tx *sql.Tx) error {
			if err := affectedOne(tx.Exec(
				"UPDATE addressbooks SET "+strings.Join(assignments, ", ")+" WHERE id = $"+strconv.Itoa(len(args)),
				args...)); err != nil {
				return err
			}
			return recordChange(tx, addressBookLog, id, "", 2)
		})
		return nil, err == nil
	})
}

// cardColumns surfaces a legacy row's NULL etag as the md5 of its data, like
// objectColumns and the contacts API's cardETagExpr.
const cardColumns = `uri, carddata, coalesce(etag, md5(carddata)), size`

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
	c, err := scanCard(b.DB.QueryRow(
		`SELECT `+cardColumns+` FROM cards WHERE addressbookid = $1 AND uri = $2`, addressBookID, uri),
		addressBookID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func scanCard(row rowScanner, addressBookID string) (*carddav.CardInfo, error) {
	var uri string
	var etag sql.NullString
	var size sql.NullInt64
	var data []byte
	if err := row.Scan(&uri, &data, &etag, &size); err != nil {
		return nil, err
	}
	return &carddav.CardInfo{
		URI: uri, AddressBookID: addressBookID, CardData: data,
		ETag: `"` + etag.String + `"`, Size: size.Int64,
	}, nil
}

func (b *CardDAV) CreateCard(addressBookID, uri string, data []byte) (string, error) {
	etag := cardETag(data)
	err := withTx(b.DB, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
INSERT INTO cards (addressbookid, uri, carddata, lastmodified, etag, size)
VALUES ($1, $2, $3, extract(epoch from now())::int, $4, $5)`,
			addressBookID, uri, data, etag, len(data)); err != nil {
			return err
		}
		return recordChange(tx, addressBookLog, addressBookID, uri, 1)
	})
	if err != nil {
		return "", err
	}
	return `"` + etag + `"`, nil
}

func (b *CardDAV) UpdateCard(addressBookID, uri string, data []byte) (string, error) {
	etag := cardETag(data)
	err := withTx(b.DB, func(tx *sql.Tx) error {
		// A vanished card is ErrNotFound, with no change row.
		if err := affectedOne(tx.Exec(`
UPDATE cards SET carddata = $1, lastmodified = extract(epoch from now())::int, etag = $2, size = $3
 WHERE addressbookid = $4 AND uri = $5`,
			data, etag, len(data), addressBookID, uri)); err != nil {
			return err
		}
		return recordChange(tx, addressBookLog, addressBookID, uri, 2)
	})
	if err != nil {
		return "", err
	}
	return `"` + etag + `"`, nil
}

// UpdateCardIfMatch is UpdateCard with the precondition in the UPDATE itself:
// the row is rewritten only if its stored etag still equals expected (the bare
// md5, as the column holds it — or, for a legacy row with a NULL etag, the md5
// of its data, the same value the API's reads surface), so a write that lost a
// race with a DAV client is refused atomically rather than by a
// read-compare-write that is not. It returns the new quoted etag and
// matched=true on success; matched=false with no write and no change row when
// the card is missing or its etag moved. An empty expected never matches.
// Backs the API's If-Match (docs/snappymail-calserver-contacts.md § Endpoints).
func (b *CardDAV) UpdateCardIfMatch(addressBookID, uri string, data []byte, expected string) (string, bool, error) {
	if expected == "" {
		return "", false, nil
	}
	etag := cardETag(data)
	matched := false
	err := withTx(b.DB, func(tx *sql.Tx) error {
		res, err := tx.Exec(`
UPDATE cards SET carddata = $1, lastmodified = extract(epoch from now())::int, etag = $2, size = $3
 WHERE addressbookid = $4 AND uri = $5 AND coalesce(etag, md5(carddata)) = $6`,
			data, etag, len(data), addressBookID, uri, expected)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		matched = true
		return recordChange(tx, addressBookLog, addressBookID, uri, 2)
	})
	if err != nil || !matched {
		return "", false, err
	}
	return `"` + etag + `"`, true, nil
}

// DeleteAllCards removes every card in one address book, with a change-log
// row per card so a synced client learns of each deletion, in one
// transaction of four statements regardless of how many cards there are —
// not two per card, which for the 20,782-card book would be 40k round trips
// under the book's row lock. All the change rows share one synctoken and the
// token moves once, which is what recordChange does per card, batched; a
// client syncing from an older token sees every uri as deleted either way.
// Returns how many were removed. The book itself stays.
func (b *CardDAV) DeleteAllCards(addressBookID string) (int, error) {
	n := 0
	err := withTx(b.DB, func(tx *sql.Tx) error {
		var token int64
		if err := tx.QueryRow(
			`SELECT synctoken FROM addressbooks WHERE id = $1 FOR UPDATE`, addressBookID).Scan(&token); err != nil {
			return err
		}
		if _, err := tx.Exec(`
INSERT INTO addressbookchanges (uri, synctoken, addressbookid, operation)
SELECT uri, $2, $1, 3 FROM cards WHERE addressbookid = $1`, addressBookID, token); err != nil {
			return err
		}
		res, err := tx.Exec(`DELETE FROM cards WHERE addressbookid = $1`, addressBookID)
		if err != nil {
			return err
		}
		deleted, err := res.RowsAffected()
		if err != nil {
			return err
		}
		n = int(deleted)
		if n == 0 {
			return nil // nothing changed: leave the token and the listeners alone
		}
		if _, err := tx.Exec(`UPDATE addressbooks SET synctoken = $1 WHERE id = $2`, token+1, addressBookID); err != nil {
			return err
		}
		_, err = tx.Exec(`SELECT pg_notify('dav_push', $1)`, "addressbook:"+addressBookID)
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (b *CardDAV) DeleteCard(addressBookID, uri string) error {
	return withTx(b.DB, func(tx *sql.Tx) error {
		if err := affectedOne(tx.Exec(
			`DELETE FROM cards WHERE addressbookid = $1 AND uri = $2`, addressBookID, uri)); err != nil {
			return err
		}
		return recordChange(tx, addressBookLog, addressBookID, uri, 3)
	})
}

func cardETag(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}
