// White-box: the AfterWrite flush of a queued resource REPLY must exit through
// the resource relay. This is in package caldav because the scenario is not
// reachable over HTTP today — a resource REPLY only goes external when the
// organizer is remote, and no inbound path (iMIP receive, outbox POST) can
// deliver a remote-organizer REQUEST to a resource yet — so the queue is
// primed directly via queueReply, exactly what autoProcess does.

package caldav

import (
	"context"
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"

	goical "github.com/emersion/go-ical"

	"calserver/internal/acl"
	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	davxml "calserver/internal/dav/xml"
)

type recordingMailer struct {
	from string
	to   []string
	msg  []byte
}

func (m *recordingMailer) Send(from string, to []string, msg []byte) error {
	m.from, m.to, m.msg = from, to, msg
	return nil
}

type recordingUserMailer struct{ called bool }

func (m *recordingUserMailer) SendAs(_, _, _ string, _ []string, _ []byte) error {
	m.called = true
	return nil
}

// TestAfterWriteFlushesResourceReplyViaRelay drives the real queued-reply
// mechanism: a REPLY whose sender is an auto-schedule resource, queued during
// the organizer's request (as autoProcess does) and flushed by AfterWrite,
// must reach the shared relay — not the per-user submission path, and not the
// not_authorized_sender refusal — even though the request principal is the
// human whose PUT triggered it.
func TestAfterWriteFlushesResourceReplyViaRelay(t *testing.T) {
	email := xml.Name{Space: davxml.NSSabreDAV, Local: "email-address"}
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/alice", Props: map[xml.Name]any{email: "alice@example.org"}},
		{URI: "principals/room1", Props: map[xml.Name]any{email: "room1@example.org"}},
	}}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	srv := dav.NewServer("/", root)

	schedule := &SchedulePlugin{AutoSchedule: func(uri string) bool { return uri == "principals/room1" }}
	srv.AddPlugin(schedule)
	relay := &recordingMailer{}
	sub := &recordingUserMailer{}
	imip := &IMipPlugin{SenderEmail: "server@example.org", Mailer: relay, Submission: sub}
	imip.UseScheduler(schedule)
	srv.AddPlugin(imip)

	// The triggering request is the organizer's (alice, Basic-authenticated).
	ctx := dav.ContextWithPrincipal(context.Background(), "principals/alice")
	dav.SetSubmissionSecret(ctx, "hunter2")
	r := httptest.NewRequest("PUT", "/calendars/room1/default/e1.ics", nil).WithContext(ctx)
	r.SetBasicAuth("alice@example.org", "hunter2")

	ev := &goical.Component{Name: "VEVENT", Props: goical.Props{}}
	ev.Props.SetText("SUMMARY", "Board meeting")
	cal := goical.NewCalendar()
	cal.Children = append(cal.Children, ev)
	reply := &ical.Message{
		Method: "REPLY", UID: "aw-1",
		Sender:            "mailto:room1@example.org",
		Recipient:         "mailto:organizer@remote.example",
		SignificantChange: true, Calendar: cal,
	}
	schedule.queueReply(r, reply)
	schedule.AfterWrite(r, "calendars/room1/default/e1.ics")

	if sub.called {
		t.Error("per-user submission used for a resource reply; must relay")
	}
	if relay.msg == nil {
		t.Fatalf("relay not used for the flushed resource reply; status=%q", reply.ScheduleStatus)
	}
	if relay.from != "server@example.org" {
		t.Errorf("relay envelope sender = %q, want server@example.org", relay.from)
	}
	if len(relay.to) != 1 || relay.to[0] != "organizer@remote.example" {
		t.Errorf("relay recipients = %v, want [organizer@remote.example]", relay.to)
	}
	if !strings.HasPrefix(reply.ScheduleStatus, "1.1") {
		t.Errorf("status = %q, want 1.1 (sent)", reply.ScheduleStatus)
	}
	// The queue must be drained: a second flush sends nothing again.
	relay.msg = nil
	schedule.AfterWrite(r, "calendars/room1/default/e1.ics")
	if relay.msg != nil {
		t.Error("second AfterWrite re-sent the queued reply; queue not drained")
	}
}
