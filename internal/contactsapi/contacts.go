package contactsapi

import (
	"encoding/xml"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"calserver/internal/carddav"
	"calserver/internal/storage"
)

// contact is the wire form of one card. id is the cards row's serial — a
// number, because SnappyMail's own action layer runs contact ids through
// intval() before any driver sees them (Actions\Contacts::DoContactsDelete
// filters a non-numeric id away silently). book and uri say where the row
// lives, which stays debuggable in a log line. etag is the bare md5 the column
// stores; the client sends it back in If-Match on update.
type contact struct {
	ID      int64  `json:"id"`
	Book    string `json:"book"`
	URI     string `json:"uri"`
	UID     string `json:"uid"`
	ETag    string `json:"etag"`
	Changed int64  `json:"changed"`
	VCard   string `json:"vcard"`
}

func toContact(c storage.ContactCard) contact {
	uid := strings.TrimSuffix(c.URI, ".vcf")
	if p, ok := parseCard(c.Data); ok && p.UID != "" {
		uid = p.UID
	}
	return contact{
		ID:      c.ID,
		Book:    c.BookURI,
		URI:     c.URI,
		UID:     uid,
		ETag:    c.ETag,
		Changed: c.LastModified,
		VCard:   string(c.Data),
	}
}

// parseID reads a contact id off the path. Anything that is not a positive
// integer is a 400, not a 404: it is a malformed request, and the distinction
// matters to a plugin author debugging what they sent.
func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return id, err == nil && id > 0
}

// --- reads -------------------------------------------------------------------

// list is GetContacts: an offset/limit/search window over every card the
// principal owns, across all books, with the total for the pager. Sorted by
// display name, case-insensitively, then by id so the order is stable.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 5000 {
		limit = 5000
	}
	search := strings.ToLower(strings.TrimSpace(q.Get("search")))

	cards, err := h.contacts.Cards(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	type row struct {
		sortKey string
		card    storage.ContactCard
	}
	var rows []row
	for _, c := range cards {
		p, ok := parseCard(c.Data)
		if !ok {
			// A card go-vcard cannot decode is still the user's; it is listed
			// (under its uri) when there is no search, and skipped by a search
			// since nothing in it can be matched.
			if search != "" {
				continue
			}
			rows = append(rows, row{sortKey: strings.ToLower(c.URI), card: c})
			continue
		}
		if !p.matches(search) {
			continue
		}
		rows = append(rows, row{sortKey: strings.ToLower(p.FN), card: c})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].sortKey != rows[j].sortKey {
			return rows[i].sortKey < rows[j].sortKey
		}
		return rows[i].card.BookURI+"/"+rows[i].card.URI < rows[j].card.BookURI+"/"+rows[j].card.URI
	})
	total := len(rows)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	out := make([]contact, 0, end-offset)
	for _, rw := range rows[offset:end] {
		out = append(out, toContact(rw.card))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "contacts": out})
}

// get is GetContactByID for a numeric id.
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return
	}
	c, err := h.contacts.CardByID(r.Context(), principalOf(r), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound, "no such contact")
		return
	}
	writeJSON(w, http.StatusOK, toContact(*c))
}

// byUID is GetContactByID with $bIsStrID: lookup by vCard UID. Both the
// migration and this API name a card <uid>.vcf, so the direct uri lookup
// answers almost every call in one indexed read; the scan behind it covers a
// card some other client named differently.
func (h *Handler) byUID(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeError(w, http.StatusBadRequest, "empty uid")
		return
	}
	principal := principalOf(r)
	c, err := h.contacts.CardByURI(r.Context(), principal, uid+".vcf", h.opts.DefaultBookURI)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if c == nil {
		cards, err := h.contacts.Cards(r.Context(), principal)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		for i := range cards {
			if p, ok := parseCard(cards[i].Data); ok && p.UID == uid {
				c = &cards[i]
				break
			}
		}
	}
	if c == nil {
		writeError(w, http.StatusNotFound, "no such contact")
		return
	}
	writeJSON(w, http.StatusOK, toContact(*c))
}

// byEmail is GetContactByEmail: the first card carrying the address. In a
// query parameter, not a path segment — "+" and "%" in local parts make path
// encoding a recurring source of bugs.
func (h *Handler) byEmail(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	if email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}
	cards, err := h.contacts.Cards(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	for _, c := range cards {
		if p, ok := parseCard(c.Data); ok && p.hasEmail(email) {
			writeJSON(w, http.StatusOK, toContact(c))
			return
		}
	}
	writeError(w, http.StatusNotFound, "no contact with that address")
}

// --- writes ------------------------------------------------------------------

type cardBody struct {
	VCard string `json:"vcard"`
}

// readCard takes the vCard out of a request body, normalizes its line endings
// and parses it far enough to know its UID, which must be present. Whether
// the UID is usable as a file name is create's question alone: on update the
// file name already exists, and a phone may have given a card a UID with a
// space in it or one 300 characters long (some Exchange ones are) that the
// user must still be able to edit.
func (h *Handler) readCard(w http.ResponseWriter, r *http.Request) ([]byte, parsedCard, bool) {
	var body cardBody
	if !readJSON(w, r, &body) {
		return nil, parsedCard{}, false
	}
	data := normalizeCRLF([]byte(strings.TrimSpace(body.VCard) + "\n"))
	p, err := parseSubmittedCard(data)
	switch {
	case errors.Is(err, carddav.ErrCardTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "vcard is too large")
	case errors.Is(err, carddav.ErrNotSingleCard):
		writeError(w, http.StatusBadRequest, "vcard must contain exactly one card")
	case errors.Is(err, carddav.ErrNoUID):
		writeError(w, http.StatusBadRequest, "vcard has no UID")
	case err != nil:
		writeError(w, http.StatusBadRequest, "vcard does not parse: "+err.Error())
	default:
		return data, p, true
	}
	return nil, parsedCard{}, false
}

// createTarget resolves the book a new card lands in: the default (webmail)
// when it exists, else the sole book, else a freshly created default. That
// last branch covers a user who was never migrated and has never touched
// DAV: their principal exists (auth provisioned it) but they have zero books,
// and a create must have somewhere to go. With create=false nothing is
// created and ok=false says there is no target — what DeleteAllContacts
// wants, since it must not conjure a book to empty it.
func (h *Handler) createTarget(r *http.Request, create bool) (storage.AddressBook, bool, error) {
	principal := principalOf(r)
	books, err := h.contacts.Books(r.Context(), principal)
	if err != nil {
		return storage.AddressBook{}, false, err
	}
	for _, b := range books {
		if b.URI == h.opts.DefaultBookURI {
			return b, true, nil
		}
	}
	if len(books) == 1 {
		return books[0], true, nil
	}
	if !create {
		return storage.AddressBook{}, false, nil
	}
	id, err := h.cards.CreateAddressBook(principal, h.opts.DefaultBookURI, map[xml.Name]any{
		{Space: "DAV:", Local: "displayname"}: h.opts.DefaultBookName,
	})
	if storage.IsUniqueViolation(err) {
		// Two first-ever creates for the same user arrived together and the
		// other one made the book a moment ago; use it.
		books, err := h.contacts.Books(r.Context(), principal)
		if err != nil {
			return storage.AddressBook{}, false, err
		}
		for _, b := range books {
			if b.URI == h.opts.DefaultBookURI {
				return b, true, nil
			}
		}
		return storage.AddressBook{}, false, errors.New("address book vanished after a create conflict")
	}
	if err != nil {
		return storage.AddressBook{}, false, err
	}
	return storage.AddressBook{ID: id, URI: h.opts.DefaultBookURI, DisplayName: h.opts.DefaultBookName}, true, nil
}

// create is ContactSave for a new contact: 201 with the stored contact, 409
// when a card with that UID already exists in the target book.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	data, p, ok := h.readCard(w, r)
	if !ok {
		return
	}
	if !storage.SafePathSegment(p.UID) || len(p.UID) > 190 {
		// The uid becomes the card's DAV path segment; a slash or whitespace
		// in it makes the card unreachable from every other client.
		writeError(w, http.StatusBadRequest, "UID is not usable as a path segment (no slashes, whitespace or control characters, at most 190 characters)")
		return
	}
	book, _, err := h.createTarget(r, true)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// The conflict check is on "<uid>.vcf" in the target book, which is the
	// name this API and the migration give a card. A card with the same UID
	// under a different file name, or in another of the user's books, is not
	// caught: the API never invents such a card, and refusing a create because
	// a phone once named one differently would be a surprising failure for
	// the user. Duplicate UIDs across books are the dedup work's problem.
	//
	// The 409 names the card that is in the way, id and etag, so a client that
	// wants to update instead updates exactly that one. Looking it up by UID
	// afterwards would not do: with the same UID in another book too, a by-uid
	// search can land on the other copy.
	uri := p.UID + ".vcf"
	principal := principalOf(r)
	existing, err := h.contacts.CardByPath(r.Context(), principal, book.URI, uri)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if existing != nil {
		writeConflict(w, *existing)
		return
	}
	if _, err := h.cards.CreateCard(book.ID, uri, data); err != nil {
		if storage.IsUniqueViolation(err) {
			// The check above and this insert are not atomic; a double-click or
			// a plugin retry lands here, and it is the same conflict.
			if existing, err := h.contacts.CardByPath(r.Context(), principal, book.URI, uri); err == nil && existing != nil {
				writeConflict(w, *existing)
				return
			}
			writeError(w, http.StatusConflict, "a contact with that UID already exists; update it instead")
			return
		}
		h.fail(w, r, err)
		return
	}
	stored, err := h.contacts.CardByPath(r.Context(), principal, book.URI, uri)
	if err != nil || stored == nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toContact(*stored))
}

// writeConflict is the 409 for a create whose UID is already taken: the error,
// plus the id and etag of the card in the way, so the client can update that
// exact card with a correct If-Match rather than search for it.
func writeConflict(w http.ResponseWriter, existing storage.ContactCard) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": "a contact with that UID already exists; update it instead",
		"id":    existing.ID,
		"etag":  existing.ETag,
	})
}

// update is ContactSave for an existing contact. If-Match, when present and
// not "*", is enforced inside the UPDATE (storage.UpdateCardIfMatch), so a
// change a phone made thirty seconds ago is refused with 412 rather than
// clobbered. Without If-Match the write is unconditional — the plugin always
// sends the etag it was given, so that path is for tooling.
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return
	}
	data, p, ok := h.readCard(w, r)
	if !ok {
		return
	}
	principal := principalOf(r)
	current, err := h.contacts.CardByID(r.Context(), principal, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if current == nil {
		writeError(w, http.StatusNotFound, "no such contact")
		return
	}
	uri := current.URI
	// The file name is fixed once a card exists, and another client may have
	// named it without regard to the UID (a phone writing ABC123.vcf holding
	// UID:urn:uuid:…). Such a card must stay editable, so the rule here is
	// only that the UID does not CHANGE: a card whose UID moves under its href
	// is a sync bug on every device that reads it.
	if stored, ok := parseCard(current.Data); ok && stored.UID != "" && stored.UID != p.UID {
		writeError(w, http.StatusBadRequest, "the vcard's UID must not change on update")
		return
	}
	// A present-but-empty If-Match is a precondition that can never hold, not
	// an unconditional write: the only way a client gets an empty etag is a
	// bug, and the 412 guarantee must not quietly disappear exactly there.
	_, hasIfMatch := r.Header["If-Match"]
	ifMatch := unquoteETag(r.Header.Get("If-Match"))
	switch {
	case !hasIfMatch, ifMatch == "*":
		if _, err := h.cards.UpdateCard(current.BookID, uri, data); err != nil {
			h.fail(w, r, err)
			return
		}
	default:
		_, matched, err := h.cards.UpdateCardIfMatch(current.BookID, uri, data, ifMatch)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if !matched {
			writeError(w, http.StatusPreconditionFailed, "the contact changed since it was read; reload it and try again")
			return
		}
	}
	stored, err := h.contacts.CardByID(r.Context(), principal, id)
	if err != nil || stored == nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toContact(*stored))
}

// delete removes one contact by id.
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return
	}
	c, err := h.contacts.CardByID(r.Context(), principalOf(r), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound, "no such contact")
		return
	}
	if err := h.cards.DeleteCard(c.BookID, c.URI); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// batchDelete is DeleteContacts: one round trip for a list of ids. Ids that
// do not exist are reported, not errors — the user may have deleted them on
// a phone in the meantime.
func (h *Handler) batchDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	principal := principalOf(r)
	deleted := 0
	missing := []int64{}
	for _, id := range body.IDs {
		if id <= 0 {
			missing = append(missing, id)
			continue
		}
		c, err := h.contacts.CardByID(r.Context(), principal, id)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if c == nil {
			missing = append(missing, id)
			continue
		}
		if err := h.cards.DeleteCard(c.BookID, c.URI); err != nil {
			h.fail(w, r, err)
			return
		}
		deleted++
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "missing": missing})
}

// deleteAll is DeleteAllContacts, and it is scoped on purpose: reads
// aggregate across every book, but this empties only the create target
// (webmail, or the sole book). Following the read rule instead would let one
// click in webmail wipe a second address book the user made in calclient and
// never thought of as webmail's. Requires ?all=1 so a stray DELETE on the
// collection path does nothing.
func (h *Handler) deleteAll(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") != "1" {
		writeError(w, http.StatusBadRequest, "pass ?all=1 to delete every contact in the webmail address book")
		return
	}
	book, ok, err := h.createTarget(r, false)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"deleted": 0, "book": ""})
		return
	}
	n, err := h.cards.DeleteAllCards(book.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n, "book": book.URI})
}
