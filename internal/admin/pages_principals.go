// Dashboard, principals, and credential-check page data handlers.

package admin

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"calendsd/internal/acl"
	"calendsd/internal/auth"
	"calendsd/internal/storage"
)

// --- Dashboard (§3.1) ---

type DashboardData struct {
	ConfigLine  string                        `json:"configLine"`
	ReadOnly    bool                          `json:"readOnly"`
	Counts      *storage.AdminCounts          `json:"counts"`
	Changes     []storage.AdminChange         `json:"recentChanges"`
	Diagnostics *storage.AdminAuthDiagnostics `json:"authDiagnostics"`
}

func (a *App) dashboardData(r *http.Request) (any, error) {
	counts, err := a.Store.Counts(r.Context())
	if err != nil {
		return nil, fmt.Errorf("counts: %w", err)
	}
	changes, err := a.Store.RecentChanges(r.Context(), 30)
	if err != nil {
		return nil, fmt.Errorf("recent changes: %w", err)
	}
	diags, err := a.Store.AuthDiagnostics(r.Context())
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

// PrincipalRow is one principal in a listing: its principals columns plus
// whether a users row exists (groups and resources have none and cannot log
// in). The panel and `calctl principal list` share it.
type PrincipalRow struct {
	URI         string `json:"uri"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	HasUser     bool   `json:"hasUser"`
	// Deletion is the purge date (YYYY-MM-DD) when this principal is
	// soft-deleted, empty otherwise. Only calctl fills it in.
	Deletion string `json:"deletion,omitempty"`
}

// PrincipalRows builds the listing rows for principals, marking those that
// diags reports as having no users row.
func PrincipalRows(principals []acl.Principal, diags *storage.AdminAuthDiagnostics) []PrincipalRow {
	noUser := map[string]bool{}
	for _, p := range diags.PrincipalsWithoutUser {
		noUser[p] = true
	}
	rows := make([]PrincipalRow, 0, len(principals))
	for _, p := range principals {
		rows = append(rows, PrincipalRow{
			URI:         p.URI,
			DisplayName: propString(p.Props, "DAV:", "displayname"),
			Email:       propString(p.Props, "http://sabredav.org/ns", "email-address"),
			HasUser:     !noUser[p.URI],
		})
	}
	return rows
}

type PrincipalsData struct {
	Principals []PrincipalRow `json:"principals"`
	// UsersWithoutPrincipal comes along so orphaned credentials are visible
	// on the same page.
	UsersWithoutPrincipal []string `json:"usersWithoutPrincipal"`
}

func (a *App) principalsData(r *http.Request) (any, error) {
	list, err := a.Principals.PrincipalsByPrefix(r.Context(), "principals")
	if err != nil {
		return nil, err
	}
	diags, err := a.Store.AuthDiagnostics(r.Context())
	if err != nil {
		return nil, err
	}
	return &PrincipalsData{
		Principals:            PrincipalRows(list, diags),
		UsersWithoutPrincipal: diags.UsersWithoutPrincipal,
	}, nil
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
	p, err := a.Principals.PrincipalByPath(r.Context(), path)
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
	if d.User, err = a.Store.UserForPrincipal(r.Context(), path); err != nil {
		return nil, err
	}
	if d.User != nil {
		d.DigestWellFormed = WellFormedDigest(d.User.DigestA1)
		if d.DigestWellFormed {
			// A well-formed digesta1 is a reusable HTTP-Digest credential;
			// don't ship it to the browser/JSON. A malformed value is no
			// credential — keep it so the page can show what's actually
			// stored (truncated hash? bcrypt in the wrong column?).
			d.User.DigestA1 = ""
		}
	}

	groups, err := a.Principals.GroupMembership(r.Context(), path)
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
	if d.ReadDelegates, err = a.Principals.GroupMemberSet(r.Context(), path+"/calendar-proxy-read"); err != nil {
		return nil, err
	}
	if d.WriteDelegates, err = a.Principals.GroupMemberSet(r.Context(), path+"/calendar-proxy-write"); err != nil {
		return nil, err
	}

	cals, err := a.Calendars.CalendarsForUser(r.Context(), path)
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
	books, err := a.AddressBooks.AddressBooksForUser(r.Context(), path)
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
		dels, err := a.Writes.Deletions(r.Context())
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

// WellFormedDigest reports whether digest looks like a stored digesta1: 32
// lowercase hex characters. Anything else can never match a login.
func WellFormedDigest(digest string) bool {
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
	user, err := a.Store.UserForPrincipal(r.Context(), "principals/"+username)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, CheckCredentials(user, username, password, a.Config.Realm))
}

// CheckCredentials checks password against user's stored digesta1 (user nil
// means no users row) by recomputing the hash under realm. The panel and
// `calctl principal auth` share it.
func CheckCredentials(user *storage.AdminUserRow, username, password, realm string) CheckCredentialsResult {
	res := CheckCredentialsResult{}
	switch {
	case user == nil:
		res.Note = "no users row — this account cannot log in"
	case !WellFormedDigest(user.DigestA1):
		res.Found = true
		res.Note = "digesta1 is not a 32-char md5 hex — auth always fails"
	default:
		res.Found, res.DigestWellFormed = true, true
		res.Match = subtle.ConstantTimeCompare(
			[]byte(auth.DigestA1(username, realm, password)), []byte(user.DigestA1)) == 1
		if res.Match {
			res.Note = "credentials valid under realm " + realm
		} else {
			res.Note = "no match under realm " + realm +
				" — wrong password, or the hash was minted under a different realm"
		}
	}
	return res
}
