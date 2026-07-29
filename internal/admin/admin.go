// Package admin implements the calserver admin panel (ADMIN-PLAN.md): a
// read-first debugging UI over the shared sabre database. Every page is a
// thin HTML layer over a JSON endpoint; data comes from the same backend
// interfaces the DAV server uses plus the read-only storage.AdminQueries
// aggregates.
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/caldav/ical"
	"calserver/internal/carddav"
	"calserver/internal/dav"
	"calserver/internal/server"
	"calserver/internal/storage"
)

type xmlName = xml.Name

// Store is the read-only aggregate access the panel needs beyond the
// backend interfaces; *storage.AdminQueries implements it, tests fake it.
type Store interface {
	Counts() (*storage.AdminCounts, error)
	RecentChanges(limit int) ([]storage.AdminChange, error)
	AuthDiagnostics() (*storage.AdminAuthDiagnostics, error)
	UserForPrincipal(principalURI string) (*storage.AdminUserRow, error)
	LookupCalendar(principalURI, uri string) (string, error)
	CalendarInstances(calendarID string) ([]storage.AdminCalendarInstance, error)
	CalendarMeta(calendarID string) (*storage.AdminCalendarMeta, error)
	PropertyRows(path string) ([]storage.AdminPropRow, error)
	AddressBook(id string) (*storage.AdminAddressBook, error)
	LookupAddressBook(principalURI, uri string) (string, error)
	Objects(q storage.AdminObjectQuery) ([]storage.AdminObjectRow, bool, error)
	Object(calendarID, uri string) (*storage.AdminObjectDetail, error)
	CardsSearch(bookID, match string, inData bool, limit int) ([]storage.AdminCardRow, bool, error)
	Card(bookID, uri string) (*storage.AdminCardDetail, error)

	ChangeLog(kind, collectionID string, limit int) ([]storage.AdminChange, error)
	PendingShareInvites(principalURI string) ([]storage.AdminCalendarInstance, error)
	Doctor(scanCap int) (*storage.AdminDoctorReport, error)
}

var _ Store = (*storage.AdminQueries)(nil)

// App wires the panel's data sources. Config must match the DAV server's
// effective config or the config echo will show the drift (§2).
type App struct {
	Store        Store
	Principals   acl.Backend
	Calendars    caldav.Backend
	AddressBooks carddav.Backend
	// Scheduling serves the inbox debugger; nil hides it (§3.6).
	Scheduling caldav.SchedulingBackend
	// NewSimulator builds a production plugin stack with the given
	// principal forced (server.NewLoggedIn); nil disables the privilege
	// simulator (§3.5).
	NewSimulator func(principal string) *dav.Server
	// NewProvisioner is NewSimulator with AutoProvision forced on; the
	// provision action triggers the production auto-provision code by
	// visiting the principal's homes through it (§3.8).
	NewProvisioner func(principal string) *dav.Server
	// Writes is the §1 carve-out (users/principals, orphan deletion); nil
	// plus ReadOnly=true is the safe default.
	Writes   Writes
	Config   server.Config
	ReadOnly bool
}

// Handler returns the admin mux (HTML pages + /api JSON). Wrap it with
// BasicAuth before serving.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	page := func(pattern, name string, data func(r *http.Request) (any, error)) {
		mux.HandleFunc("GET "+pattern, func(w http.ResponseWriter, r *http.Request) {
			a.respond(w, r, name, data)
		})
		mux.HandleFunc("GET /api/"+name, func(w http.ResponseWriter, r *http.Request) {
			a.respond(w, r, "", data)
		})
	}
	page("/{$}", "dashboard", a.dashboardData)
	page("/principals", "principals", a.principalsData)
	page("/principal", "principal", a.principalData)
	page("/calendar", "calendar", a.calendarData)
	page("/addressbook", "addressbook", a.addressBookData)
	page("/objects", "objects", a.objectsData)
	page("/object", "object", a.objectData)
	page("/cards", "cards", a.cardsData)
	page("/card", "card", a.cardData)
	page("/scheduling", "scheduling", a.schedulingData)
	page("/sync", "sync", a.syncData)
	page("/simulate", "simulate", a.simulateData)
	page("/doctor", "doctor", a.doctorData)
	mux.HandleFunc("POST /api/check-credentials", a.checkCredentials)
	mux.HandleFunc("POST /api/create-user", a.createUser)
	mux.HandleFunc("POST /api/set-password", a.setPassword)
	mux.HandleFunc("POST /api/delete-user", a.deleteUser)
	mux.HandleFunc("POST /api/schedule-delete", a.scheduleDelete)
	mux.HandleFunc("POST /api/restore-user", a.restoreUser)
	mux.HandleFunc("POST /api/set-delegation", a.setDelegation)
	mux.HandleFunc("POST /api/proppatch", a.propPatch)
	mux.HandleFunc("POST /api/provision", a.provision)
	mux.HandleFunc("POST /api/delete-orphans", a.deleteOrphans)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	enc.Encode(v)
}

// --- Dashboard (§3.1) ---

type DashboardData struct {
	ConfigLine  string                        `json:"configLine"`
	ReadOnly    bool                          `json:"readOnly"`
	Counts      *storage.AdminCounts          `json:"counts"`
	Changes     []storage.AdminChange         `json:"recentChanges"`
	Diagnostics *storage.AdminAuthDiagnostics `json:"authDiagnostics"`
}

func (a *App) dashboardData(r *http.Request) (any, error) {
	counts, err := a.Store.Counts()
	if err != nil {
		return nil, fmt.Errorf("counts: %w", err)
	}
	changes, err := a.Store.RecentChanges(30)
	if err != nil {
		return nil, fmt.Errorf("recent changes: %w", err)
	}
	diags, err := a.Store.AuthDiagnostics()
	if err != nil {
		return nil, fmt.Errorf("auth diagnostics: %w", err)
	}
	return &DashboardData{
		ConfigLine:  a.Config.Canonical(),
		ReadOnly:    a.ReadOnly,
		Counts:      counts,
		Changes:     changes,
		Diagnostics: diags,
	}, nil
}

// --- Principals (§3.2) ---

type PrincipalRow struct {
	URI         string `json:"uri"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	HasUser     bool   `json:"hasUser"`
}

type PrincipalsData struct {
	Principals []PrincipalRow `json:"principals"`
	// UsersWithoutPrincipal comes along so orphaned credentials are visible
	// on the same page.
	UsersWithoutPrincipal []string `json:"usersWithoutPrincipal"`
}

func (a *App) principalsData(r *http.Request) (any, error) {
	list, err := a.Principals.PrincipalsByPrefix("principals")
	if err != nil {
		return nil, err
	}
	diags, err := a.Store.AuthDiagnostics()
	if err != nil {
		return nil, err
	}
	noUser := map[string]bool{}
	for _, p := range diags.PrincipalsWithoutUser {
		noUser[p] = true
	}
	out := &PrincipalsData{UsersWithoutPrincipal: diags.UsersWithoutPrincipal}
	for _, p := range list {
		out.Principals = append(out.Principals, PrincipalRow{
			URI:         p.URI,
			DisplayName: propString(p.Props, "DAV:", "displayname"),
			Email:       propString(p.Props, "http://sabredav.org/ns", "email-address"),
			HasUser:     !noUser[p.URI],
		})
	}
	return out, nil
}

// CollectionRef links an inspector page to one collection of a principal.
type CollectionRef struct {
	URI         string `json:"uri"`
	DisplayName string `json:"displayName"`
	ID          string `json:"id"`
}

type PrincipalDetail struct {
	URI         string `json:"uri"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	// User is the matching users row; nil means this principal cannot log
	// in (§3.2 auth diagnostics).
	User             *storage.AdminUserRow `json:"user"`
	DigestWellFormed bool                  `json:"digestWellFormed"`
	// Groups lists non-proxy group memberships verbatim.
	Groups []string `json:"groups"`
	// SeesCalendarsOf renders proxy membership in delegation terms: this
	// principal was granted read/write proxy access by those users.
	SeesCalendarsOfRead  []string `json:"seesCalendarsOfRead"`
	SeesCalendarsOfWrite []string `json:"seesCalendarsOfWrite"`
	// Delegates lists who can act for this principal (members of its own
	// calendar-proxy-read/-write sub-principals).
	ReadDelegates  []string `json:"readDelegates"`
	WriteDelegates []string `json:"writeDelegates"`

	Calendars    []CollectionRef `json:"calendars"`
	AddressBooks []CollectionRef `json:"addressBooks"`

	// Deletion, when non-nil, marks a pending soft-delete with its purge date.
	Deletion *DeletionInfo `json:"deletion,omitempty"`
}

// DeletionInfo renders a pending soft-delete on the principal page.
type DeletionInfo struct {
	PurgeAfter string `json:"purgeAfter"`
	Reason     string `json:"reason,omitempty"`
}

func (a *App) principalData(r *http.Request) (any, error) {
	path := r.URL.Query().Get("path")
	if path == "" {
		return nil, fmt.Errorf("%w: missing ?path=principals/<name>", errBadRequest)
	}
	p, err := a.Principals.PrincipalByPath(path)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("%w: principal %s", errNotFound, path)
	}
	d := &PrincipalDetail{
		URI:         p.URI,
		DisplayName: propString(p.Props, "DAV:", "displayname"),
		Email:       propString(p.Props, "http://sabredav.org/ns", "email-address"),
	}
	if d.User, err = a.Store.UserForPrincipal(path); err != nil {
		return nil, err
	}
	if d.User != nil {
		d.DigestWellFormed = wellFormedDigest(d.User.DigestA1)
		if d.DigestWellFormed {
			// A well-formed digesta1 is a reusable HTTP-Digest credential;
			// don't ship it to the browser/JSON. A malformed value is no
			// credential — keep it so the page can show what's actually
			// stored (truncated hash? bcrypt in the wrong column?).
			d.User.DigestA1 = ""
		}
	}

	groups, err := a.Principals.GroupMembership(path)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		switch owner, kind := splitProxy(g); kind {
		case "calendar-proxy-read":
			d.SeesCalendarsOfRead = append(d.SeesCalendarsOfRead, owner)
		case "calendar-proxy-write":
			d.SeesCalendarsOfWrite = append(d.SeesCalendarsOfWrite, owner)
		default:
			d.Groups = append(d.Groups, g)
		}
	}
	if d.ReadDelegates, err = a.Principals.GroupMemberSet(path + "/calendar-proxy-read"); err != nil {
		return nil, err
	}
	if d.WriteDelegates, err = a.Principals.GroupMemberSet(path + "/calendar-proxy-write"); err != nil {
		return nil, err
	}

	cals, err := a.Calendars.CalendarsForUser(path)
	if err != nil {
		return nil, err
	}
	for _, c := range cals {
		d.Calendars = append(d.Calendars, CollectionRef{
			URI:         c.URI,
			DisplayName: propString(c.Props, "DAV:", "displayname"),
			ID:          c.ID.Calendar,
		})
	}
	books, err := a.AddressBooks.AddressBooksForUser(path)
	if err != nil {
		return nil, err
	}
	for _, b := range books {
		d.AddressBooks = append(d.AddressBooks, CollectionRef{
			URI:         b.URI,
			DisplayName: propString(b.Props, "DAV:", "displayname"),
			ID:          b.ID,
		})
	}
	// Pending soft-delete, if any (read through the writes carve-out; nil in a
	// deployment with no writes wired, where the danger-zone forms 403 anyway).
	if a.Writes != nil {
		dels, err := a.Writes.Deletions()
		if err != nil {
			return nil, err
		}
		for _, del := range dels {
			if del.Principal == p.URI {
				d.Deletion = &DeletionInfo{PurgeAfter: del.PurgeAfter.Format("2006-01-02"), Reason: del.Reason}
				break
			}
		}
	}
	return d, nil
}

// splitProxy splits "principals/alice/calendar-proxy-read" into
// ("principals/alice", "calendar-proxy-read"); kind is "" for non-proxy
// paths.
func splitProxy(group string) (owner, kind string) {
	i := strings.LastIndexByte(group, '/')
	if i < 0 {
		return group, ""
	}
	switch group[i+1:] {
	case "calendar-proxy-read", "calendar-proxy-write":
		return group[:i], group[i+1:]
	}
	return group, ""
}

func wellFormedDigest(digest string) bool {
	if len(digest) != 32 {
		return false
	}
	for _, c := range digest {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// --- Calendar & address book inspectors (§3.3) ---

// InstanceView is one principal's view of the calendar with its
// property layers: the live column-backed props are on the instance row,
// PropRows adds dead props and per-user overlays for the instance path.
type InstanceView struct {
	Instance storage.AdminCalendarInstance `json:"instance"`
	Path     string                        `json:"path"`
	PropRows []storage.AdminPropRow        `json:"propRows"`
}

type CalendarDetail struct {
	ID        string                     `json:"id"`
	Meta      *storage.AdminCalendarMeta `json:"meta"`
	Instances []InstanceView             `json:"instances"`
}

func (a *App) calendarData(r *http.Request) (any, error) {
	id, err := a.resolveCalendar(r)
	if err != nil {
		return nil, err
	}
	meta, err := a.Store.CalendarMeta(id)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return nil, fmt.Errorf("%w: calendar %s", errNotFound, id)
	}
	instances, err := a.Store.CalendarInstances(id)
	if err != nil {
		return nil, err
	}
	d := &CalendarDetail{ID: id, Meta: meta}
	for _, in := range instances {
		path := collectionPath("calendars", in.PrincipalURI, in.URI)
		props, err := a.Store.PropertyRows(path)
		if err != nil {
			return nil, err
		}
		d.Instances = append(d.Instances, InstanceView{Instance: in, Path: path, PropRows: props})
	}
	return d, nil
}

type AddressBookDetail struct {
	Book     *storage.AdminAddressBook `json:"book"`
	Path     string                    `json:"path"`
	PropRows []storage.AdminPropRow    `json:"propRows"`
}

func (a *App) addressBookData(r *http.Request) (any, error) {
	id, err := a.resolveAddressBook(r)
	if err != nil {
		return nil, err
	}
	book, err := a.Store.AddressBook(id)
	if err != nil {
		return nil, err
	}
	if book == nil {
		return nil, fmt.Errorf("%w: address book %s", errNotFound, id)
	}
	path := collectionPath("addressbooks", book.PrincipalURI, book.URI)
	props, err := a.Store.PropertyRows(path)
	if err != nil {
		return nil, err
	}
	return &AddressBookDetail{Book: book, Path: path, PropRows: props}, nil
}

// collectionPath rebuilds the propertystorage path convention:
// "calendars/<name>/<uri>" for principal "principals/<name>".
func collectionPath(root, principalURI, uri string) string {
	return root + "/" + strings.TrimPrefix(principalURI, "principals/") + "/" + uri
}

// resolveCalendar accepts ?id= directly or ?principal=&uri= via lookup.
func (a *App) resolveCalendar(r *http.Request) (string, error) {
	q := r.URL.Query()
	if id := q.Get("id"); id != "" {
		return id, nil
	}
	principal, uri := q.Get("principal"), q.Get("uri")
	if principal == "" || uri == "" {
		return "", fmt.Errorf("%w: need ?id= or ?principal=&uri=", errBadRequest)
	}
	id, err := a.Store.LookupCalendar(principal, uri)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("%w: no calendar %s/%s", errNotFound, principal, uri)
	}
	return id, nil
}

func (a *App) resolveAddressBook(r *http.Request) (string, error) {
	q := r.URL.Query()
	if id := q.Get("id"); id != "" {
		return id, nil
	}
	principal, uri := q.Get("principal"), q.Get("uri")
	if principal == "" || uri == "" {
		return "", fmt.Errorf("%w: need ?id= or ?principal=&uri=", errBadRequest)
	}
	id, err := a.Store.LookupAddressBook(principal, uri)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("%w: no address book %s/%s", errNotFound, principal, uri)
	}
	return id, nil
}

// --- Object browser (§3.4) ---

type ObjectsData struct {
	CalendarID string                   `json:"calendarId"`
	Query      string                   `json:"query"`
	Objects    []storage.AdminObjectRow `json:"objects"`
	Truncated  bool                     `json:"truncated"`
}

func (a *App) objectsData(r *http.Request) (any, error) {
	q := r.URL.Query()
	id := q.Get("calendar")
	if id == "" {
		var err error
		if id, err = a.resolveCalendarParam(q.Get("principal"), q.Get("uri")); err != nil {
			return nil, err
		}
	}
	query := storage.AdminObjectQuery{
		CalendarID: id,
		Match:      q.Get("match"),
		InData:     q.Get("indata") == "1",
	}
	var err error
	if query.Start, err = parseTimeParam(q.Get("start")); err != nil {
		return nil, err
	}
	if query.End, err = parseTimeParam(q.Get("end")); err != nil {
		return nil, err
	}
	if lim := q.Get("limit"); lim != "" {
		if query.Limit, err = strconv.Atoi(lim); err != nil {
			return nil, fmt.Errorf("%w: limit: %v", errBadRequest, err)
		}
	}
	objects, truncated, err := a.Store.Objects(query)
	if err != nil {
		return nil, err
	}
	return &ObjectsData{CalendarID: id, Query: query.Match, Objects: objects, Truncated: truncated}, nil
}

func (a *App) resolveCalendarParam(principal, uri string) (string, error) {
	if principal == "" || uri == "" {
		return "", fmt.Errorf("%w: need ?calendar= or ?principal=&uri=", errBadRequest)
	}
	id, err := a.Store.LookupCalendar(principal, uri)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("%w: no calendar %s/%s", errNotFound, principal, uri)
	}
	return id, nil
}

// parseTimeParam accepts RFC 3339 or a raw Unix-seconds integer.
func parseTimeParam(s string) (*int64, error) {
	if s == "" {
		return nil, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return &n, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, fmt.Errorf("%w: time %q (want RFC 3339 or Unix seconds)", errBadRequest, s)
	}
	n := t.Unix()
	return &n, nil
}

// DerivedFields are the denormalized columns recomputed from calendardata
// with the same ical.Denormalize the write path uses, so a diff means the
// stored row is stale, not that the recomputation differs (§3.4).
type DerivedFields struct {
	ETag            string `json:"etag"`
	Size            int64  `json:"size"`
	ComponentType   string `json:"componentType"`
	UID             string `json:"uid"`
	FirstOccurrence *int64 `json:"firstOccurrence"`
	LastOccurrence  *int64 `json:"lastOccurrence"`
}

type ObjectDetail struct {
	CalendarID string                 `json:"calendarId"`
	Stored     storage.AdminObjectRow `json:"stored"`
	Data       string                 `json:"data"`
	Derived    *DerivedFields         `json:"derived"`
	ParseError string                 `json:"parseError"`
	// Diffs names the stored columns that disagree with the derived
	// values — the "event missing from time-range REPORT" explainer.
	Diffs []string `json:"diffs"`
}

func (a *App) objectData(r *http.Request) (any, error) {
	q := r.URL.Query()
	id, uri := q.Get("calendar"), q.Get("uri")
	if id == "" || uri == "" {
		return nil, fmt.Errorf("%w: need ?calendar=&uri=", errBadRequest)
	}
	obj, err := a.Store.Object(id, uri)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, fmt.Errorf("%w: object %s in calendar %s", errNotFound, uri, id)
	}
	d := &ObjectDetail{CalendarID: id, Stored: obj.Row, Data: string(obj.Data)}
	den, err := ical.Denormalize(obj.Data)
	if err != nil {
		d.ParseError = err.Error()
		return d, nil
	}
	d.Derived = &DerivedFields{
		ETag:            den.ETag,
		Size:            den.Size,
		ComponentType:   den.ComponentType,
		UID:             den.UID,
		FirstOccurrence: den.FirstOccurrence,
		LastOccurrence:  den.LastOccurrence,
	}
	d.Diffs = diffStoredDerived(obj.Row, d.Derived)
	return d, nil
}

func diffStoredDerived(stored storage.AdminObjectRow, derived *DerivedFields) []string {
	var diffs []string
	if stored.ETag != derived.ETag {
		diffs = append(diffs, "etag")
	}
	if stored.Size != derived.Size {
		diffs = append(diffs, "size")
	}
	if stored.ComponentType != derived.ComponentType {
		diffs = append(diffs, "componenttype")
	}
	if stored.UID != derived.UID {
		diffs = append(diffs, "uid")
	}
	if !equalInt64Ptr(stored.FirstOccurrence, derived.FirstOccurrence) {
		diffs = append(diffs, "firstoccurence")
	}
	if !equalInt64Ptr(stored.LastOccurrence, derived.LastOccurrence) {
		diffs = append(diffs, "lastoccurence")
	}
	return diffs
}

func equalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// --- Cards ---

type CardsData struct {
	BookID    string                 `json:"bookId"`
	Query     string                 `json:"query"`
	Cards     []storage.AdminCardRow `json:"cards"`
	Truncated bool                   `json:"truncated"`
}

func (a *App) cardsData(r *http.Request) (any, error) {
	q := r.URL.Query()
	id := q.Get("book")
	if id == "" {
		return nil, fmt.Errorf("%w: need ?book=", errBadRequest)
	}
	limit := 0
	if lim := q.Get("limit"); lim != "" {
		var err error
		if limit, err = strconv.Atoi(lim); err != nil {
			return nil, fmt.Errorf("%w: limit: %v", errBadRequest, err)
		}
	}
	cards, truncated, err := a.Store.CardsSearch(id, q.Get("match"), q.Get("indata") == "1", limit)
	if err != nil {
		return nil, err
	}
	return &CardsData{BookID: id, Query: q.Get("match"), Cards: cards, Truncated: truncated}, nil
}

type CardDetail struct {
	BookID string               `json:"bookId"`
	Stored storage.AdminCardRow `json:"stored"`
	Data   string               `json:"data"`
	// DerivedETag/DerivedSize follow the cards convention: md5 hex of the
	// payload, payload length.
	DerivedETag string   `json:"derivedEtag"`
	DerivedSize int64    `json:"derivedSize"`
	Diffs       []string `json:"diffs"`
}

func (a *App) cardData(r *http.Request) (any, error) {
	q := r.URL.Query()
	id, uri := q.Get("book"), q.Get("uri")
	if id == "" || uri == "" {
		return nil, fmt.Errorf("%w: need ?book=&uri=", errBadRequest)
	}
	card, err := a.Store.Card(id, uri)
	if err != nil {
		return nil, err
	}
	if card == nil {
		return nil, fmt.Errorf("%w: card %s in book %s", errNotFound, uri, id)
	}
	d := &CardDetail{BookID: id, Stored: card.Row, Data: string(card.Data)}
	d.DerivedETag = strings.Trim((&carddav.CardInfo{CardData: card.Data}).QuotedETag(), `"`)
	d.DerivedSize = int64(len(card.Data))
	if d.Stored.ETag != d.DerivedETag {
		d.Diffs = append(d.Diffs, "etag")
	}
	if d.Stored.Size != d.DerivedSize {
		d.Diffs = append(d.Diffs, "size")
	}
	return d, nil
}

// --- Credential check (§3.2) ---

// CheckCredentialsResult reports whether a username/password would pass DAV
// auth under the configured realm. digesta1 is an opaque md5, so a realm
// mismatch is not detectable offline; this interactive check answers it:
// a known-good password that does not match a well-formed stored hash means
// the hash was minted under a different realm.
type CheckCredentialsResult struct {
	Found            bool   `json:"found"`
	DigestWellFormed bool   `json:"digestWellFormed"`
	Match            bool   `json:"match"`
	Note             string `json:"note"`
}

func (a *App) checkCredentials(w http.ResponseWriter, r *http.Request) {
	// No DB write, but it carries a password: POST-only plus the CSRF
	// header so a cross-site form can't probe credentials.
	if !requireMutationHeader(w, r) {
		return
	}
	username, password := r.PostFormValue("username"), r.PostFormValue("password")
	if username == "" {
		http.Error(w, "missing username", http.StatusBadRequest)
		return
	}
	user, err := a.Store.UserForPrincipal("principals/" + username)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	res := CheckCredentialsResult{}
	switch {
	case user == nil:
		res.Note = "no users row — this account cannot log in"
	case !wellFormedDigest(user.DigestA1):
		res.Found = true
		res.Note = "digesta1 is not a 32-char md5 hex — auth always fails"
	default:
		res.Found, res.DigestWellFormed = true, true
		res.Match = subtle.ConstantTimeCompare(
			[]byte(auth.DigestA1(username, a.Config.Realm, password)), []byte(user.DigestA1)) == 1
		if res.Match {
			res.Note = "credentials valid under realm " + a.Config.Realm
		} else {
			res.Note = "no match under realm " + a.Config.Realm +
				" — wrong password, or the hash was minted under a different realm"
		}
	}
	writeJSON(w, res)
}

func propString(props map[xmlName]any, space, local string) string {
	if props == nil {
		return ""
	}
	s, _ := props[xmlName{Space: space, Local: local}].(string)
	return s
}
