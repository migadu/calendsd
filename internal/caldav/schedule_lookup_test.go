package caldav_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"calendsd/internal/acl"
	"calendsd/internal/auth"
	"calendsd/internal/caldav"
	"calendsd/internal/dav"
	"calendsd/internal/dav/mock"
)

// indexedBackend is a scheduling backend with a UID index. It counts the
// full-calendar loads and ranged queries the scheduler makes, which the
// index and the time-range prefilter must make the only way in.
type indexedBackend struct {
	*caldav.MockSchedulingBackend
	fullLoads, uidLookups, rangedQueries int
}

func (b *indexedBackend) CalendarObjects(ctx context.Context, id caldav.CalendarID) ([]caldav.ObjectInfo, error) {
	b.fullLoads++
	return b.MockSchedulingBackend.CalendarObjects(ctx, id)
}

func (b *indexedBackend) CalendarQuery(ctx context.Context, id caldav.CalendarID, comp string, start, end time.Time) ([]string, error) {
	if !start.IsZero() && !end.IsZero() {
		b.rangedQueries++
	}
	return b.MockSchedulingBackend.CalendarQuery(ctx, id, comp, start, end)
}

func (b *indexedBackend) CalendarObjectByUID(ctx context.Context, id caldav.CalendarID, uid string) (*caldav.ObjectInfo, error) {
	b.uidLookups++
	uris, err := b.MockSchedulingBackend.CalendarQuery(context.Background(), id, "", time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}
	for _, uri := range uris {
		obj, err := b.MockSchedulingBackend.CalendarObject(context.Background(), id, uri)
		if err == nil && obj != nil && strings.Contains(string(obj.CalendarData), "\r\nUID:"+uid+"\r\n") {
			return obj, nil
		}
	}
	return nil, nil
}

// TestSchedulingUsesIndexedLookups: delivering to a resource finds its
// existing copy by UID and checks conflicts with a ranged query, never by
// loading every object of a calendar.
func TestSchedulingUsesIndexedLookups(t *testing.T) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1", Props: map[xmlNM]any{
			{Space: "http://sabredav.org/ns", Local: "email-address"}: "user1@example.org",
		}},
		{URI: "principals/resource1", Props: map[xmlNM]any{
			{Space: "http://sabredav.org/ns", Local: "email-address"}: "resource1@example.org",
		}},
	}}
	backend := &indexedBackend{MockSchedulingBackend: &caldav.MockSchedulingBackend{}}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1"},
		{ID: caldav.CalendarID{Calendar: "2"}, URI: "default", PrincipalURI: "principals/resource1"},
	}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&caldav.SchedulePlugin{
		AutoSchedule: func(uri string) bool { return strings.HasPrefix(uri, "principals/resource") },
	})

	for _, times := range [][2]string{
		{"20260901T100000Z", "20260901T110000Z"},
		{"20260901T120000Z", "20260901T130000Z"}, // reschedule the same booking
	} {
		req := httptest.NewRequest("PUT", "/calendars/user1/default/b1.ics",
			strings.NewReader(bookingICS("book-1", times[0], times[1])))
		req.Header.Set("Content-Type", "text/calendar")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != 201 && w.Code != 204 {
			t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
		}
	}

	resource := backend.Objects[caldav.CalendarID{Calendar: "2"}]
	if len(resource) != 1 {
		t.Fatalf("resource calendar has %d objects, want the one rescheduled copy", len(resource))
	}
	for _, obj := range resource {
		if !strings.Contains(string(obj.CalendarData), "DTSTART:20260901T120000Z") {
			t.Errorf("resource copy not rescheduled:\n%s", obj.CalendarData)
		}
	}
	if backend.fullLoads != 0 {
		t.Errorf("full calendar loads = %d, want 0", backend.fullLoads)
	}
	if backend.uidLookups == 0 || backend.rangedQueries == 0 {
		t.Errorf("uid lookups = %d, ranged queries = %d; want both used", backend.uidLookups, backend.rangedQueries)
	}
}
