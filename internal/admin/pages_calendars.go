// Calendar and address book inspector page data handlers.

package admin

import (
	"fmt"
	"net/http"
	"strings"

	"calserver/internal/storage"
)

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
	meta, err := a.Store.CalendarMeta(r.Context(), id)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return nil, fmt.Errorf("%w: calendar %s", errNotFound, id)
	}
	instances, err := a.Store.CalendarInstances(r.Context(), id)
	if err != nil {
		return nil, err
	}
	d := &CalendarDetail{ID: id, Meta: meta}
	for _, in := range instances {
		path := collectionPath("calendars", in.PrincipalURI, in.URI)
		props, err := a.Store.PropertyRows(r.Context(), path)
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
	book, err := a.Store.AddressBook(r.Context(), id)
	if err != nil {
		return nil, err
	}
	if book == nil {
		return nil, fmt.Errorf("%w: address book %s", errNotFound, id)
	}
	path := collectionPath("addressbooks", book.PrincipalURI, book.URI)
	props, err := a.Store.PropertyRows(r.Context(), path)
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
	id, err := a.Store.LookupCalendar(r.Context(), principal, uri)
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
	id, err := a.Store.LookupAddressBook(r.Context(), principal, uri)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("%w: no address book %s/%s", errNotFound, principal, uri)
	}
	return id, nil
}
