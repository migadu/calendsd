package carddav

import (
	"bytes"
	"errors"
	"io"
	"strings"

	vcard "github.com/emersion/go-vcard"
)

// MaxCardSize is the largest vCard a client may store, in bytes; address
// books advertise it as {CARDDAV:}max-resource-size.
const MaxCardSize = 10000000

var (
	// ErrCardTooLarge is ValidateCard refusing a body over MaxCardSize.
	ErrCardTooLarge = errors.New("vCard exceeds the maximum resource size")
	// ErrNotSingleCard is ValidateCard refusing a body that is not exactly
	// one vCard: several, or text around the one.
	ErrNotSingleCard = errors.New("body must contain exactly one vCard")
	// ErrNoUID is ValidateCard refusing a card without a UID.
	ErrNoUID = errors.New("vCard has no UID")
)

// ValidateCard checks a vCard body a client submits, for every write path
// (DAV PUT and the contacts API): at most MaxCardSize bytes, exactly one card
// and nothing else, and a UID. Anything else is a decode error. go-vcard's
// decoder stops at the first END:VCARD and skips lines it cannot parse, so
// without the single-card checks text around the card would be stored and
// served raw to every other client, unparsed by any of them.
func ValidateCard(data []byte) (vcard.Card, error) {
	if len(data) > MaxCardSize {
		return nil, ErrCardTooLarge
	}
	body := strings.ToUpper(strings.TrimSpace(string(data)))
	if !strings.HasPrefix(body, "BEGIN:VCARD") || !strings.HasSuffix(body, "END:VCARD") {
		return nil, ErrNotSingleCard
	}
	dec := vcard.NewDecoder(bytes.NewReader(data))
	card, err := dec.Decode()
	if err != nil {
		return nil, err
	}
	if _, err := dec.Decode(); !errors.Is(err, io.EOF) {
		return nil, ErrNotSingleCard
	}
	if strings.TrimSpace(card.Value(vcard.FieldUID)) == "" {
		return nil, ErrNoUID
	}
	return card, nil
}
