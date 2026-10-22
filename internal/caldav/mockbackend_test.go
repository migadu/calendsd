package caldav_test

import (
	"fmt"
	"sync"
	"testing"

	"calserver/internal/caldav"
)

// TestMockBackendConcurrentUse: every mock method takes the same lock, so
// concurrent use is race-free (run with -race).
func TestMockBackendConcurrentUse(t *testing.T) {
	m := &caldav.MockSchedulingBackend{}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			principal := fmt.Sprintf("principals/u%d", i%2)
			if _, err := m.CreateCalendar(t.Context(), principal, fmt.Sprintf("cal%d", i), nil); err != nil {
				t.Error(err)
			}
			if _, err := m.CalendarsForUser(t.Context(), principal); err != nil {
				t.Error(err)
			}
			if err := m.CreateSchedulingObject(t.Context(), principal, fmt.Sprintf("i%d.ics", i), []byte("x")); err != nil {
				t.Error(err)
			}
			if _, err := m.SchedulingObjects(t.Context(), principal); err != nil {
				t.Error(err)
			}
			if err := m.SetAvailability(t.Context(), principal, []byte("x")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	cals0, _ := m.CalendarsForUser(t.Context(), "principals/u0")
	cals1, _ := m.CalendarsForUser(t.Context(), "principals/u1")
	if len(cals0)+len(cals1) != 8 {
		t.Errorf("calendars = %d, want 8", len(cals0)+len(cals1))
	}
}
