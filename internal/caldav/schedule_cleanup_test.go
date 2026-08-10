package caldav

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"calserver/internal/caldav/ical"
)

// A PUT that stashes a deferred send in InterceptWrite but never reaches
// AfterWrite (e.g. the persist failed) must not orphan the entry: when the
// request ends, the request-scoped cleanup drops it.
func TestPendingSendCleanedWhenRequestEnds(t *testing.T) {
	p := &SchedulePlugin{}
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("PUT", "/calendars/u/default/e.ics", nil).WithContext(ctx)

	p.deferSend(r, &ical.Message{Recipient: "mailto:guest@example.com"}, "calendars/u/default/e.ics")
	if _, ok := p.pending.Load(r); !ok {
		t.Fatal("deferSend did not stash the pending send")
	}

	cancel() // the request ends without AfterWrite draining the stash

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := p.pending.Load(r); !ok {
			return // cleaned up
		}
		if time.Now().After(deadline) {
			t.Fatal("pending send was not cleaned up after the request ended")
		}
		time.Sleep(time.Millisecond)
	}
}
