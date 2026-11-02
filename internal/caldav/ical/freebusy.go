package ical

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"
)

// busyPeriod is one busy interval with its FBTYPE.
type busyPeriod struct {
	start, end time.Time
	fbType     string
}

// GenerateFreeBusy computes a VFREEBUSY response for the objects' VEVENTs
// within [start, end). Subset of vobject's FreeBusyGenerator: transparent and
// cancelled events are skipped, TENTATIVE maps to BUSY-TENTATIVE, recurring
// events expand within the range with each override replacing its
// occurrence, and overlapping same-type periods coalesce
// (pinned against sabre's output in testdata/freebusy-golden.ics).
//
// extraProps are additional pre-formatted iCalendar property lines (unfolded,
// no CRLF) injected into the VFREEBUSY component. The outbox POST free-busy
// handler passes UID/ORGANIZER/ATTENDEE so the response echoes the queried
// recipient, matching sabre; the free-busy-query REPORT passes none.
func GenerateFreeBusy(objects [][]byte, start, end time.Time, loc *time.Location, now time.Time, extraProps ...string) ([]byte, error) {
	return generateFreeBusy(objects, nil, start, end, loc, now, extraProps...)
}

// GenerateFreeBusyWithAvailability is GenerateFreeBusy plus the principal's
// working hours: time in [start, end) that the calendar-availability value
// marks unavailable is reported as FBTYPE=BUSY-UNAVAILABLE
// (docs/design/VAVAILABILITY-PLAN.md §4.2). Only the outbox POST passes availability;
// the free-busy-query REPORT and the auto-schedule conflict check keep using
// GenerateFreeBusy. An unusable availability value returns an error wrapping
// ErrInvalidAvailability, so the caller can fall back to GenerateFreeBusy.
func GenerateFreeBusyWithAvailability(objects [][]byte, availability []byte, start, end time.Time, loc *time.Location, now time.Time, extraProps ...string) ([]byte, error) {
	unavailable, err := unavailablePeriods(availability, start, end)
	if err != nil {
		return nil, err
	}
	return generateFreeBusy(objects, unavailable, start, end, loc, now, extraProps...)
}

// generateFreeBusy computes the response from the objects' busy time plus
// extra precomputed periods (already clamped to [start, end)). Periods of each
// FBTYPE coalesce separately, so an event outside working hours is still
// listed as BUSY alongside the BUSY-UNAVAILABLE span, like CalendarServer.
func generateFreeBusy(objects [][]byte, extra []busyPeriod, start, end time.Time, loc *time.Location, now time.Time, extraProps ...string) ([]byte, error) {
	collected, err := collectBusy(objects, start, end, loc)
	if err != nil {
		return nil, err
	}
	periods := coalesce(append(append([]busyPeriod(nil), extra...), collected...))

	const stamp = "20060102T150405Z"
	var b strings.Builder
	writeLine := func(s string) { b.WriteString(s + "\r\n") }
	writeLine("BEGIN:VCALENDAR")
	writeLine("VERSION:2.0")
	writeLine("PRODID:-//calendsd//calendsd//EN")
	writeLine("CALSCALE:GREGORIAN")
	writeLine("BEGIN:VFREEBUSY")
	writeLine("DTSTART:" + start.UTC().Format(stamp))
	writeLine("DTEND:" + end.UTC().Format(stamp))
	writeLine("DTSTAMP:" + now.UTC().Format(stamp))
	for _, line := range extraProps {
		writeLine(line)
	}
	for _, p := range periods {
		fbType := ""
		if p.fbType != "BUSY" {
			fbType = ";FBTYPE=" + p.fbType
		}
		writeLine(fmt.Sprintf("FREEBUSY%s:%s/%s",
			fbType, p.start.UTC().Format(stamp), p.end.UTC().Format(stamp)))
	}
	writeLine("END:VFREEBUSY")
	writeLine("END:VCALENDAR")
	return []byte(b.String()), nil
}

// collectBusy gathers the busy periods of every object within [start, end):
// VEVENT instances (expanded, bounded; see eventBusyPeriods) and stored
// VFREEBUSY periods. Unparseable objects contribute nothing, like sabre.
func collectBusy(objects [][]byte, start, end time.Time, loc *time.Location) ([]busyPeriod, error) {
	var periods []busyPeriod
	for _, data := range objects {
		cal, err := Decode(data)
		if err != nil {
			logSkippedObject("decode", err)
			continue
		}
		ResolveNonIANATZIDs(cal)
		for _, comp := range cal.Children {
			switch comp.Name {
			case goical.CompEvent:
				var overrides map[int64]*goical.Component
				if comp.Props.Get(goical.PropRecurrenceID) == nil {
					overrides = overridesOf(cal, comp, loc)
				}
				p, err := eventBusyPeriods(comp, start, end, loc, overrides)
				if err != nil {
					// One sabre-written object with an unparseable DTSTART or
					// DURATION must not fail the whole free-busy answer
					// (audit 6.14). The instance-cap fallback is handled
					// inside eventBusyPeriods and never reaches here.
					logSkippedObject("event", err)
					continue
				}
				periods = append(periods, p...)
			case "VFREEBUSY":
				// A stored VFREEBUSY object contributes its own FREEBUSY
				// periods, like vobject's FreeBusyGenerator::calculateBusy.
				p, err := freeBusyComponentPeriods(comp, start, end)
				if err != nil {
					logSkippedObject("freebusy", err)
					continue
				}
				periods = append(periods, p...)
			}
		}
	}
	return periods, nil
}

// logSkippedObject records an object free-busy could not use. Decode
// failures were silently skipped before; a skip is now visible either way so
// an operator can find the broken object instead of a user wondering why a
// slot reads free.
func logSkippedObject(stage string, err error) {
	slog.Default().Warn("free-busy: skipping unusable object", "stage", stage, "err", err)
}

// Period is a half-open busy interval [Start, End).
type Period struct{ Start, End time.Time }

// BusyPeriods returns the coalesced busy time of the objects within
// [start, end): the same intervals a VFREEBUSY for that window would list,
// every FBTYPE merged together. The auto-schedule conflict check uses it to
// compute a resource's busy time once per booking (audit 4.6).
func BusyPeriods(objects [][]byte, start, end time.Time, loc *time.Location) ([]Period, error) {
	collected, err := collectBusy(objects, start, end, loc)
	if err != nil {
		return nil, err
	}
	for i := range collected {
		collected[i].fbType = "BUSY"
	}
	var out []Period
	for _, p := range coalesce(collected) {
		out = append(out, Period{p.start, p.end})
	}
	return out, nil
}

// InstancePeriods returns the busy instances of every VEVENT in cal that
// fall within [start, end), uncoalesced, overrides counted at their own
// time. A series too dense to enumerate contributes one period covering the
// rest of the window (see eventBusyPeriods).
func InstancePeriods(cal *goical.Calendar, start, end time.Time, loc *time.Location) ([]Period, error) {
	ResolveNonIANATZIDs(cal)
	var out []Period
	for _, comp := range cal.Children {
		if comp.Name != goical.CompEvent {
			continue
		}
		var overrides map[int64]*goical.Component
		if comp.Props.Get(goical.PropRecurrenceID) == nil {
			overrides = overridesOf(cal, comp, loc)
		}
		ps, err := eventBusyPeriods(comp, start, end, loc, overrides)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			out = append(out, Period{p.start, p.end})
		}
	}
	return out, nil
}

// coalesce merges overlapping or touching periods of the same FBTYPE and
// returns the result in chronological order, like vobject's FreeBusyData
// timeline.
func coalesce(periods []busyPeriod) []busyPeriod {
	byType := map[string][]busyPeriod{}
	for _, p := range periods {
		byType[p.fbType] = append(byType[p.fbType], p)
	}
	var out []busyPeriod
	for _, group := range byType {
		slices.SortFunc(group, func(a, b busyPeriod) int { return a.start.Compare(b.start) })
		var merged []busyPeriod
		for _, p := range group {
			if n := len(merged); n > 0 && !p.start.After(merged[n-1].end) {
				if p.end.After(merged[n-1].end) {
					merged[n-1].end = p.end
				}
				continue
			}
			merged = append(merged, p)
		}
		out = append(out, merged...)
	}
	slices.SortFunc(out, func(a, b busyPeriod) int { return a.start.Compare(b.start) })
	return out
}

// freeBusyComponentPeriods extracts the busy intervals from a stored VFREEBUSY
// component's FREEBUSY properties, clamped to [start, end). FBTYPE=FREE periods
// are skipped, mirroring vobject's FreeBusyGenerator.
func freeBusyComponentPeriods(comp *goical.Component, start, end time.Time) ([]busyPeriod, error) {
	var out []busyPeriod
	for _, prop := range comp.Props["FREEBUSY"] {
		fbType := "BUSY"
		if t := prop.Params.Get("FBTYPE"); t != "" {
			fbType = strings.ToUpper(t)
		}
		if fbType == "FREE" {
			continue
		}
		for _, periodStr := range strings.Split(prop.Value, ",") {
			slash := strings.IndexByte(periodStr, '/')
			if slash < 0 {
				continue
			}
			s, err := parseFreeBusyTime(periodStr[:slash])
			if err != nil {
				return nil, err
			}
			e, err := parseFreeBusyPeriodEnd(s, periodStr[slash+1:])
			if err != nil {
				return nil, err
			}
			if !s.Before(end) || !e.After(start) {
				continue
			}
			if s.Before(start) {
				s = start
			}
			if e.After(end) {
				e = end
			}
			out = append(out, busyPeriod{start: s, end: e, fbType: fbType})
		}
	}
	return out, nil
}

// parseFreeBusyTime parses a FREEBUSY period datetime, which RFC 5545 requires
// to be a UTC ("Z") value.
func parseFreeBusyTime(s string) (time.Time, error) {
	return time.Parse("20060102T150405Z", s)
}

// parseFreeBusyPeriodEnd resolves the end of a FREEBUSY period: an explicit UTC
// datetime, or a duration (starting with P/-P/+P) added to the start.
func parseFreeBusyPeriodEnd(start time.Time, s string) (time.Time, error) {
	if strings.HasPrefix(s, "P") || strings.HasPrefix(s, "-P") || strings.HasPrefix(s, "+P") {
		dur, err := (&goical.Prop{Value: s}).Duration()
		if err != nil {
			return time.Time{}, err
		}
		return start.Add(dur), nil
	}
	return parseFreeBusyTime(s)
}

// eventBusyPeriods lists a VEVENT's busy time in [start, end). A master
// leaves out the occurrences in overridden: each override counts on its own,
// at its own time and with its own TRANSP and STATUS.
func eventBusyPeriods(comp *goical.Component, start, end time.Time, loc *time.Location, overridden map[int64]*goical.Component) ([]busyPeriod, error) {
	if transp := comp.Props.Get("TRANSP"); transp != nil && strings.EqualFold(transp.Value, "TRANSPARENT") {
		return nil, nil
	}
	fbType := "BUSY"
	if status := comp.Props.Get("STATUS"); status != nil {
		switch strings.ToUpper(status.Value) {
		case "CANCELLED":
			return nil, nil
		case "TENTATIVE":
			fbType = "BUSY-TENTATIVE"
		}
	}

	dtstartProp := comp.Props.Get(goical.PropDateTimeStart)
	if dtstartProp == nil {
		return nil, nil
	}
	dtstart, err := dtstartProp.DateTime(loc)
	if err != nil {
		return nil, err
	}
	endOf, err := occurrenceEnd(comp, dtstartProp, dtstart)
	if err != nil {
		return nil, err
	}

	clamp := func(s, e time.Time) *busyPeriod {
		if !s.Before(end) || !e.After(start) {
			return nil
		}
		if s.Before(start) {
			s = start
		}
		if e.After(end) {
			e = end
		}
		return &busyPeriod{start: s, end: e, fbType: fbType}
	}

	if _, err := comp.Props.RecurrenceRule(); err != nil {
		return nil, err
	}
	if !IsRecurring(comp) {
		if overridden[dtstart.Unix()] != nil {
			return nil, nil
		}
		if p := clamp(dtstart, endOf(dtstart)); p != nil {
			return []busyPeriod{*p}, nil
		}
		return nil, nil
	}

	set, err := recurrenceSet(comp, loc)
	if err != nil {
		return nil, err
	}
	var out []busyPeriod
	last, err := scanWindow(set.Iterator(), endOf, start, end, func(occ time.Time) bool {
		if overridden[occ.Unix()] != nil {
			return false
		}
		if p := clamp(occ, endOf(occ)); p != nil {
			out = append(out, *p)
		}
		return false
	})
	if errors.Is(err, ErrTooManyInstances) {
		// Too dense to enumerate: report the rest of the window as busy from
		// the last instance seen. Over-reporting busy time is the safe side
		// for scheduling; a silently truncated answer would invite double
		// bookings (audit 3.1).
		if p := clamp(last, end); p != nil {
			out = append(out, *p)
		}
		return out, nil
	}
	return out, err
}
