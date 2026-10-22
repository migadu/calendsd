package caldav_test

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	goical "github.com/emersion/go-ical"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"calendsd/internal/caldav"
	"calendsd/internal/caldav/ical"
	"calendsd/internal/metrics"
)

type failingMailer struct{}

func (failingMailer) Send(from string, to []string, msg []byte) error {
	return errors.New("smtp down")
}

// TestIMipDeliveryOutcomes pins the schedule-status → outcome-label mapping:
// only smtp_error is alert-worthy, so a user inviting a typo'd address must
// not land in the same series as a broken SMTP relay.
func TestIMipDeliveryOutcomes(t *testing.T) {
	newMsg := func(recipient string, significant bool) *ical.Message {
		ev := &goical.Component{Name: "VEVENT", Props: goical.Props{}}
		ev.Props.SetText("SUMMARY", "Lunch")
		cal := goical.NewCalendar()
		cal.Children = append(cal.Children, ev)
		return &ical.Message{
			Method:            "REQUEST",
			Sender:            "mailto:organizer@example.org",
			Recipient:         recipient,
			SignificantChange: significant,
			Calendar:          cal,
		}
	}

	req := httptest.NewRequest("POST", "/", nil)
	set := metrics.New("test")
	sent := &caldav.IMipPlugin{SenderEmail: "s@example.org", Mailer: &spyMailer{}, Metrics: set}
	sent.DeliverExternal(req, newMsg("mailto:guest@example.com", true))  // sent
	sent.DeliverExternal(req, newMsg("mailto:guest@example.com", false)) // skipped
	sent.DeliverExternal(req, newMsg("mailto:not an address", true))     // invalid_recipient
	sent.DeliverExternal(req, newMsg("urn:uuid:1234", true))             // unsupported (non-mailto)

	noMailer := &caldav.IMipPlugin{SenderEmail: "s@example.org", Metrics: set}
	noMailer.DeliverExternal(req, newMsg("mailto:guest@example.com", true)) // no_mailer

	broken := &caldav.IMipPlugin{SenderEmail: "s@example.org", Mailer: failingMailer{}, Metrics: set}
	broken.DeliverExternal(req, newMsg("mailto:guest@example.com", true)) // smtp_error

	want := `
# HELP calserver_imip_deliveries_total External iTIP deliveries by outcome; smtp_error, auth_error, and retry_exhausted are alert-worthy (delivery/auth infrastructure failures). rate_limited, not_authorized_sender, and no_credential are expected client-side outcomes. A message that is queued for retry counts once as queued and once more at its terminal outcome.
# TYPE calserver_imip_deliveries_total counter
calserver_imip_deliveries_total{outcome="invalid_recipient"} 1
calserver_imip_deliveries_total{outcome="no_mailer"} 1
calserver_imip_deliveries_total{outcome="sent"} 1
calserver_imip_deliveries_total{outcome="skipped"} 1
calserver_imip_deliveries_total{outcome="smtp_error"} 1
calserver_imip_deliveries_total{outcome="unsupported"} 1
`
	if err := testutil.GatherAndCompare(set.Registry(), strings.NewReader(want),
		"calserver_imip_deliveries_total"); err != nil {
		t.Error(err)
	}
	// The send histogram observed the two actual SMTP calls (sent + smtp_error).
	if n := testutil.CollectAndCount(set.Registry(), "calserver_imip_delivery_duration_seconds"); n != 1 {
		t.Errorf("imip duration series = %d, want 1", n)
	}
}
