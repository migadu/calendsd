package caldav_test

// Audit 3.5: the durable handler decides the send path from the job's
// declared identity, never from whether a credential happens to be present.
// The credential purge NULLs the secret on an overdue 'available' job; before
// this the handler then relayed the message unmetered and still counted it
// as sent_as_user.

import (
	"context"
	"strings"
	"testing"

	"github.com/migadu/calendsd/internal/caldav"
	"github.com/migadu/calendsd/internal/jobs"
	"github.com/migadu/calendsd/internal/metrics"
)

func identityPlugin(mailer *spyMailer, um *spyUserMailer, wb *writeBackRec) *caldav.IMipPlugin {
	return &caldav.IMipPlugin{SenderEmail: "server@example.org", Mailer: mailer, Submission: um,
		Metrics: metrics.New("t"), WriteBack: wb.fn()}
}

// A per-user job whose credential was purged is discarded with a terminal
// 5.1 on the object; the relay mailer is never touched.
func TestDurableHandlerPurgedCredentialIsDiscardedNotRelayed(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"explicit identity", map[string]any{
			"identity": "user", "envelope_from": "alice@example.org", "username": "alice",
			"recipient": "a@x.test", "method": "REQUEST", "uid": "u1", "outcome": "sent_as_user",
			"object_path": "cal/e.ics"}},
		{"legacy row without identity (classified by username)", map[string]any{
			"envelope_from": "alice@example.org", "username": "alice",
			"recipient": "a@x.test", "method": "REQUEST", "uid": "u1", "outcome": "sent_as_user",
			"object_path": "cal/e.ics"}},
	} {
		mailer, um, wb := &spyMailer{}, &spyUserMailer{}, &writeBackRec{}
		p := identityPlugin(mailer, um, wb)
		job := durableJob(t, tc.args, nil, 1, 6)

		err := p.DurableHandler(context.Background(), job)
		if !jobs.IsDiscard(err) {
			t.Fatalf("%s: handler returned %v, want Discard", tc.name, err)
		}
		if mailer.to != nil {
			t.Errorf("%s: relay mailer was used for a per-user job: to=%v", tc.name, mailer.to)
		}
		if um.called {
			t.Errorf("%s: SendAs was attempted without a credential", tc.name)
		}
		if !wb.called || !strings.HasPrefix(wb.status, "5.1;") || !strings.Contains(wb.status, "Credential expired") {
			t.Errorf("%s: write-back = %+v, want a terminal 5.1 credential-expired status", tc.name, wb)
		}
	}
}

// A relay job must not carry a credential; if one does, it is discarded.
func TestDurableHandlerRelayJobWithCredentialIsDiscarded(t *testing.T) {
	mailer, um, wb := &spyMailer{}, &spyUserMailer{}, &writeBackRec{}
	p := identityPlugin(mailer, um, wb)
	job := durableJob(t, map[string]any{
		"identity": "relay", "envelope_from": "server@example.org",
		"recipient": "a@x.test", "method": "REQUEST", "uid": "u1", "outcome": "sent", "object_path": "cal/e.ics",
	}, []byte("stray"), 1, 6)

	if err := p.DurableHandler(context.Background(), job); !jobs.IsDiscard(err) {
		t.Fatalf("handler returned %v, want Discard", err)
	}
	if mailer.to != nil || um.called {
		t.Error("no send may happen for a mis-labelled job")
	}
}

// The `calctl imip resend --relay` override rewrites the identity to relay and
// drops the credential; that combination still relays.
func TestDurableHandlerRelayOverrideStillRelays(t *testing.T) {
	mailer, um, wb := &spyMailer{}, &spyUserMailer{}, &writeBackRec{}
	p := identityPlugin(mailer, um, wb)
	job := durableJob(t, map[string]any{
		"identity": "relay", "envelope_from": "alice@example.org", "username": "alice",
		"recipient": "a@x.test", "method": "REQUEST", "uid": "u1", "outcome": "sent_as_user", "object_path": "cal/e.ics",
	}, nil, 1, 6)

	if err := p.DurableHandler(context.Background(), job); err != nil {
		t.Fatalf("handler returned %v, want nil", err)
	}
	if mailer.from != "server@example.org" || len(mailer.to) != 1 || um.called {
		t.Errorf("relay override: mailer from=%q to=%v SendAs=%v, want server relay only", mailer.from, mailer.to, um.called)
	}
	if !wb.called || !strings.HasPrefix(wb.status, "1.1;") {
		t.Errorf("write-back = %+v, want statusSent", wb)
	}
}
