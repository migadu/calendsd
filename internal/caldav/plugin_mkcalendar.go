// MKCALENDAR handling (RFC 4791 §5.3.1) and its request-body parsing.

package caldav

import (
	"encoding/xml"
	"io"
	"net/http"

	"github.com/migadu/calendsd/internal/dav"
	davxml "github.com/migadu/calendsd/internal/dav/xml"
	"github.com/migadu/calendsd/internal/pathutil"
)

// mkcalendar creates a calendar collection (RFC 4791 §5.3.1).
func (p *Plugin) mkcalendar(w http.ResponseWriter, r *http.Request) error {
	path, err := p.server.Path(r)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	// iCal abuses MKCALENDAR to create subscriptions: a resourcetype in
	// the body overrides the default.
	resourceTypes := []xml.Name{
		{Space: davxml.NSDav, Local: "collection"}, nameCalendar,
	}
	props := map[xml.Name]any{}
	if len(body) > 0 {
		types, parsed, err := davxml.ParseMkcalendar(body)
		if err != nil {
			return dav.ErrBadRequest("%s", err)
		}
		if types != nil {
			resourceTypes = types
		}
		props = parsed
	}

	if exists, err := p.server.Tree.NodeExists(r.Context(), path); err != nil {
		return err
	} else if exists {
		return dav.ErrMethodNotAllowed("The resource you tried to create already exists")
	}
	parentPath, name := pathutil.Split(path)
	parent, err := p.server.Tree.NodeForPath(r.Context(), parentPath)
	if err != nil {
		return err
	}
	home, isHome := parent.(*CalendarHome)
	if !isHome {
		return dav.ErrMethodNotAllowed("You can only create calendars inside a calendar home")
	}
	if err := home.CreateExtendedCollection(r.Context(), name, resourceTypes, props); err != nil {
		return err
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
	return nil
}
