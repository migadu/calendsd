package admin

import (
	"embed"
	"html/template"
	"net/http"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"epoch": func(v *int64) string {
		if v == nil {
			return "—"
		}
		return time.Unix(*v, 0).UTC().Format("2006-01-02 15:04:05Z")
	},
	"access": func(v int) string {
		switch v {
		case 1:
			return "owner"
		case 2:
			return "read"
		case 3:
			return "read-write"
		}
		return "?"
	},
	"op": func(v int) string {
		switch v {
		case 1:
			return "add"
		case 2:
			return "modify"
		case 3:
			return "delete"
		}
		return "?"
	},
	"invite": func(v int) string {
		switch v {
		case 1:
			return "no response"
		case 2:
			return "accepted"
		case 3:
			return "declined"
		case 4:
			return "invalid"
		}
		return "?"
	},
}).ParseFS(templateFS, "templates/*.html"))

func renderPage(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		// Headers are gone; at least surface the failure in the body.
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
