package contactsapi

import (
	"net/http"
	"net/mail"
	"sort"
	"strconv"
	"strings"

	"calendsd/internal/storage"
)

// suggestions is GetSuggestions: ranked {email, name} pairs for the address
// autocomplete, from two sources united —
//
//   - contact_frecency, indexed, ranked by how often the user actually mailed
//     the address (the migrated prop_frec seed plus live IncFrec counts);
//   - the user's cards, scanned in Go, for "someone I have never written to"
//     — but only while the book is small enough that a per-keystroke scan is
//     free. Above SuggestionScanLimit cards the frecency table answers alone,
//     which is the better ranking source anyway, and a content search stays
//     available as a deliberate GetContacts call.
//
// A card-derived pair carries the card's display name; a frecency-only pair
// carries whatever name IncFrec parsed out of the address. When both know the
// same address the card's name wins and the frecency score ranks it.
func (h *Handler) suggestions(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	principal := principalOf(r)

	// Over-fetch from the table so the union has room to re-rank.
	ranked, cards, err := h.contacts.SuggestionSources(r.Context(), principal, q, limit*3, h.opts.SuggestionScanLimit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	byEmail := map[string]*storage.Suggestion{}
	var out []*storage.Suggestion
	for i := range ranked {
		s := &ranked[i]
		byEmail[strings.ToLower(s.Email)] = s
		out = append(out, s)
	}

	for _, c := range cards {
		p, ok := parseCard(c.Data)
		if !ok || !p.matches(q) {
			continue
		}
		for _, email := range p.Emails {
			key := strings.ToLower(email)
			if s, seen := byEmail[key]; seen {
				if p.FN != "" {
					s.Name = p.FN
				}
				continue
			}
			s := &storage.Suggestion{Email: email, Name: p.FN}
			byEmail[key] = s
			out = append(out, s)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		ni, nj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if ni != nj {
			return ni < nj
		}
		return out[i].Email < out[j].Email
	})
	if len(out) > limit {
		out = out[:limit]
	}
	result := make([]storage.Suggestion, 0, len(out))
	for _, s := range out {
		result = append(result, *s)
	}
	writeJSON(w, http.StatusOK, result)
}

// bumpFrecency is IncFrec. It takes what SnappyMail hands its driver — a flat
// array of address strings, "Name <addr>" or bare — and parses the display
// name out the way PdoAddressBook does, so a collected address is never a
// nameless suggestion later. It writes frecency rows only, never a card:
// auto-created contacts polluting the real address book is a long-standing
// RainLoop annoyance, and worse once that book syncs to everyone's phone.
func (h *Handler) bumpFrecency(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Addresses []string `json:"addresses"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	var entries []storage.FrecencyEntry
	for _, raw := range body.Addresses {
		if e, ok := parseAddress(raw); ok {
			entries = append(entries, e)
		}
	}
	n, err := h.frecency.Bump(r.Context(), principalOf(r), entries)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"updated": n})
}

// parseAddress splits "Name <addr>" (or a bare address) into its parts. An
// address that does not parse but still looks like one is kept bare, since
// a lost frecency count is worse than a missing display name.
func parseAddress(raw string) (storage.FrecencyEntry, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return storage.FrecencyEntry{}, false
	}
	if a, err := mail.ParseAddress(raw); err == nil {
		return storage.FrecencyEntry{Email: a.Address, Name: a.Name}, true
	}
	if strings.Contains(raw, "@") && !strings.ContainsAny(raw, " <>") {
		return storage.FrecencyEntry{Email: raw}, true
	}
	return storage.FrecencyEntry{}, false
}
