package caldav

// Audit 4.3: the SCHEDULE-STATUS write-back re-read the object under its own
// lock, but a user PUT never takes that lock, so a save landing between the
// write-back's read and its Put was overwritten. The write is now conditional
// on the ETag that was read; a mismatch re-reads the user's revision and
// applies only the attendee parameter to it.

import (
	"context"
	"encoding/xml"
	"strings"
	"testing"

	"calendsd/internal/acl"
	"calendsd/internal/dav"
	"calendsd/internal/dav/mock"
	davxml "calendsd/internal/dav/xml"
)

const writeBackEvent = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:wb-1\r\nDTSTAMP:20260101T000000Z\r\n" +
	"DTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nSUMMARY:Original\r\n" +
	"ORGANIZER:mailto:alice@example.org\r\n" +
	"ATTENDEE:mailto:bob@example.org\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

// racingBackend simulates a user PUT landing inside the write-back's
// read→update window: the first conditional update first stores the user's
// revision, then reports the mismatch. Later calls behave normally.
type racingBackend struct {
	*MockSchedulingBackend
	calls       int
	alwaysRace  bool
	userVersion []byte
}

func (b *racingBackend) UpdateCalendarObjectIfMatch(ctx context.Context, id CalendarID, uri string, data []byte, expected string) (string, error) {
	b.calls++
	if b.calls == 1 || b.alwaysRace {
		if _, err := b.MockSchedulingBackend.CreateCalendarObject(ctx, id, uri, b.userVersion); err != nil {
			return "", err
		}
		return "", ErrETagMismatch
	}
	return b.MockSchedulingBackend.UpdateCalendarObjectIfMatch(ctx, id, uri, data, expected)
}

func writeBackServer(t *testing.T, data string, alwaysRace bool) (*SchedulePlugin, *racingBackend) {
	t.Helper()
	email := xml.Name{Space: davxml.NSSabreDAV, Local: "email-address"}
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/alice", Props: map[xml.Name]any{email: "alice@example.org"}},
		{URI: "principals/bob", Props: map[xml.Name]any{email: "bob@example.org"}},
	}}
	inner := &MockSchedulingBackend{}
	inner.Calendars = []CalendarInfo{{ID: CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/alice"}}
	inner.Objects = map[CalendarID]map[string]*ObjectInfo{
		{Calendar: "1"}: {"e.ics": &ObjectInfo{URI: "e.ics", CalendarData: []byte(data)}},
	}
	backend := &racingBackend{MockSchedulingBackend: inner, alwaysRace: alwaysRace,
		userVersion: []byte(strings.Replace(data, "SUMMARY:Original", "SUMMARY:Edited by user", 1))}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	schedule := &SchedulePlugin{}
	srv.AddPlugin(schedule)
	return schedule, backend
}

func TestWriteBackPreservesConcurrentUserEdit(t *testing.T) {
	schedule, backend := writeBackServer(t, writeBackEvent, false)
	schedule.patchScheduleStatus(context.Background(), "calendars/alice/default/e.ics", "REQUEST", "mailto:bob@example.org", statusSent)

	got := string(backend.Objects[CalendarID{Calendar: "1"}]["e.ics"].CalendarData)
	if !strings.Contains(got, "SUMMARY:Edited by user") {
		t.Errorf("user's concurrent edit was overwritten:\n%s", got)
	}
	if !strings.Contains(got, "SCHEDULE-STATUS=1.1") {
		t.Errorf("schedule status not applied to the user's revision:\n%s", got)
	}
	if backend.calls != 2 {
		t.Errorf("conditional update called %d times, want 2 (mismatch, then success)", backend.calls)
	}
}

func TestWriteBackGivesUpAfterRepeatedRaces(t *testing.T) {
	schedule, backend := writeBackServer(t, writeBackEvent, true)
	schedule.patchScheduleStatus(context.Background(), "calendars/alice/default/e.ics", "REQUEST", "mailto:bob@example.org", statusSent)
	if backend.calls != writeBackAttempts {
		t.Errorf("conditional update called %d times, want %d then give up", backend.calls, writeBackAttempts)
	}
	got := string(backend.Objects[CalendarID{Calendar: "1"}]["e.ics"].CalendarData)
	if strings.Contains(got, "SCHEDULE-STATUS") {
		t.Errorf("a status landed despite every attempt racing:\n%s", got)
	}
}

func TestWriteBackDroppedWhenAttendeeRemoved(t *testing.T) {
	without := strings.Replace(writeBackEvent, "ATTENDEE:mailto:bob@example.org\r\n", "", 1)
	schedule, backend := writeBackServer(t, without, false)
	schedule.patchScheduleStatus(context.Background(), "calendars/alice/default/e.ics", "REQUEST", "mailto:bob@example.org", statusSent)
	if backend.calls != 0 {
		t.Errorf("update attempted %d times for an attendee no longer on the object", backend.calls)
	}
	if got := string(backend.Objects[CalendarID{Calendar: "1"}]["e.ics"].CalendarData); got != without {
		t.Errorf("object changed:\n%s", got)
	}
}
