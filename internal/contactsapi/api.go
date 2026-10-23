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
	"time"

	"calendsd/internal/auth"
	"calendsd/internal/storage"
)

// Options tune the API. Zero values take the documented defaults.
// DefaultMaxBodyBytes caps a contacts API request body. The vcard field of a
// single contact cannot legitimately approach 2 MiB (a photo-laden card is
// tens of KB); bulk import is a CLI migration, not an HTTP route. The DAV
// listener's 100 MiB cap was previously passed here (audit 5.9).
const DefaultMaxBodyBytes = 2 << 20

type Options struct {
	// MaxBodyBytes caps a request body; zero means DefaultMaxBodyBytes. This
	// listener has its own mux and inherits nothing from the DAV server.
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
	// RequestTimeout bounds each request, authentication included: past it
	// the database and rcptd calls are cancelled and the client gets a 503
	// rather than silence. It sits under webmail's own 10 s curl timeout so
	// the answer arrives before the client gives up. Zero takes 8 s.
	RequestTimeout time.Duration
	// Logger receives request errors. Nil uses slog.Default().
	Logger *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = DefaultMaxBodyBytes
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
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 8 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Handler serves the API. Build it with New.
type Handler struct {
	auth       auth.Backend
	tombstoned func(ctx context.Context, principal string) bool
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
func New(db *sql.DB, backend auth.Backend, tombstoned func(context.Context, string) bool, opts Options) *Handler {
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
	ctx, cancel := context.WithTimeout(r.Context(), h.opts.RequestTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	principal, err := h.auth.Check(r)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			// The rcptd lookup or first-login provisioning ran out of time:
			// a server-side stall, not bad credentials, so no challenge.
			h.timedOut(w, r, "authentication")
			return
		case errors.Is(err, auth.ErrUnavailable):
			// rcptd is down or faulting. A 401 here would have webmail ask
			// the user for a password that is fine.
			h.opts.Logger.Warn("contacts api: authentication unavailable", "reason", err,
				"method", r.Method, "path", r.URL.Path)
			writeError(w, http.StatusServiceUnavailable, auth.ClientMessage(err))
			return
		}
		h.auth.Challenge(w.Header(), r)
		writeError(w, http.StatusUnauthorized, auth.ClientMessage(err))
		return
	}
	if h.tombstoned != nil && h.tombstoned(ctx, principal) {
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
	if errors.Is(err, context.DeadlineExceeded) {
		h.timedOut(w, r, "storage")
		return
	}
	// The card went away between our read and the write (a concurrent
	// delete): the same answer as if it had already been gone.
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such contact")
		return
	}
	h.opts.Logger.Error("contacts api", "method", r.Method, "path", r.URL.Path,
		"principal", principalOf(r), "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// timedOut answers a request that hit RequestTimeout with a 503, logging which
// stage stalled so the cause is findable after the fact.
func (h *Handler) timedOut(w http.ResponseWriter, r *http.Request, stage string) {
	h.opts.Logger.Warn("contacts api: request timed out", "stage", stage,
		"method", r.Method, "path", r.URL.Path, "principal", principalOf(r),
		"timeout", h.opts.RequestTimeout)
	writeError(w, http.StatusServiceUnavailable, "timed out waiting for "+stage)
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
