package ical

import (
	"fmt"
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
// (VAVAILABILITY-PLAN.md §4.2). Only the outbox POST passes availability;
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
	periods := append([]busyPeriod(nil), extra...)
	for _, data := range objects {
		cal, err := Decode(data)
		if err != nil {
			continue // unparseable objects contribute nothing, like sabre
		}
		resolveNonIANATZIDs(cal)
		for _, comp := range cal.Children {
			switch comp.Name {
			case goical.CompEvent:
				var overrides map[int64]*goical.Component
				if comp.Props.Get(goical.PropRecurrenceID) == nil {
					overrides = overridesOf(cal, comp, loc)
				}
				p, err := eventBusyPeriods(comp, start, end, loc, overrides)
				if err != nil {
					return nil, err
				}
				periods = append(periods, p...)
			case "VFREEBUSY":
				// A stored VFREEBUSY object contributes its own FREEBUSY
				// periods, like vobject's FreeBusyGenerator::calculateBusy.
				p, err := freeBusyComponentPeriods(comp, start, end)
				if err != nil {
					return nil, err
				}
				periods = append(periods, p...)
			}
		}
	}
	periods = coalesce(periods)

	const stamp = "20060102T150405Z"
	var b strings.Builder
	writeLine := func(s string) { b.WriteString(s + "\r\n") }
	writeLine("BEGIN:VCALENDAR")
	writeLine("VERSION:2.0")
	writeLine("PRODID:-//calserver//calserver//EN")
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
	next := set.Iterator()
	// Bounded by the finite window end (see EventInTimeRange); maxTimeRangeScan
	// is only a pathological-rule backstop, not an occurrence cap that would
	// truncate a long-running series before its instances in the window.
	for i := 0; i < maxTimeRangeScan; i++ {
		occStart, more := next()
		if !more || !occStart.Before(end) {
			break
		}
		if overridden[occStart.Unix()] != nil {
			continue
		}
		if p := clamp(occStart, endOf(occStart)); p != nil {
			out = append(out, *p)
		}
	}
	return out, nil
}
