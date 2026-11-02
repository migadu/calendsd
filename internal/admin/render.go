// Response plumbing shared by every page: error mapping, JSON/HTML dispatch.

package admin

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"log/slog"
	"net/http"
)

type xmlName = xml.Name

// errBadRequest wraps parameter errors so respond can pick the status.
var errBadRequest = errors.New("bad request")

// errNotFound marks lookups that came back empty.
var errNotFound = errors.New("not found")

func (a *App) respond(w http.ResponseWriter, r *http.Request, template string, data func(r *http.Request) (any, error)) {
	v, err := data(r)
	switch {
	case errors.Is(err, errBadRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case errors.Is(err, errNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case err != nil:
		slog.Error("admin page failed", "path", r.URL.Path, "err", err)
		http.Error(w, "internal error; see the server log", http.StatusInternalServerError)
		return
	}
	if template == "" {
		writeJSON(w, v)
		return
	}
	renderPage(w, template, v)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// The header is out, so a failure can only be logged: an unencodable
	// value is a bug, a write error a client that went away.
	if err := enc.Encode(v); err != nil {
		slog.Warn("admin: writing JSON response", "err", err)
	}
}

func propString(props map[xmlName]any, space, local string) string {
	if props == nil {
		return ""
	}
	s, _ := props[xmlName{Space: space, Local: local}].(string)
	return s
}
