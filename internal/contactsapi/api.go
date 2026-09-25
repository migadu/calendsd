// Package contactsapi is the JSON contacts API webmail talks to
// (docs/snappymail-calserver-contacts.md § The API): a thin per-user layer
// over the same cards rows CardDAV serves, shaped one-to-one after
// SnappyMail's AddressBookInterface so the PHP plugin stays thin.
//
// It is not a second address-book store. A contact saved here appears on a
// phone without a sync step, because there is only one row.
//
// Authentication is Basic with the user's own mailbox credentials, through
// the same auth.Backend the DAV server uses — shared as an instance, not
// re-created, so the rcptd credential cache and first-login provisioning are
// one thing. Authorization is then automatic: the principal comes from the
// credential and every query below carries it, so a handler physically cannot
// read another user's cards.
//
// The listener is a separate loopback port, never a path on the DAV one, so
// none of this is reachable through the public DAV hostname.
package contactsapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"calserver/internal/auth"
	"calserver/internal/storage"
)

// Options tune the API. Zero values take the documented defaults.
type Options struct {
	// MaxBodyBytes caps a request body. The DAV server applies
	// dav.DefaultMaxRequestBodySize (100 MiB) to its own listener; this
	// listener has its own mux and inherits nothing, so it is set here.
	MaxBodyBytes int64
	// DefaultBookURI is where a create lands when the principal has such a
	// book, and what is created for a principal with no book at all. "webmail"
	// — the book the migration lands in — so the plugin's create target is a
	// constant rather than a rule.
	DefaultBookURI  string
	DefaultBookName string
	// SuggestionScanLimit is the card count above which suggestions are served
	// from contact_frecency alone rather than also scanning card content per
	// keystroke. p99 is 446 cards; the largest book is 20,782, which would cost
	// ~100 ms of CPU per keystroke on a shared host.
	SuggestionScanLimit int
	// Logger receives request errors. Nil uses slog.Default().
	Logger *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 100 << 20
	}
	if o.DefaultBookURI == "" {
		o.DefaultBookURI = "webmail"
	}
	if o.DefaultBookName == "" {
		o.DefaultBookName = "Webmail contacts"
	}
	if o.SuggestionScanLimit <= 0 {
		o.SuggestionScanLimit = 5000
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Handler serves the API. Build it with New.
type Handler struct {
	auth       auth.Backend
	tombstoned func(principal string) bool
	cards      *storage.CardDAV
	contacts   *storage.Contacts
	frecency   *storage.Frecency
	opts       Options
	mux        *http.ServeMux
}

// New builds the API over db. backend is the DAV server's own credential
// backend (see the package comment for why it must be shared). tombstoned
// mirrors the DAV auth gate's soft-delete check; nil disables it, which is
// only right in tests.
func New(db *sql.DB, backend auth.Backend, tombstoned func(string) bool, opts Options) *Handler {
	h := &Handler{
		auth:       backend,
		tombstoned: tombstoned,
		cards:      &storage.CardDAV{DB: db},
		contacts:   &storage.Contacts{DB: db},
		frecency:   &storage.Frecency{DB: db},
		opts:       opts.withDefaults(),
		mux:        http.NewServeMux(),
	}
	m := h.mux
	m.HandleFunc("GET /api/v1/health", h.health)
	m.HandleFunc("GET /api/v1/contacts", h.list)
	m.HandleFunc("POST /api/v1/contacts", h.create)
	m.HandleFunc("DELETE /api/v1/contacts", h.deleteAll)
	// The specific routes are registered alongside the {id...} wildcard; Go's
	// mux prefers the more specific pattern, so "by-email" and "by-uid/x" never
	// fall through to a contact lookup.
	m.HandleFunc("GET /api/v1/contacts/by-email", h.byEmail)
	m.HandleFunc("GET /api/v1/contacts/by-uid/{uid}", h.byUID)
	m.HandleFunc("POST /api/v1/contacts/batch-delete", h.batchDelete)
	// An id is the cards row's serial: a number, because SnappyMail's action
	// layer runs contact ids through intval() before any driver sees them.
	m.HandleFunc("GET /api/v1/contacts/{id}", h.get)
	m.HandleFunc("PUT /api/v1/contacts/{id}", h.update)
	m.HandleFunc("DELETE /api/v1/contacts/{id}", h.delete)
	m.HandleFunc("GET /api/v1/suggestions", h.suggestions)
	m.HandleFunc("POST /api/v1/frecency", h.bumpFrecency)
	return h
}

type ctxKey int

const principalKey ctxKey = iota

func principalOf(r *http.Request) string {
	p, _ := r.Context().Value(principalKey).(string)
	return p
}

// ServeHTTP authenticates, then dispatches. Every route below /api/ is
// authenticated; there is no anonymous surface.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.opts.MaxBodyBytes)
	if h.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "no authentication backend configured")
		return
	}
	principal, ok, reason := h.auth.Check(r)
	if !ok {
		h.auth.Challenge(w.Header())
		writeError(w, http.StatusUnauthorized, reason)
		return
	}
	if h.tombstoned != nil && h.tombstoned(principal) {
		// Valid credentials, account scheduled for deletion: the same 403 the
		// DAV gate gives, so nothing runs as them (SOFT-DELETE-PLAN.md §5).
		writeError(w, http.StatusForbidden, "this account is scheduled for deletion")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), principalKey, principal))
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "principal": principalOf(r)})
}

// --- plumbing ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// fail logs a storage error with the request that hit it and answers 500.
// The message stays generic: a database error is not the client's to see.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	h.opts.Logger.Error("contacts api", "method", r.Method, "path", r.URL.Path,
		"principal", principalOf(r), "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// readJSON decodes a JSON body into v, distinguishing a body over the cap
// (413) from a malformed one (400).
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit))
			return false
		}
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return false
	}
	return true
}

// unquoteETag strips the quotes (and a weak-validator prefix) an If-Match
// header or a CardInfo carries, so it compares against the bare column value.
func unquoteETag(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	return strings.Trim(s, `"`)
}
