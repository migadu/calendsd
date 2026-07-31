// Package admin implements the calserver admin panel (ADMIN-PLAN.md): a
// read-first debugging UI over the shared sabre database. Every page is a
// thin HTML layer over a JSON endpoint; data comes from the same backend
// interfaces the DAV server uses plus the read-only storage.AdminQueries
// aggregates.
package admin

import (
	"net/http"

	"calserver/internal/acl"
	"calserver/internal/caldav"
	"calserver/internal/carddav"
	"calserver/internal/dav"
	"calserver/internal/server"
	"calserver/internal/storage"
)

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
