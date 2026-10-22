package admin

// Phase C write endpoints (ADMIN-PLAN.md §3.8), all POST, all behind the
// CSRF header and the -readonly gate, all emitting a structured audit log
// line (no audit table, per §1). Delegation and collection properties go
// through the production backends/plugin chain; users/principals and
// orphan deletion go through the storage.AdminWrites carve-out.

import (
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"calserver/internal/auth"
	"calserver/internal/config"
	davxml "calserver/internal/dav/xml"
	"calserver/internal/storage"
)

// Writes is the §1 carve-out surface; *storage.AdminWrites implements it,
// tests fake it. digestA1 is computed by the handlers so the realm stays in
// one place (App.Config.Realm).
type Writes interface {
	CreateUser(username, digestA1, email, displayName string) error
	SetPassword(username, digestA1 string) error
	DeleteUser(username string) error
	DeleteUserCascade(username string) error
	ScheduleDelete(username string, days int, reason string) error
	CancelDelete(username string) error
	Deletions() ([]storage.DeletedPrincipal, error)
	DeleteOrphans(check string) (int64, error)
}

var _ Writes = (*storage.AdminWrites)(nil)

// mutationGate enforces the CSRF header and the read-only default on every
// mutating endpoint.
func (a *App) mutationGate(w http.ResponseWriter, r *http.Request) bool {
	if !requireMutationHeader(w, r) {
		return false
	}
	if a.ReadOnly {
		http.Error(w, "read-only mode: restart `calctl panel` with -readonly=false to enable write actions", http.StatusForbidden)
		return false
	}
	return true
}

// writeGate is mutationGate for the endpoints that write through Writes,
// which must also be wired. proppatch and provision write through the DAV
// stack instead and check their own factories.
func (a *App) writeGate(w http.ResponseWriter, r *http.Request) bool {
	if !a.mutationGate(w, r) {
		return false
	}
	if a.Writes == nil {
		http.Error(w, "write actions not wired in this deployment", http.StatusForbidden)
		return false
	}
	return true
}

// audit is the structured audit line every §3.8 write emits.
func audit(r *http.Request, action string, kv ...any) {
	slog.Info("admin write", append([]any{"actor", actor(r), "action", action}, kv...)...)
}

type writeResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func (a *App) createUser(w http.ResponseWriter, r *http.Request) {
	if !a.writeGate(w, r) {
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	if username == "" || password == "" {
		http.Error(w, "username and password are required", http.StatusBadRequest)
		return
	}
	digest := auth.DigestA1(username, a.Config.Realm, password)
	err := a.Writes.CreateUser(username, digest,
		r.PostFormValue("email"), r.PostFormValue("displayname"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	audit(r, "create-user", "username", username, "realm", a.Config.Realm)
	writeJSON(w, writeResult{OK: true,
		Detail: "created user " + username + " and principals/" + username + " (with proxy sub-principals)"})
}

func (a *App) setPassword(w http.ResponseWriter, r *http.Request) {
	if !a.writeGate(w, r) {
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	if username == "" || password == "" {
		http.Error(w, "username and password are required", http.StatusBadRequest)
		return
	}
	if err := a.Writes.SetPassword(username, auth.DigestA1(username, a.Config.Realm, password)); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	audit(r, "set-password", "username", username, "realm", a.Config.Realm)
	writeJSON(w, writeResult{OK: true, Detail: "password reset for " + username + " under realm " + a.Config.Realm})
}

func (a *App) deleteUser(w http.ResponseWriter, r *http.Request) {
	if !a.writeGate(w, r) {
		return
	}
	username := r.PostFormValue("username")
	if username == "" {
		http.Error(w, "username is required", http.StatusBadRequest)
		return
	}
	// cascade=1 also drops the account's owned calendars and address books;
	// the default refuses while any collection remains (DAV cleanup first).
	cascade := r.PostFormValue("cascade") == "1"
	var err error
	if cascade {
		err = a.Writes.DeleteUserCascade(username)
	} else {
		err = a.Writes.DeleteUser(username)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	audit(r, "delete-user", "username", username, "cascade", cascade)
	detail := "deleted user " + username + " and principals/" + username + " (with proxy sub-principals)"
	if cascade {
		detail += ", along with all owned calendars and address books"
	}
	writeJSON(w, writeResult{OK: true, Detail: detail})
}

func (a *App) scheduleDelete(w http.ResponseWriter, r *http.Request) {
	if !a.writeGate(w, r) {
		return
	}
	username := r.PostFormValue("username")
	if username == "" {
		http.Error(w, "username is required", http.StatusBadRequest)
		return
	}
	// Default the window from the server config (itself defaulted to 30); an
	// explicit days= overrides it.
	days := config.RetentionDays(a.Config.RetentionDays)
	if v := r.PostFormValue("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, "days must be a positive integer", http.StatusBadRequest)
			return
		}
		days = n
	}
	if err := a.Writes.ScheduleDelete(username, days, r.PostFormValue("reason")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	audit(r, "schedule-delete", "username", username, "days", days)
	writeJSON(w, writeResult{OK: true,
		Detail: fmt.Sprintf("scheduled %s for deletion in %d day(s); it can no longer log in", username, days)})
}

func (a *App) restoreUser(w http.ResponseWriter, r *http.Request) {
	if !a.writeGate(w, r) {
		return
	}
	username := r.PostFormValue("username")
	if username == "" {
		http.Error(w, "username is required", http.StatusBadRequest)
		return
	}
	if err := a.Writes.CancelDelete(username); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	audit(r, "restore-user", "username", username)
	writeJSON(w, writeResult{OK: true, Detail: "restored " + username + "; the account can log in again"})
}

func (a *App) setDelegation(w http.ResponseWriter, r *http.Request) {
	if !a.writeGate(w, r) {
		return
	}
	principal := r.PostFormValue("principal")
	kind := r.PostFormValue("kind")
	if principal == "" || (kind != "read" && kind != "write") {
		http.Error(w, "need principal= and kind=read|write", http.StatusBadRequest)
		return
	}
	members := strings.Fields(r.PostFormValue("members"))
	// Reject typos up front: every member must be an existing principal.
	for _, m := range members {
		p, err := a.Principals.PrincipalByPath(m)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if p == nil {
			http.Error(w, "unknown principal "+m, http.StatusBadRequest)
			return
		}
	}
	group := principal + "/calendar-proxy-" + kind
	if err := a.Principals.SetGroupMemberSet(group, members); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	audit(r, "set-delegation", "group", group, "members", strings.Join(members, ","))
	writeJSON(w, writeResult{OK: true,
		Detail: fmt.Sprintf("%s now has %d member(s)", group, len(members))})
}

func (a *App) propPatch(w http.ResponseWriter, r *http.Request) {
	if !a.mutationGate(w, r) {
		return
	}
	if a.NewSimulator == nil {
		http.Error(w, "simulator not wired; proppatch unavailable", http.StatusForbidden)
		return
	}
	path := r.PostFormValue("path")
	name := r.PostFormValue("name")
	if path == "" || name == "" {
		http.Error(w, "need path= and name= ({DAV:}displayname style)", http.StatusBadRequest)
		return
	}
	space, local, ok := parseClark(name)
	if !ok {
		http.Error(w, "property name must be clark-form, e.g. {DAV:}displayname", http.StatusBadRequest)
		return
	}
	remove := r.PostFormValue("remove") == "1"
	var propXML string
	if remove {
		propXML = fmt.Sprintf(`<d:remove><d:prop><x:%s xmlns:x="%s"/></d:prop></d:remove>`,
			local, xmlEscape(space))
	} else {
		propXML = fmt.Sprintf(`<d:set><d:prop><x:%s xmlns:x="%s">%s</x:%s></d:prop></d:set>`,
			local, xmlEscape(space), xmlEscape(r.PostFormValue("value")), local)
	}
	body := `<?xml version="1.0"?><d:propertyupdate xmlns:d="DAV:">` + propXML + `</d:propertyupdate>`

	// Through the full plugin chain as the admin principal: the backend
	// PropPatch path, column mapping, per-user overlay logic and all.
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	res := davRequest(a.NewSimulator(adminViewPrincipal), "PROPPATCH", path, body, "0")
	audit(r, "proppatch", "path", path, "name", name, "remove", remove, "status", res.Code)
	if res.Code != http.StatusMultiStatus || !propstatsOK(res.Body.Bytes()) {
		http.Error(w, fmt.Sprintf("PROPPATCH not accepted (status %d): %s", res.Code, res.Body.String()),
			http.StatusBadGateway)
		return
	}
	detail := "property updated on " + path
	if remove {
		detail = "property removed on " + path
	}
	writeJSON(w, writeResult{OK: true, Detail: detail})
}

// propstatsOK reports whether a PROPPATCH multistatus body carries only 2xx
// propstats. The chain answers a successful set with "200 OK" but a
// successful remove with "204 No Content" (sabre's convention, see
// dav.PropPatch), so checking for one literal status is wrong.
func propstatsOK(body []byte) bool {
	var ms xmlMultistatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return false
	}
	seen := false
	for _, resp := range ms.Responses {
		for _, ps := range resp.Propstat {
			seen = true
			f := strings.Fields(ps.Status) // "HTTP/1.1 204 No Content"
			if len(f) < 2 || !strings.HasPrefix(f[1], "2") {
				return false
			}
		}
	}
	return seen
}

func (a *App) provision(w http.ResponseWriter, r *http.Request) {
	if !a.mutationGate(w, r) {
		return
	}
	if a.NewProvisioner == nil {
		http.Error(w, "provisioner not wired in this deployment", http.StatusForbidden)
		return
	}
	principal := r.PostFormValue("principal")
	name, ok := strings.CutPrefix(principal, "principals/")
	if !ok || name == "" || strings.Contains(name, "/") {
		http.Error(w, "need principal=principals/<name>", http.StatusBadRequest)
		return
	}
	// The production auto-provision code runs on home access; a Depth-1
	// PROPFIND on each home as the principal triggers it (§3.8: "reuse
	// auto-provision code").
	srv := a.NewProvisioner(principal)
	var statuses []string
	for _, home := range []string{"/calendars/" + name + "/", "/addressbooks/" + name + "/"} {
		res := davRequest(srv, "PROPFIND", home,
			`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/></d:prop></d:propfind>`, "1")
		statuses = append(statuses, fmt.Sprintf("%s: %d", home, res.Code))
	}
	audit(r, "provision", "principal", principal, "result", strings.Join(statuses, ", "))
	writeJSON(w, writeResult{OK: true,
		Detail: "auto-provision triggered — " + strings.Join(statuses, ", ")})
}

func (a *App) deleteOrphans(w http.ResponseWriter, r *http.Request) {
	if !a.writeGate(w, r) {
		return
	}
	check := r.PostFormValue("check")
	n, err := a.Writes.DeleteOrphans(check)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	audit(r, "delete-orphans", "check", check, "deleted", n)
	writeJSON(w, writeResult{OK: true, Detail: fmt.Sprintf("deleted %d row(s) for %s", n, check)})
}

// davRequest runs one in-process DAV request against h, as the simulator
// and the DAV-backed write actions do.
func davRequest(h http.Handler, method, path, body, depth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// parseClark parses a clark-notation property name via the shared davxml
// parser, then rejects locals that would break out of the generated XML
// element (the value and namespace are escaped separately).
func parseClark(name string) (space, local string, ok bool) {
	n, err := davxml.ParseClark(name)
	if err != nil || strings.ContainsAny(n.Local, "<>& \"'") {
		return "", "", false
	}
	return n.Space, n.Local, true
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
