package ical

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	goical "github.com/emersion/go-ical"
)

// maxDate mirrors sabre's PDO::MAX_DATE ('2038-01-01'): the expansion cap for
// lastOccurrence. Sabre evaluates it in PHP's default timezone; deployments
// pair sabre with UTC, and the shared-DB parity test will catch a mismatch.
var maxDate = time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC)

// maxRecurrences bounds every recurrence-set iteration, mirroring sabre
// vobject's RRuleIterator::maxRecurrences. Without it a crafted rule
// (FREQ=SECONDLY, open-ended query range) would iterate billions of times
// and pin a core — a remotely triggerable DoS. On reaching the cap we stop
// as if the rule ended, which is what sabre does.
const maxRecurrences = 3500

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
	// Resolve Outlook-style non-IANA TZIDs from any embedded VTIMEZONE so the
	// occurrence-bounds computation can parse their date-times.
	ResolveNonIANATZIDs(cal)

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
	// Every recurring component, not only the VEVENT whose bounds are
	// computed below (which skips the recurrence set for an infinite rule):
	// a rule expansion would panic on must not be stored.
	for _, child := range cal.Children {
		if err := checkRecurrence(child, time.UTC); err != nil {
			return nil, err
		}
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
		// Overridden instances can extend the object's last occurrence
		// beyond what the master's RRULE alone implies.
		first, last, err := eventOccurrenceBounds(comp, overridesOf(cal, comp, time.UTC))
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
func eventOccurrenceBounds(comp *goical.Component, ovByRID map[int64]*goical.Component) (first, last time.Time, err error) {
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

	if !IsRecurring(comp) {
		end, err := eventEnd(comp, dtstartProp, dtstart, time.UTC)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		return dtstart, end, nil
	}

	// Recurring event. Infinite rules (no UNTIL, no COUNT) cap at maxDate.
	if roption != nil && roption.Until.IsZero() && roption.Count == 0 {
		return dtstart, maxDate, nil
	}

	set, err := recurrenceSet(comp, time.UTC)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("building recurrence set: %w", err)
	}
	// A plain occurrence's end is its start plus the master duration; an
	// overridden instance (RECURRENCE-ID) uses its own end, which can push
	// the last occurrence later. Mirrors sabre's EventIterator::getDtEnd.
	endOf, err := occurrenceEnd(comp, dtstartProp, dtstart)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}

	type occurrence struct{ start, end time.Time }
	var occs []occurrence
	consumed := make(map[int64]bool, len(ovByRID))
	next := set.Iterator()
	capped := false
	for i := 0; ; i++ {
		start, ok := next()
		if !ok {
			break
		}
		if i == maxRecurrences {
			capped = true
			break
		}
		if ov, has := ovByRID[start.Unix()]; has {
			if os, oe, e := overrideStartEnd(ov); e == nil {
				occs = append(occs, occurrence{os, oe})
				consumed[start.Unix()] = true
				continue
			}
		}
		occs = append(occs, occurrence{start, endOf(start)})
	}
	// Overrides whose RECURRENCE-ID fell outside the iterated set (moved out
	// of range, or an RDATE-only instance) still bound the object.
	for rid, ov := range ovByRID {
		if consumed[rid] {
			continue
		}
		if os, oe, e := overrideStartEnd(ov); e == nil {
			occs = append(occs, occurrence{os, oe})
		}
	}
	if len(occs) == 0 {
		return dtstart, endOf(dtstart), nil
	}

	// Walk occurrences in start order, taking each end until one reaches
	// maxDate — sabre's getDtEnd loop.
	slices.SortFunc(occs, func(a, b occurrence) int { return a.start.Compare(b.start) })
	last = occs[0].end
	for _, o := range occs {
		last = o.end
		if !last.Before(maxDate) {
			break
		}
	}
	// The series goes on past the cap, so the walk above stopped short of
	// its real end: bound it by UNTIL instead, or by maxDate for a COUNT,
	// as for an infinite rule.
	if capped {
		bound := maxDate
		if roption != nil && !roption.Until.IsZero() {
			if u := endOf(roption.Until); u.Before(maxDate) {
				bound = u
			}
		}
		if bound.After(last) {
			last = bound
		}
	}
	return dtstart, last, nil
}

// overrideStartEnd returns an overridden instance's own start and end.
func overrideStartEnd(ov *goical.Component) (start, end time.Time, err error) {
	dsProp := ov.Props.Get(goical.PropDateTimeStart)
	if dsProp == nil {
		return time.Time{}, time.Time{}, errors.New("override VEVENT is missing DTSTART")
	}
	start, err = dsProp.DateTime(time.UTC)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err = eventEnd(ov, dsProp, start, time.UTC)
	return start, end, err
}
