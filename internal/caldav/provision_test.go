package caldav

import (
	"context"
	"encoding/xml"
	"errors"
	"testing"
)

// losingBackend plays the loser of the first-request race: another request
// creates the default calendar first, so this insert fails on the unique URI.
type losingBackend struct{ *MockBackend }

func (b losingBackend) CreateCalendar(ctx context.Context, principalURI, uri string, props map[xml.Name]any) (CalendarID, error) {
	if _, err := b.MockBackend.CreateCalendar(ctx, principalURI, uri, props); err != nil {
		return CalendarID{}, err
	}
	return CalendarID{}, errors.New("duplicate key value violates unique constraint")
}

func TestProvisionLosingRaceUsesWinnersCalendar(t *testing.T) {
	home := NewCalendarHome(losingBackend{&MockBackend{}}, "principals/u")
	home.autoProvision = true
	exists, err := home.ChildExists(t.Context(), "calendar")
	if err != nil || !exists {
		t.Fatalf("ChildExists(calendar) = %v, %v; want true, nil", exists, err)
	}
}
