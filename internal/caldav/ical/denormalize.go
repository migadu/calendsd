package ical

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	goical "github.com/emersion/go-ical"
)

// maxDate mirrors sabre's PDO::MAX_DATE ('2038-01-01'): the expansion cap for
// lastOccurrence. Sabre evaluates it in PHP's default timezone; deployments
// pair sabre with UTC, and the shared-DB parity test will catch a mismatch.
var maxDate = time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC)

// ErrNoComponent is returned for calendar data without a VEVENT/VTODO/VJOURNAL.
// Maps to 400 Bad Request, like sabre's getDenormalizedData.
var ErrNoComponent = errors.New("calendar object must have a VJOURNAL, VEVENT or VTODO component")

// Denormalized is the per-object metadata stored in the calendarobjects row,
// matching sabre PDO::getDenormalizedData field for field.
type Denormalized struct {
	// ETag is the md5 hex digest of the raw bytes, without quotes.
	ETag string
	// Size is the payload size in bytes.
	Size int64
	// ComponentType is the first non-VTIMEZONE component name (VEVENT, ...).
	ComponentType string
	// UID is that component's UID value.
	UID string
	// FirstOccurrence/LastOccurrence are Unix timestamps bounding the
	// object's occurrences, only set for VEVENT, clamped to >= 0, with
	// LastOccurrence capped at maxDate for recurring events.
	FirstOccurrence, LastOccurrence *int64
}

// Denormalize computes the metadata sabre stores alongside a calendar object.
// Behavioral contract (PLAN.md §5): any divergence from sabre here makes
// time-range prefilters disagree between the two servers.
func Denormalize(data []byte) (*Denormalized, error) {
	cal, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("parsing calendar data: %w", err)
	}

	var comp *goical.Component
	for _, child := range cal.Children {
		if child.Name != goical.CompTimezone {
			comp = child
			break
		}
	}
	if comp == nil {
		return nil, ErrNoComponent
	}

	sum := md5.Sum(data)
	d := &Denormalized{
		ETag:          hex.EncodeToString(sum[:]),
		Size:          int64(len(data)),
		ComponentType: comp.Name,
	}
	if uid := comp.Props.Get(goical.PropUID); uid != nil {
		d.UID = uid.Value
	}

	if comp.Name == goical.CompEvent {
		first, last, err := eventOccurrenceBounds(comp)
		if err != nil {
			return nil, err
		}
		// Sabre clamps negative (pre-1970) timestamps to zero.
		d.FirstOccurrence = clampUnix(first)
		d.LastOccurrence = clampUnix(last)
	}
	return d, nil
}

func clampUnix(t time.Time) *int64 {
	u := t.Unix()
	if u < 0 {
		u = 0
	}
	return &u
}

// eventOccurrenceBounds computes the [first, last] occurrence window of a
// VEVENT, replicating sabre's rules:
//   - first = DTSTART
//   - non-recurring: last = DTEND, else DTSTART+DURATION, else DTSTART+1day
//     for all-day events, else DTSTART
//   - recurring: infinite rules cap at maxDate; finite rules expand until the
//     first occurrence whose *end* reaches maxDate (or the final occurrence).
func eventOccurrenceBounds(comp *goical.Component) (first, last time.Time, err error) {
	dtstartProp := comp.Props.Get(goical.PropDateTimeStart)
	if dtstartProp == nil {
		return time.Time{}, time.Time{}, errors.New("VEVENT is missing DTSTART")
	}
	dtstart, err := dtstartProp.DateTime(time.UTC)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("parsing DTSTART: %w", err)
	}

	roption, err := comp.Props.RecurrenceRule()
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("parsing RRULE: %w", err)
	}

	if roption == nil {
		end, err := nonRecurringEnd(comp, dtstartProp, dtstart)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		return dtstart, end, nil
	}

	// Recurring event. Infinite rules (no UNTIL, no COUNT) cap at maxDate.
	if roption.Until.IsZero() && roption.Count == 0 {
		return dtstart, maxDate, nil
	}

	set, err := comp.RecurrenceSet(time.UTC)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("building recurrence set: %w", err)
	}
	// Each occurrence's end is its start plus the master duration.
	// TODO(parity): sabre's EventIterator also honors overridden instances
	// (RECURRENCE-ID with their own DTEND); port when the query engine lands.
	dur, err := masterDuration(comp, dtstartProp, dtstart)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}

	last = dtstart.Add(dur)
	next := set.Iterator()
	for {
		start, ok := next()
		if !ok {
			break
		}
		last = start.Add(dur)
		if !last.Before(maxDate) {
			break
		}
	}
	return dtstart, last, nil
}

// nonRecurringEnd resolves the end of a single-instance VEVENT:
// DTEND, else DTSTART+DURATION, else +1 day for all-day starts, else DTSTART.
func nonRecurringEnd(comp *goical.Component, dtstartProp *goical.Prop, dtstart time.Time) (time.Time, error) {
	if dtendProp := comp.Props.Get(goical.PropDateTimeEnd); dtendProp != nil {
		dtend, err := dtendProp.DateTime(time.UTC)
		if err != nil {
			return time.Time{}, fmt.Errorf("parsing DTEND: %w", err)
		}
		return dtend, nil
	}
	if durProp := comp.Props.Get(goical.PropDuration); durProp != nil {
		dur, err := durProp.Duration()
		if err != nil {
			return time.Time{}, fmt.Errorf("parsing DURATION: %w", err)
		}
		return dtstart.Add(dur), nil
	}
	if dtstartProp.ValueType() == goical.ValueDate {
		return dtstart.AddDate(0, 0, 1), nil
	}
	return dtstart, nil
}

// masterDuration is the length of one occurrence, derived from the master
// component: DTEND-DTSTART, else DURATION, else 1 day for all-day, else 0.
func masterDuration(comp *goical.Component, dtstartProp *goical.Prop, dtstart time.Time) (time.Duration, error) {
	end, err := nonRecurringEnd(comp, dtstartProp, dtstart)
	if err != nil {
		return 0, err
	}
	return end.Sub(dtstart), nil
}
