package contactsapi

import (
	"bytes"
	"strings"

	vcard "github.com/emersion/go-vcard"

	"calserver/internal/carddav"
)

// parsedCard is what the API needs out of a vCard: enough to search, sort,
// suggest and identify it. Everything else stays in the raw bytes, which are
// what the client gets and what the store keeps (the API never re-serializes
// a card it did not just receive).
type parsedCard struct {
	UID    string
	FN     string   // display name; falls back to N, then the first email
	Emails []string // as written, in card order
	// haystack is the lowercased text a substring search runs over: FN, the N
	// parts, NICKNAME, ORG and every EMAIL.
	haystack string
}

// parseCard decodes vCard bytes. Undecodable data yields ok=false; the caller
// decides whether that card is skipped (search) or served raw (get by id).
func parseCard(data []byte) (parsedCard, bool) {
	card, err := vcard.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return parsedCard{}, false
	}
	return fromCard(card), true
}

// parseSubmittedCard is parseCard for a body a client submits, which must
// pass the same validation as a DAV PUT (carddav.ValidateCard).
func parseSubmittedCard(data []byte) (parsedCard, error) {
	card, err := carddav.ValidateCard(data)
	if err != nil {
		return parsedCard{}, err
	}
	return fromCard(card), nil
}

func fromCard(card vcard.Card) parsedCard {
	p := parsedCard{
		UID:    strings.TrimSpace(card.Value("UID")),
		FN:     strings.TrimSpace(card.Value("FN")),
		Emails: cleanValues(card.Values("EMAIL")),
	}
	var parts []string
	if n := card.Name(); n != nil {
		parts = append(parts, n.GivenName, n.AdditionalName, n.FamilyName, n.HonorificPrefix, n.HonorificSuffix)
	}
	if p.FN == "" {
		p.FN = strings.Join(cleanValues(parts), " ")
	}
	if p.FN == "" && len(p.Emails) > 0 {
		p.FN = p.Emails[0]
	}
	var h []string
	h = append(h, p.FN)
	h = append(h, parts...)
	h = append(h, card.Values("NICKNAME")...)
	h = append(h, card.Values("ORG")...)
	h = append(h, p.Emails...)
	p.haystack = strings.ToLower(strings.Join(cleanValues(h), "\n"))
	return p
}

func cleanValues(in []string) []string {
	var out []string
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// matches reports whether the card contains q (already lowercased) anywhere
// in its searchable text. An empty q matches every card.
func (p parsedCard) matches(q string) bool {
	return q == "" || strings.Contains(p.haystack, q)
}

// hasEmail reports whether the card carries the address, compared
// case-insensitively — addresses are not case-sensitive in practice, and a
// search by address should not miss "Bob@Example.org" for "bob@example.org".
func (p parsedCard) hasEmail(email string) bool {
	for _, e := range p.Emails {
		if strings.EqualFold(e, email) {
			return true
		}
	}
	return false
}

// normalizeCRLF gives a card the line endings vCard requires, the same
// treatment the DAV PUT path applies (carddav.normalizeVCard).
func normalizeCRLF(data []byte) []byte {
	s := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n", "\r\n")
	return []byte(s)
}
