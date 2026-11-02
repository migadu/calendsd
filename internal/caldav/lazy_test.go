package caldav_test

// A calendar listed over a MetaLister carries no payloads: the Depth:1
// PROPFIND answers getetag and getcontentlength from the row's metadata, and
// an object's data is fetched once, only when something reads it
// (docs/slow-queries.md, item 3).

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/auth"
	"github.com/migadu/calendsd/internal/caldav"
	"github.com/migadu/calendsd/internal/caldav/ical"
	"github.com/migadu/calendsd/internal/dav"
	"github.com/migadu/calendsd/internal/dav/mock"
)

// metaMock is the mock backend as a MetaLister: the listing strips the
// payload the way storage's metadata query never selects it, and every
// single-object fetch is counted.
type metaMock struct {
	*caldav.MockSchedulingBackend
	fetches atomic.Int32
}

func (m *metaMock) CalendarObjectsMeta(ctx context.Context, id caldav.CalendarID) ([]caldav.ObjectInfo, error) {
	objs, err := m.CalendarObjects(ctx, id)
	for i := range objs {
		objs[i].ETag = objs[i].QuotedETag()
		objs[i].Size = int64(len(objs[i].CalendarData))
		objs[i].CalendarData = nil
	}
	return objs, err
}

func (m *metaMock) CalendarObject(ctx context.Context, id caldav.CalendarID, uri string) (*caldav.ObjectInfo, error) {
	m.fetches.Add(1)
	return m.MockSchedulingBackend.CalendarObject(ctx, id, uri)
}

// lazyServer: user1 owns calendar "1" holding one CLASS:PRIVATE event; user2
// holds a read-only share of it as calendar "2".
func lazyServer(loggedIn string) (*dav.Server, *metaMock) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1"}, {URI: "principals/user2"},
	}}
	backend := &metaMock{MockSchedulingBackend: &caldav.MockSchedulingBackend{}}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1"},
		{ID: caldav.CalendarID{Calendar: "2"}, URI: "shared", PrincipalURI: "principals/user2",
			Props: map[xmlNM]any{{Space: "http://sabredav.org/ns", Local: "read-only"}: "1"}},
	}
	obj := &caldav.ObjectInfo{URI: "dentist.ics", CalendarData: []byte(privateEvent)}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{
		{Calendar: "1"}: {"dentist.ics": obj},
		{Calendar: "2"}: {"dentist.ics": obj},
	}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin(loggedIn))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	return srv, backend
}

func propfindDepth1(t *testing.T, srv *dav.Server, path, props string) string {
	t.Helper()
	body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop>` +
		props + `</d:prop></d:propfind>`
	req := httptest.NewRequest("PROPFIND", path, strings.NewReader(body))
	req.Header.Set("Depth", "1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 207 {
		t.Fatalf("PROPFIND %s = %d:\n%s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func TestListingServesMetadataWithoutPayload(t *testing.T) {
	srv, backend := lazyServer("principals/user1")
	body := propfindDepth1(t, srv, "/calendars/user1/default/", `<d:getetag/><d:getcontentlength/>`)

	// The quotes come out XML-escaped; the md5 between them is the check.
	want := strings.Trim((&caldav.ObjectInfo{CalendarData: []byte(privateEvent)}).QuotedETag(), `"`)
	if !strings.Contains(body, want) {
		t.Errorf("getetag %s missing from:\n%s", want, body)
	}
	if size := "<d:getcontentlength>" + itoa(len(privateEvent)) + "</d:getcontentlength>"; !strings.Contains(body, size) {
		t.Errorf("%s missing from:\n%s", size, body)
	}
	if n := backend.fetches.Load(); n != 0 {
		t.Errorf("listing getetag/getcontentlength fetched %d payloads, want 0", n)
	}
}

// A read-only sharee's getcontentlength is the length of the redacted body
// they would GET, so the payload is read for them.
func TestListingSizeOnReadOnlyShareIsRedactedLength(t *testing.T) {
	srv, backend := lazyServer("principals/user2")
	body := propfindDepth1(t, srv, "/calendars/user2/shared/", `<d:getcontentlength/>`)

	redacted := len(ical.RedactRestricted([]byte(privateEvent)))
	if redacted == len(privateEvent) {
		t.Fatal("fixture: redaction changed nothing")
	}
	if size := "<d:getcontentlength>" + itoa(redacted) + "</d:getcontentlength>"; !strings.Contains(body, size) {
		t.Errorf("%s missing from:\n%s", size, body)
	}
	if n := backend.fetches.Load(); n != 1 {
		t.Errorf("read-only listing fetched %d payloads, want 1", n)
	}
}

// Asking for calendar-data in the listing loads each object once and serves
// the full payload.
func TestListingCalendarDataLoadsOnce(t *testing.T) {
	srv, backend := lazyServer("principals/user1")
	body := propfindDepth1(t, srv, "/calendars/user1/default/", `<d:getcontentlength/><c:calendar-data/>`)
	if !strings.Contains(body, "SUMMARY:Dentist") {
		t.Errorf("calendar-data missing from:\n%s", body)
	}
	if n := backend.fetches.Load(); n != 1 {
		t.Errorf("fetched %d payloads, want 1", n)
	}
}

// An object deleted between the listing and the read has no body to serve:
// GET reports not found, the data property is absent, and nothing is served
// empty.
func TestLazyObjectDeletedBeforeRead(t *testing.T) {
	srv, backend := lazyServer("principals/user1")
	ctx := t.Context()
	node, err := srv.Tree.NodeForPath(ctx, "calendars/user1/default")
	if err != nil {
		t.Fatal(err)
	}
	children, err := node.(dav.Collection).Children(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 {
		t.Fatalf("%d children, want 1", len(children))
	}
	obj := children[0].(*caldav.CalendarObject)
	if err := backend.DeleteCalendarObject(ctx, caldav.CalendarID{Calendar: "1"}, "dentist.ics"); err != nil {
		t.Fatal(err)
	}

	_, err = obj.Get(ctx)
	var de *dav.Error
	if !errors.As(err, &de) || de.Status != 404 {
		t.Errorf("Get after delete = %v, want 404", err)
	}
	if data := obj.Data(); data != nil {
		t.Errorf("Data after delete = %q, want nil", data)
	}
	if n := backend.fetches.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1 (the failure is remembered)", n)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
