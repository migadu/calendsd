package admin

// Phase B handler tests: privilege simulator (mock-backed production
// plugin chain), scheduling inbox parsing, sync preview, doctor.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"calendsd/internal/acl"
	"calendsd/internal/auth"
	"calendsd/internal/caldav"
	"calendsd/internal/dav"
	"calendsd/internal/dav/mock"
	"calendsd/internal/storage"
)

// Fake store additions for Phase B.

func (fakeStore) ChangeLog(ctx context.Context, kind, collectionID string, limit int) ([]storage.AdminChange, error) {
	return []storage.AdminChange{
		{Kind: kind, CollectionID: collectionID, ObjectURI: "a.ics", SyncToken: 4,
			Operation: 2, LastModified: i64(1750000000)},
		{Kind: kind, CollectionID: collectionID, ObjectURI: "gone.ics", SyncToken: 3,
			Operation: 3},
	}, nil
}

func (fakeStore) PendingShareInvites(ctx context.Context, principalURI string) ([]storage.AdminCalendarInstance, error) {
	if principalURI != "principals/bob" {
		return nil, nil
	}
	return []storage.AdminCalendarInstance{
		{InstanceID: "11", CalendarID: "7", PrincipalURI: "principals/bob",
			URI: "shared-default", Access: 2, ShareHref: "mailto:bob@example.org",
			InviteStatus: 1},
	}, nil
}

func (fakeStore) Doctor(ctx context.Context, scanCap int) (*storage.AdminDoctorReport, error) {
	return &storage.AdminDoctorReport{
		Findings: []storage.AdminFinding{{
			Check: "orphaned-instance", Subject: "instance id=42",
			Detail: "calendarinstances row whose calendars row is gone",
		}},
		ObjectsScanned: 2, CardsScanned: 1,
	}, nil
}

// fakeScheduling serves one REQUEST inbox message.
type fakeScheduling struct{}

const testITIP = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:evt-9\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260401T090000Z\r\nSUMMARY:Planning\r\nORGANIZER;CN=Alice:mailto:alice@example.org\r\nATTENDEE;CN=Bob;PARTSTAT=NEEDS-ACTION;SCHEDULE-STATUS=1.2:mailto:bob@example.org\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func (fakeScheduling) SchedulingObjects(ctx context.Context, principalURI string) ([]caldav.SchedulingObjectInfo, error) {
	if principalURI != "principals/bob" {
		return nil, nil
	}
	return []caldav.SchedulingObjectInfo{{URI: "itip-1.ics",
		PrincipalURI: principalURI, CalendarData: []byte(testITIP)}}, nil
}

func (fakeScheduling) SchedulingObject(ctx context.Context, principalURI, objectURI string) (*caldav.SchedulingObjectInfo, error) {
	return nil, nil
}
func (fakeScheduling) CreateSchedulingObject(ctx context.Context, principalURI, objectURI string, data []byte) error {
	return nil
}
func (fakeScheduling) DeleteSchedulingObject(ctx context.Context, principalURI, objectURI string) error {
	return nil
}

// syncBackend overrides the mock's "sync unsupported" answer.
type syncBackend struct {
	caldav.MockBackend
}

func (s *syncBackend) ChangesForCalendar(ctx context.Context, id caldav.CalendarID, syncToken string, syncLevel, limit int) (*dav.ChangeSet, error) {
	if syncToken == "3" {
		return &dav.ChangeSet{SyncToken: "5", Added: []string{"a.ics"}, Deleted: []string{"gone.ics"}}, nil
	}
	return nil, nil
}

// newMockSimulatorEnv builds a discovery_test-style environment: shared
// mock backends underneath per-principal servers running the production
// acl/caldav plugins. The backend is returned so write tests can inspect
// what came through the chain.
func newMockSimulatorEnv() (func(principal string) *dav.Server, *caldav.MockBackend) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/admin"},
		{URI: "principals/alice"},
		{URI: "principals/bob"},
	}}
	backend := &caldav.MockBackend{Calendars: []caldav.CalendarInfo{{
		ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/alice",
		Props: map[xmlName]any{},
	}}}
	factory := func(principal string) *dav.Server {
		root := mock.NewCollection("root")
		root.Attach(acl.NewPrincipalCollection(principals, "principals"))
		root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
		srv := dav.NewServer("/", root)
		srv.AddPlugin(auth.NewLoggedInPlugin(principal))
		srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
		srv.AddPlugin(&caldav.Plugin{})
		return srv
	}
	return factory, backend
}

func newMockSimulator(principal string) *dav.Server {
	factory, _ := newMockSimulatorEnv()
	return factory(principal)
}

func testAppB() *App {
	a := testApp()
	a.Scheduling = fakeScheduling{}
	a.NewSimulator = newMockSimulator
	a.Calendars = &syncBackend{}
	return a
}

func TestSimulator(t *testing.T) {
	h := testAppB().Handler()

	// The owner holds read+write on her calendar.
	d := getJSON[SimulateData](t, h, "/api/simulate?principal=principals/alice&path=/calendars/alice/default")
	if !d.Requested || d.Status != http.StatusMultiStatus {
		t.Fatalf("simulate = %+v", d)
	}
	privs := strings.Join(d.Privileges, " ")
	if !strings.Contains(privs, "{DAV:}read") || !strings.Contains(privs, "{DAV:}write") {
		t.Fatalf("owner privileges = %v, want read+write", d.Privileges)
	}
	if d.ACLNote != "" || len(d.ACL) == 0 {
		t.Fatalf("acl view = note %q aces %v", d.ACLNote, d.ACL)
	}
	if d.Owner != "principals/alice" {
		t.Fatalf("owner = %q, want principals/alice", d.Owner)
	}
	var ownerACE, adminACE bool
	for _, ace := range d.ACL {
		if strings.Contains(ace.Principal, "principals/alice") &&
			strings.HasPrefix(ace.Privilege, "{DAV:}") {
			ownerACE = true
		}
		if ace.Principal == "principals/admin" && ace.Privilege == "{DAV:}all" {
			adminACE = true
		}
	}
	if !ownerACE || !adminACE {
		t.Fatalf("missing owner/admin ACEs: %+v", d.ACL)
	}

	// A stranger gets no read privilege on someone else's calendar; the
	// status/privilege emptiness is the "why 403" answer.
	s := getJSON[SimulateData](t, h, "/api/simulate?principal=principals/bob&path=/calendars/alice/default")
	if strings.Contains(strings.Join(s.Privileges, " "), "{DAV:}read") {
		t.Fatalf("stranger got read on a foreign calendar: %+v", s)
	}

	// Bare form page carries no results but renders.
	bare := getJSON[SimulateData](t, h, "/api/simulate")
	if bare.Requested || bare.Status != 0 {
		t.Fatalf("bare simulate = %+v", bare)
	}
}

func TestSchedulingInbox(t *testing.T) {
	h := testAppB().Handler()
	d := getJSON[SchedulingData](t, h, "/api/scheduling?principal=principals/bob")
	if d.InboxUnavailable || len(d.Inbox) != 1 {
		t.Fatalf("scheduling = %+v", d)
	}
	item := d.Inbox[0]
	if item.Method != "REQUEST" || item.UID != "evt-9" || item.Summary != "Planning" {
		t.Fatalf("parsed inbox item = %+v", item)
	}
	if len(item.Attendees) != 1 || item.Attendees[0].PartStat != "NEEDS-ACTION" ||
		item.Attendees[0].ScheduleStatus != "1.2" {
		t.Fatalf("attendees = %+v", item.Attendees)
	}
	if len(d.PendingInvites) != 1 || d.PendingInvites[0].PrincipalURI != "principals/bob" {
		t.Fatalf("pending invites = %+v", d.PendingInvites)
	}

	// Another principal: empty inbox, no invites.
	e := getJSON[SchedulingData](t, h, "/api/scheduling?principal=principals/alice")
	if len(e.Inbox) != 0 || len(e.PendingInvites) != 0 {
		t.Fatalf("alice scheduling = %+v", e)
	}
}

func TestSyncPreview(t *testing.T) {
	h := testAppB().Handler()

	// Known token, prefixed form accepted.
	d := getJSON[SyncData](t, h, "/api/sync?kind=calendar&id=7&token=http://sabre.io/ns/sync/3")
	if d.Preview == nil || d.Preview.SyncToken != "5" {
		t.Fatalf("preview = %+v (%s)", d.Preview, d.PreviewNote)
	}
	if len(d.Preview.Added) != 1 || len(d.Preview.Deleted) != 1 {
		t.Fatalf("preview sets = %+v", d.Preview)
	}
	if len(d.Log) != 2 || d.Log[1].LastModified != nil {
		t.Fatalf("log = %+v", d.Log)
	}

	// Unknown token: full-resync note.
	u := getJSON[SyncData](t, h, "/api/sync?kind=calendar&id=7&token=99")
	if u.Preview != nil || !strings.Contains(u.PreviewNote, "full resync") {
		t.Fatalf("unknown token = %+v (%q)", u.Preview, u.PreviewNote)
	}
}

func TestDoctor(t *testing.T) {
	h := testAppB().Handler()
	d := getJSON[DoctorData](t, h, "/api/doctor")
	if len(d.Report.Findings) != 1 || d.Report.Findings[0].Check != "orphaned-instance" {
		t.Fatalf("doctor = %+v", d.Report)
	}
}

func TestPhaseBHTMLPagesRender(t *testing.T) {
	h := testAppB().Handler()
	pages := map[string]string{
		"/simulate": "Privilege simulator",
		"/simulate?principal=principals/alice&path=/calendars/alice/default": "{DAV:}read",
		"/scheduling?principal=principals/bob":                               "Planning",
		"/sync?kind=calendar&id=7&token=3":                                   "gone.ics",
		"/doctor":                                                            "orphaned-instance",
	}
	for path, marker := range pages {
		w := get(t, h, path)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d: %s", path, w.Code, w.Body.String())
			continue
		}
		if !strings.Contains(w.Body.String(), marker) {
			t.Errorf("GET %s: page does not contain %q", path, marker)
		}
	}
}
