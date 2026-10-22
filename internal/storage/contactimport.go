// Bulk import of webmail address books into the sabre schema
// (docs/snappymail-calserver-contacts.md § Migration). The counterpart of
// scripts/snappymail-export-contacts.php in the ansible repo, which produces
// the bundle this reads; `calctl contacts import` is the CLI over it.
//
// The whole design is arranged around one property: a re-run is a no-op or a
// repair, never a duplication. That rests on four invariants the exporter
// supplies and this file must not break:
//
//  1. the principal is a pure function of the source address (lowercased);
//  2. the book uri is a constant ("webmail");
//  3. the card uri is a pure function of the source contact;
//  4. the card BYTES are a pure function of the source row and the exporter
//     version — so this package treats CardData as opaque and NEVER parses or
//     re-serializes it. The moment it round-trips a card through a vCard
//     library, the bytes depend on that library's version and the next `go get`
//     turns a no-op re-run into millions of updates.
//
// Given those, md5(carddata) == cards.etag is an exact, cheap "has this card
// changed" test — which is what cardETag already computes.
package storage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Column widths of contact_frecency, which the seed must respect itself: the
// exporter clamps the values on the CARD, but the frecency rows take the name
// straight from the FULLNAME property row and the address from the email row,
// so a 64 KB junk name or an "address" over 500 characters reaches the seed
// untouched. Run 1's dry run refused ten principals this way.
const (
	frecencyEmailMax = 320 // contact_frecency.email VARCHAR(320)
	frecencyNameMax  = 255 // contact_frecency.name  VARCHAR(255)
)

// DefaultMaxCardBytes bounds a single card. The largest real card in the
// SnappyMail store is 6.97 MB (an embedded photo), so this rejects nothing
// genuine while keeping one corrupt bundle record from becoming an unbounded
// allocation.
const DefaultMaxCardBytes = 7_500_000

// ContactImport lands one principal's exported webmail contacts, in one
// transaction per principal. It is safe to run several concurrently: each
// principal is serialized by an advisory lock keyed on its principaluri, so two
// workers (or a second run started by someone who did not know the first was
// going) queue instead of racing into addressbooks_ukey.
type ContactImport struct {
	DB *sql.DB
	// MaxCardBytes overrides DefaultMaxCardBytes when non-zero.
	MaxCardBytes int
}

// ImportCard is one card exactly as the bundle carries it.
type ImportCard struct {
	URI  string // "<uid>.vcf"
	Data []byte // opaque vCard bytes; see invariant 4
	// LastModified is the source contact's `changed`, not the import clock, so
	// clients get a truthful timestamp and the row stays a pure function of the
	// bundle. Zero falls back to the export epoch.
	LastModified int64
}

// SeedFrecency is one migrated usage count (SnappyMail's prop_frec).
type SeedFrecency struct {
	Email string // lowercased by the importer if it is not already
	Name  string // display name; "" is stored as NULL
	Count int
}

// ImportRequest is everything one principal's import needs. Cards and Frecency
// are the principal's complete set as of the export — the import is not
// incremental within a principal.
type ImportRequest struct {
	RunID       string  // uuid identifying this run, recorded in the ledger
	Principal   string  // bare lowercased address, e.g. "alice@example.org"
	SourceUsers []int32 // the rainloop_users id(s) folded into this principal
	BookURI     string  // "webmail"
	BookName    string  // "Webmail contacts"
	BookDesc    string
	Digest      string // the bundle's per-principal sentinel digest
	ExporterVer string // exporter + Sabre\VObject version that produced it
	ExportEpoch int64  // fallback for a card with no source timestamp

	Cards    []ImportCard
	Frecency []SeedFrecency

	// DryRun decides everything and writes nothing: the transaction is rolled
	// back at the end, so the result reports exactly what a real run would do.
	DryRun bool
	// ForceOverwrite permits the one case the decision table refuses by
	// default — the bundle changed AND a DAV client changed the book since the
	// last run. It never enables a delete.
	ForceOverwrite bool
	// ForceVersion accepts a bundle whose exporter version differs from the
	// one the ledger recorded. Never inside the cutover window.
	ForceVersion bool
	// Reverify suppresses the two-read skip: a principal whose digest and
	// synctoken both match is compared card by card anyway, proving the
	// destination really holds what the bundle says. It writes nothing when
	// they agree, and is what `calctl contacts import --resume=false` does.
	Reverify bool
}

// ImportAction is what ImportPrincipal decided to do.
type ImportAction string

const (
	// ActionImported: no ledger row, so principal + book + COPY.
	ActionImported ImportAction = "imported"
	// ActionDiffed: the book already exists from an earlier run and the bundle
	// changed, so each card was compared and only real changes written.
	ActionDiffed ImportAction = "diffed"
	// ActionSkipped: ledger digest and book synctoken both unchanged. Two
	// indexed reads and no writes — the common case on a re-run.
	ActionSkipped ImportAction = "skipped"
	// ActionTokenRefreshed: the user has been using the book but the source is
	// unchanged, so only the ledger's recorded synctoken moves.
	ActionTokenRefreshed ImportAction = "synctoken-refreshed"
	// ActionRefused: a policy stop. Reported, never fatal to the run.
	ActionRefused ImportAction = "refused"
)

// ImportResult is one principal's outcome. A refusal is a result, not an error:
// the run reports it and moves to the next principal.
type ImportResult struct {
	Principal        string       `json:"principal"`
	Action           ImportAction `json:"action"`
	Reason           string       `json:"reason,omitempty"`
	BookID           int64        `json:"bookId,omitempty"`
	PrincipalCreated bool         `json:"principalCreated,omitempty"`
	BookCreated      bool         `json:"bookCreated,omitempty"`
	CardsTotal       int          `json:"cardsTotal"`
	CardsCreated     int          `json:"cardsCreated"`
	CardsUpdated     int          `json:"cardsUpdated"`
	CardsSkipped     int          `json:"cardsSkipped"`
	FrecencyRows     int          `json:"frecencyRows"`
	SyncToken        int64        `json:"syncToken,omitempty"`
	// Orphans are cards in the book that the bundle no longer carries. The
	// importer has no delete path, so these are reported and left: it cannot
	// tell "the user deleted this in calclient yesterday" from "this was
	// tombstoned in SnappyMail after the export", and guessing wrong in one
	// direction destroys user data. Acting on the list is a human decision.
	Orphans []string `json:"orphans,omitempty"`
	// Overwritten are cards the diff rewrote. Under --force-overwrite these are
	// edits a DAV client made since the last run that the bundle just won over,
	// so they are listed individually rather than counted.
	Overwritten []string `json:"overwritten,omitempty"`
	// Created lists, under --force-overwrite only, the cards the diff inserted
	// into a book an earlier run had already filled. Each is either a contact
	// the user added in webmail since that run or one they deleted on a DAV
	// client that the bundle has just brought back — the importer cannot tell
	// which, so the operator gets the uris and decides. On a fresh COPY or an
	// ordinary diff the count is enough.
	Created []string `json:"created,omitempty"`
	// Elsewhere lists the bundle cards whose uri already exists in another of
	// the principal's address books, which the importer left out. SnappyMail's
	// own CardDAV sync wrote webmail contacts to a DAV book under their own
	// UID, so a card with that UID in the user's other book IS that contact,
	// and importing it again would show every such contact twice on every
	// phone. The match is exact — same uri, same principal — and never a guess
	// on name or address, and nothing is merged: the user's copy stays as it is.
	Elsewhere []string `json:"elsewhere,omitempty"`
}

// ledgerRow is the contact_import row for one principal.
type ledgerRow struct {
	BookID      int64
	BookURI     string
	Digest      string
	ExporterVer string
	SyncToken   int64
}

// ImportPrincipal imports one principal, entirely or not at all.
//
// Principal row, book row, every card, the frecency seed and the ledger row
// commit together, so a crash is a non-event: a principal is either fully
// imported with a ledger row to prove it, or absent. There is no state where
// cards exist without a ledger row saying so, which is the state that would
// make a re-run ambiguous.
func (c *ContactImport) ImportPrincipal(ctx context.Context, req ImportRequest) (ImportResult, error) {
	res := ImportResult{Principal: req.Principal, CardsTotal: len(req.Cards)}

	if reason := validateRequest(&req, c.maxCardBytes()); reason != "" {
		res.Action, res.Reason = ActionRefused, reason
		return res, nil
	}
	principalURI := "principals/" + req.Principal

	// One dedicated connection: the COPY below reaches pgx through Conn.Raw,
	// which needs the connection the transaction is running on.
	conn, err := c.DB.Conn(ctx)
	if err != nil {
		return res, err
	}
	defer conn.Close()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	// Serialize this principal against any other worker or run. Held to commit.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, principalURI); err != nil {
		return res, fmt.Errorf("locking %s: %w", principalURI, err)
	}

	ledger, err := readLedger(ctx, tx, principalURI)
	if err != nil {
		return res, err
	}

	if ledger != nil {
		// A serializer change makes every digest differ for reasons that are
		// not data, so it is its own refusal rather than folded into the table
		// below: "trust a bundle from a different serializer" and "overwrite
		// what a client wrote since" are unrelated decisions.
		if ledger.ExporterVer != req.ExporterVer && !req.ForceVersion {
			res.Action = ActionRefused
			res.Reason = fmt.Sprintf("bundle exporter %q differs from the imported %q; re-export or pass --force-version",
				req.ExporterVer, ledger.ExporterVer)
			return res, nil
		}
		if ledger.BookURI != req.BookURI {
			// The ledger names the book this principal's cards live in. Writing
			// into it under a different --book-uri and then recording the new
			// uri would leave the ledger describing a book that holds nothing.
			res.Action = ActionRefused
			res.Reason = fmt.Sprintf("ledger says this principal's cards are in address book %q, but this run was given --book-uri %q",
				ledger.BookURI, req.BookURI)
			return res, nil
		}
		res.BookID = ledger.BookID

		token, found, err := bookSyncToken(ctx, tx, ledger.BookID)
		if err != nil {
			return res, err
		}
		if !found {
			res.Action = ActionRefused
			res.Reason = fmt.Sprintf("ledger says address book %d holds this principal's cards, but it no longer exists", ledger.BookID)
			return res, nil
		}
		// The fast path, and the whole point of the ledger: an unchanged
		// re-run costs the ledger read plus this one and stops here, before
		// the tombstone and principal checks, which only matter to a write.
		if token == ledger.SyncToken && ledger.Digest == req.Digest && !req.Reverify {
			res.Action, res.SyncToken = ActionSkipped, token
			res.CardsSkipped = len(req.Cards)
			return res, tx.Commit()
		}
		res.SyncToken = token
	}

	// Everything past here may write.
	tombstoned, err := principalTombstoned(ctx, tx, principalURI)
	if err != nil {
		return res, err
	}
	if tombstoned {
		res.Action = ActionRefused
		res.Reason = "principal is soft-deleted and pending purge; restore it first or leave it out of the run"
		return res, nil
	}

	created, err := ensurePrincipalRows(ctx, tx, principalURI, req.Principal)
	if err != nil {
		return res, err
	}
	res.PrincipalCreated = created

	// Leave out the cards the user already has in another book (see
	// ImportResult.Elsewhere). Applied on every run, not only the first: a
	// card skipped here must stay skipped when the diff runs later, or the
	// second run would bring the twin the first run avoided.
	if req.Cards, res.Elsewhere, err = withoutCardsElsewhere(ctx, tx, principalURI, req.BookURI, req.Cards); err != nil {
		return res, err
	}

	if ledger == nil {
		// No ledger row, so any book under this uri is the user's own — a
		// calclient user who happened to name a book "webmail". Refuse rather
		// than merge into it; the ledger row IS the proof of ownership.
		_, found, err := bookByURI(ctx, tx, principalURI, req.BookURI)
		if err != nil {
			return res, err
		}
		if found {
			res.Action = ActionRefused
			res.Reason = fmt.Sprintf("address book %q already exists for this principal and no import created it", req.BookURI)
			return res, nil
		}
		id, err := createBook(ctx, tx, principalURI, req.BookURI, req.BookName, req.BookDesc)
		if err != nil {
			return res, err
		}
		res.BookID, res.BookCreated = id, true
	}

	// Every case below the first dereferences ledger, which is safe only because
	// BookCreated and ledger == nil are the same condition here: a nil ledger
	// either created the book just above or already returned a refusal. Go stops
	// at the first true case, so the later ones are never evaluated when ledger
	// is nil. Keep BookCreated first if this switch is ever rearranged.
	switch {
	case res.BookCreated:
		// No client can hold a sync token for a book that did not exist a
		// moment ago, so there is nothing to notify: COPY the cards, leave
		// synctoken at the 1 the insert set, write zero change rows. A client's
		// first sync sends an empty token, which returns every card as Added.
		if err := c.copyCards(ctx, conn, res.BookID, req); err != nil {
			return res, err
		}
		res.Action, res.CardsCreated = ActionImported, len(req.Cards)

	case ledger.Digest == req.Digest && res.SyncToken == ledger.SyncToken:
		// Only reachable under Reverify, since the fast path above took this
		// case otherwise. Prove the cards match rather than assume it.
		if err := c.diffCards(ctx, tx, res.BookID, req, &res); err != nil {
			return res, err
		}
		res.Action = ActionDiffed

	case ledger.Digest == req.Digest:
		// Source unchanged, destination moved: the user has been using the
		// book. Nothing to do but record where the token got to, so the next
		// run's comparison is against reality.
		res.Action = ActionTokenRefreshed
		res.CardsSkipped = len(req.Cards)

	case res.SyncToken != ledger.SyncToken && !req.ForceOverwrite:
		res.Action = ActionRefused
		res.Reason = fmt.Sprintf("bundle changed and a DAV client changed address book %d since the last import (synctoken %d → %d); pass --force-overwrite to let the bundle win",
			ledger.BookID, ledger.SyncToken, res.SyncToken)
		return res, nil

	default:
		// Source changed on a book an earlier run created. COPY is illegal now
		// — a client may hold a sync token, and ChangesForAddressBook has no
		// token-gap detection, so a bulk insert without change rows is silently
		// invisible to it forever. Diff card by card instead, which keeps the
		// change log honest.
		if err := c.diffCards(ctx, tx, res.BookID, req, &res); err != nil {
			return res, err
		}
		res.Action = ActionDiffed
	}

	n, err := seedFrecency(ctx, tx, principalURI, req.Frecency)
	if err != nil {
		return res, err
	}
	res.FrecencyRows = n

	token, _, err := bookSyncToken(ctx, tx, res.BookID)
	if err != nil {
		return res, err
	}
	res.SyncToken = token

	if err := writeLedger(ctx, tx, principalURI, req, res); err != nil {
		return res, err
	}

	if req.DryRun {
		// Roll back deliberately: res already describes what would have
		// happened, which is the only output --dry-run owes the operator.
		return res, tx.Rollback()
	}
	return res, tx.Commit()
}

func (c *ContactImport) maxCardBytes() int {
	if c.MaxCardBytes > 0 {
		return c.MaxCardBytes
	}
	return DefaultMaxCardBytes
}

// validateRequest checks the things a bad bundle could get wrong that would
// otherwise fail deep inside a transaction or, worse, succeed wrongly. It
// returns a refusal reason, or "" when the request is sound.
func validateRequest(req *ImportRequest, maxCard int) string {
	switch {
	case req.Principal == "":
		return "empty principal"
	case req.Principal != strings.ToLower(req.Principal):
		// The login path builds principals/<lowercased email>. An import that
		// creates principals/User@Domain against a login producing
		// principals/user@domain yields two principals and an address book the
		// user never sees, with no error anywhere. The exporter lowercases;
		// this asserts rather than trusts.
		return fmt.Sprintf("principal %q is not lowercased", req.Principal)
	case strings.ContainsAny(req.Principal, "/\n\r "):
		return fmt.Sprintf("principal %q contains a path or whitespace character", req.Principal)
	case strings.Count(req.Principal, "@") != 1:
		// Also rejects rcptd's user@domain@MASTER form, which would mint a junk
		// principal out of a submission credential.
		return fmt.Sprintf("principal %q is not a single email address", req.Principal)
	case req.BookURI == "" || strings.ContainsAny(req.BookURI, "/\n\r"):
		return fmt.Sprintf("invalid address book uri %q", req.BookURI)
	case req.Digest == "":
		return "bundle record has no digest"
	case req.ExporterVer == "":
		return "bundle record has no exporter version"
	case req.RunID == "":
		return "no run id"
	}

	seen := make(map[string]bool, len(req.Cards))
	for _, card := range req.Cards {
		switch {
		case card.URI == "":
			return "a card has an empty uri"
		case len(card.URI) > 200:
			return fmt.Sprintf("card uri %q exceeds cards.uri VARCHAR(200)", card.URI)
		case !safePathSegment(card.URI):
			// The uri becomes one segment of a DAV path. A slash makes the
			// card unreachable from every client, whitespace and control
			// characters break URL handling in one client or another, and a
			// leading dot is a traversal or a hidden file waiting to happen.
			// The exporter derives a uid for these; this catches a bundle
			// that did not.
			return fmt.Sprintf("card uri %q is not a safe path segment", card.URI)
		case seen[card.URI]:
			// cards_ukey would reject this mid-COPY; catching it here names the
			// uri instead of surfacing a constraint violation.
			return fmt.Sprintf("card uri %q appears twice in the bundle", card.URI)
		case len(card.Data) > maxCard:
			return fmt.Sprintf("card %q is %d bytes, over the %d-byte limit", card.URI, len(card.Data), maxCard)
		case !bytes.HasPrefix(card.Data, []byte("BEGIN:VCARD")),
			!bytes.Contains(card.Data, []byte("END:VCARD")):
			// The only inspection of card bytes anywhere in the importer, and
			// deliberately a sniff rather than a parse (invariant 4).
			return fmt.Sprintf("card %q is not a vCard", card.URI)
		}
		seen[card.URI] = true
	}
	return ""
}

// SafePathSegment reports whether s can stand as one segment of a DAV path:
// no separators, no whitespace or control characters, not dot-led. Shared by
// the importer's bundle validation and the contacts API's create path, so a
// card uri that would be unreachable over DAV is refused at both doors.
func SafePathSegment(s string) bool { return safePathSegment(s) }

// safePathSegment reports whether s can stand as one segment of a DAV path:
// no separators, no whitespace or control characters, not dot-led.
func safePathSegment(s string) bool {
	if s == "" || strings.ContainsAny(s, "/\\") || strings.HasPrefix(s, ".") {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func readLedger(ctx context.Context, tx *sql.Tx, principalURI string) (*ledgerRow, error) {
	var row ledgerRow
	err := tx.QueryRowContext(ctx, `
SELECT book_id, book_uri, bundle_digest, exporter_ver, synctoken
  FROM contact_import WHERE principal = $1`, principalURI).
		Scan(&row.BookID, &row.BookURI, &row.Digest, &row.ExporterVer, &row.SyncToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading ledger for %s: %w", principalURI, err)
	}
	// bundle_digest is CHAR(64) and comes back blank-padded on some drivers.
	row.Digest = strings.TrimSpace(row.Digest)
	return &row, nil
}

// withoutCardsElsewhere splits cards into those to import and the uris of
// those the principal already holds in an address book other than bookURI.
func withoutCardsElsewhere(ctx context.Context, tx *sql.Tx, principalURI, bookURI string, cards []ImportCard) ([]ImportCard, []string, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT c.uri
  FROM cards c JOIN addressbooks a ON a.id = c.addressbookid
 WHERE a.principaluri = $1 AND a.uri <> $2`, principalURI, bookURI)
	if err != nil {
		return nil, nil, fmt.Errorf("listing cards in other books: %w", err)
	}
	defer rows.Close()
	taken := map[string]bool{}
	for rows.Next() {
		var uri string
		if err := rows.Scan(&uri); err != nil {
			return nil, nil, err
		}
		taken[uri] = true
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(taken) == 0 {
		return cards, nil, nil
	}
	keep := cards[:0:0]
	var elsewhere []string
	for _, card := range cards {
		if taken[card.URI] {
			elsewhere = append(elsewhere, card.URI)
			continue
		}
		keep = append(keep, card)
	}
	return keep, elsewhere, nil
}

// bookSyncToken reads the book's synctoken and locks the row to commit. The
// lock is what makes the ledger comparison mean anything: without it a DAV
// client could write between this read and the diff, and the import would
// overwrite that edit believing the book untouched since the last run.
// Client writes lock the same row (recordChange), so they wait for the import.
func bookSyncToken(ctx context.Context, tx *sql.Tx, bookID int64) (int64, bool, error) {
	var token int64
	err := tx.QueryRowContext(ctx,
		`SELECT synctoken FROM addressbooks WHERE id = $1 FOR UPDATE`, bookID).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return token, err == nil, err
}

func bookByURI(ctx context.Context, tx *sql.Tx, principalURI, uri string) (int64, bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM addressbooks WHERE principaluri = $1 AND uri = $2`, principalURI, uri).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

func principalTombstoned(ctx context.Context, tx *sql.Tx, principalURI string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM deleted_principals WHERE principal = $1)`, principalURI).Scan(&exists)
	return exists, err
}

// ensurePrincipalRows creates the principal triple when the address has never
// logged in, and reports whether it did. This is provisioner.ensure's logic
// without the process-local memo — the importer cannot rely on logins, because
// most users will not touch webmail between migration and cutover and the
// import would be writing into principals that do not exist.
//
// It deliberately stores BlockedDigestA1 rather than a real digest: with rcptd
// authenticating, users.digesta1 is dead weight, and bulk-importing 110k digest
// credentials for invented passwords is the wrong default.
func ensurePrincipalRows(ctx context.Context, tx *sql.Tx, principalURI, username string) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM principals WHERE uri = $1)`, principalURI).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if err := createUserRows(tx, username, BlockedDigestA1(), username, username); err != nil {
		return false, fmt.Errorf("creating %s: %w", principalURI, err)
	}
	return true, nil
}

func createBook(ctx context.Context, tx *sql.Tx, principalURI, uri, name, description string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO addressbooks (principaluri, uri, displayname, description, synctoken)
VALUES ($1, $2, nullif($3, ''), nullif($4, ''), 1) RETURNING id`,
		principalURI, uri, name, description).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("creating address book %s/%s: %w", principalURI, uri, err)
	}
	return id, nil
}

// copyCards bulk-loads a freshly created book. The importer fills every column
// itself, including lastmodified from the source contact rather than now(), so
// clients get a truthful timestamp and the row stays a pure function of the
// bundle.
// conn must be the connection the caller's transaction is open on: copyInto
// reaches pgx through it, which is what puts the COPY inside that transaction.
func (c *ContactImport) copyCards(ctx context.Context, conn *sql.Conn, bookID int64, req ImportRequest) error {
	if len(req.Cards) == 0 {
		return nil
	}
	rows := make([][]any, 0, len(req.Cards))
	for _, card := range req.Cards {
		mtime := card.LastModified
		if mtime <= 0 {
			mtime = req.ExportEpoch
		}
		rows = append(rows, []any{
			bookID, card.URI, card.Data, mtime, cardETag(card.Data), len(card.Data),
		})
	}
	_, err := copyInto(ctx, conn, "cards",
		[]string{"addressbookid", "uri", "carddata", "lastmodified", "etag", "size"}, rows)
	return err
}

// diffCards is the re-run path: compare, then write only what genuinely
// changed. A blind re-upsert would produce correct rows and still be wrong —
// CreateCard and UpdateCard each call recordChange, which locks the book row,
// inserts a change row, bumps the synctoken and fires pg_notify. Re-running
// blind is millions of change rows and a push notification per card telling
// every device to re-download everything.
func (c *ContactImport) diffCards(ctx context.Context, tx *sql.Tx, bookID int64, req ImportRequest, res *ImportResult) error {
	existing, err := cardETags(ctx, tx, bookID)
	if err != nil {
		return err
	}
	for _, card := range req.Cards {
		mtime := card.LastModified
		if mtime <= 0 {
			mtime = req.ExportEpoch
		}
		etag := cardETag(card.Data)
		current, present := existing[card.URI]
		switch {
		case !present:
			if _, err := tx.ExecContext(ctx, `
INSERT INTO cards (addressbookid, uri, carddata, lastmodified, etag, size)
VALUES ($1, $2, $3, $4, $5, $6)`,
				bookID, card.URI, card.Data, mtime, etag, len(card.Data)); err != nil {
				return fmt.Errorf("inserting card %s: %w", card.URI, err)
			}
			if err := recordChange(tx, addressBookLog, strconv.FormatInt(bookID, 10), card.URI, 1); err != nil {
				return err
			}
			res.CardsCreated++
			if req.ForceOverwrite {
				// Same reasoning as Overwritten below: the operator asked the
				// bundle to win over a client's changes, and a create here may
				// be a card that client deleted. Name it.
				res.Created = append(res.Created, card.URI)
			}
		case current == etag:
			res.CardsSkipped++
		default:
			if _, err := tx.ExecContext(ctx, `
UPDATE cards SET carddata = $1, lastmodified = $2, etag = $3, size = $4
 WHERE addressbookid = $5 AND uri = $6`,
				card.Data, mtime, etag, len(card.Data), bookID, card.URI); err != nil {
				return fmt.Errorf("updating card %s: %w", card.URI, err)
			}
			if err := recordChange(tx, addressBookLog, strconv.FormatInt(bookID, 10), card.URI, 2); err != nil {
				return err
			}
			res.CardsUpdated++
			if req.ForceOverwrite {
				// Only under --force-overwrite, where these are edits a DAV
				// client made that the bundle just won over and the operator
				// needs them named. On an ordinary diff the bundle is simply
				// newer than what is stored, CardsUpdated says how many, and
				// listing every uri of a 20,782-card book helps nobody.
				res.Overwritten = append(res.Overwritten, card.URI)
			}
		}
		delete(existing, card.URI)
	}
	// Whatever is left is in calserver and not in the bundle: the exact
	// delete-candidate set. Reported, never acted on.
	for uri := range existing {
		res.Orphans = append(res.Orphans, uri)
	}
	sort.Strings(res.Orphans)
	return nil
}

// cardETags reads the book's uri → etag map. The column stores the etag bare
// while scanCard hands it out wrapped in double quotes, and comparing the
// quoted form against a raw md5 never matches — the symptom being a re-run that
// rewrites every card, which looks exactly like a determinism bug and is not
// one. Reading the column directly sidesteps the trap.
func cardETags(ctx context.Context, tx *sql.Tx, bookID int64) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT uri, etag FROM cards WHERE addressbookid = $1`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uri string
		var etag sql.NullString
		if err := rows.Scan(&uri, &etag); err != nil {
			return nil, err
		}
		out[uri] = etag.String
	}
	return out, rows.Err()
}

// seedFrecency writes the migrated usage counts. It pre-aggregates per
// lowercased address — SnappyMail counts `Bob@x` and `bob@x` separately, and
// the destination keys on the lowercased form — and writes into seed_frec,
// which the importer owns outright. That is what makes the seed idempotent: a
// summing upsert would double the counts on every re-run, while replacing a
// column nothing else writes cannot.
func seedFrecency(ctx context.Context, tx *sql.Tx, principalURI string, seeds []SeedFrecency) (int, error) {
	if len(seeds) == 0 {
		return 0, nil
	}
	type agg struct {
		name  string
		best  int // count of the row the name came from
		total int
	}
	merged := map[string]*agg{}
	for _, s := range seeds {
		email := strings.ToLower(strings.TrimSpace(s.Email))
		if email == "" || s.Count <= 0 {
			continue
		}
		if utf8.RuneCountInString(email) > frecencyEmailMax {
			// Nothing over 320 characters is an address anyone will type into
			// a To: field; a truncated form would not match one either. Drop
			// it rather than store a suggestion that can never fire.
			continue
		}
		s.Name = truncateRunes(strings.TrimSpace(s.Name), frecencyNameMax)
		a := merged[email]
		if a == nil {
			a = &agg{}
			merged[email] = a
		}
		a.total += s.Count
		// The name comes from the most-used row, so a correspondent named once
		// and mailed twice under a blank name keeps the name.
		if s.Name != "" && (a.name == "" || s.Count > a.best) {
			a.name, a.best = s.Name, s.Count
		}
	}
	if len(merged) == 0 {
		return 0, nil
	}
	emails := make([]string, 0, len(merged))
	for email := range merged {
		emails = append(emails, email)
	}
	sort.Strings(emails) // deterministic write order, so a re-run is byte-identical

	const chunk = 500
	for start := 0; start < len(emails); start += chunk {
		end := min(start+chunk, len(emails))
		var sb strings.Builder
		args := []any{principalURI}
		sb.WriteString(`INSERT INTO contact_frecency (principal, email, name, seed_frec) VALUES `)
		for i, email := range emails[start:end] {
			if i > 0 {
				sb.WriteString(", ")
			}
			a := merged[email]
			args = append(args, email, a.name, a.total)
			fmt.Fprintf(&sb, "($1, $%d, nullif($%d, ''), $%d)", len(args)-2, len(args)-1, len(args))
		}
		sb.WriteString(` ON CONFLICT (principal, email) DO UPDATE
   SET seed_frec = excluded.seed_frec,
       name      = coalesce(excluded.name, contact_frecency.name)`)
		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return 0, fmt.Errorf("seeding frecency for %s: %w", principalURI, err)
		}
	}
	return len(merged), nil
}

// truncateRunes cuts s to at most n characters (not bytes — VARCHAR(n) counts
// characters), never splitting a multi-byte sequence.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

func writeLedger(ctx context.Context, tx *sql.Tx, principalURI string, req ImportRequest, res ImportResult) error {
	// source_users goes in as a pgx-encoded array, cast so the column type is
	// decided here; non-nil, so no source ids is '{}' rather than NULL.
	//
	// cards_written accumulates across runs — it answers "how many cards has
	// the migration ever written for this principal", which a re-run that
	// writes nothing must not reset to zero. cards_total, cards_skipped and
	// the rest describe the last visit.
	ids := append([]int32{}, req.SourceUsers...)
	_, err := tx.ExecContext(ctx, `
INSERT INTO contact_import (principal, source_users, book_id, book_uri, bundle_digest,
                            exporter_ver, cards_total, cards_written, cards_skipped,
                            synctoken, run_id, imported_at)
VALUES ($1, $2::int[], $3, $4, $5, $6, $7, $8, $9, $10, $11, now())
ON CONFLICT (principal) DO UPDATE
   SET source_users  = excluded.source_users,
       book_id       = excluded.book_id,
       book_uri      = excluded.book_uri,
       bundle_digest = excluded.bundle_digest,
       exporter_ver  = excluded.exporter_ver,
       cards_total   = excluded.cards_total,
       cards_written = contact_import.cards_written + excluded.cards_written,
       cards_skipped = excluded.cards_skipped,
       synctoken     = excluded.synctoken,
       run_id        = excluded.run_id,
       imported_at   = excluded.imported_at`,
		principalURI, ids, res.BookID, req.BookURI, req.Digest,
		req.ExporterVer, res.CardsTotal, res.CardsCreated+res.CardsUpdated, res.CardsSkipped,
		res.SyncToken, req.RunID)
	if err != nil {
		return fmt.Errorf("writing ledger for %s: %w", principalURI, err)
	}
	return nil
}

// ImportSummary totals one run, for `calctl contacts import` and for the
// after-the-fact question of what a run did.
type ImportSummary struct {
	RunID      string `json:"runId"`
	Principals int    `json:"principals"`
	Cards      int    `json:"cards"`
	Written    int    `json:"written"`
	Skipped    int    `json:"skipped"`
}

// Summary reads a run's totals back out of the ledger. An empty runID totals
// every run, which is what "how much of the corpus has landed" asks.
func (c *ContactImport) Summary(ctx context.Context, runID string) (ImportSummary, error) {
	out := ImportSummary{RunID: runID}
	query := `
SELECT count(*), coalesce(sum(cards_total), 0), coalesce(sum(cards_written), 0),
       coalesce(sum(cards_skipped), 0)
  FROM contact_import`
	var args []any
	if runID != "" {
		query += ` WHERE run_id = $1`
		args = append(args, runID)
	}
	err := c.DB.QueryRowContext(ctx, query, args...).
		Scan(&out.Principals, &out.Cards, &out.Written, &out.Skipped)
	return out, err
}

// OrphanBooks lists address books whose principal does not exist. There is no
// foreign key on addressbooks.principaluri, so an orphan is silent: webmail
// serves it correctly while DAV discovery 404s and the user sees nothing on
// their phone. Worth running at the end of every import.
func (c *ContactImport) OrphanBooks(ctx context.Context) ([]string, error) {
	rows, err := c.DB.QueryContext(ctx, `
SELECT DISTINCT a.principaluri FROM addressbooks a
  LEFT JOIN principals p ON p.uri = a.principaluri
 WHERE p.uri IS NULL ORDER BY 1`)
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

// NewRunID mints the uuid that ties one import run's ledger rows together.
func NewRunID() (string, error) { return newUUID() }
