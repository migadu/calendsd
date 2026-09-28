package ical

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"
)

// resolveNonIANATZIDs rewrites, in place, every date-time property in cal whose
// TZID is not resolvable by time.LoadLocation but is defined by an embedded
// VTIMEZONE, converting the value to a UTC (Z) date-time using the offsets in
// that VTIMEZONE. go-ical's Prop.DateTime resolves TZIDs only through
// time.LoadLocation, so Outlook-style non-IANA zone names (e.g. "W. Europe
// Standard Time") otherwise hard-error even when the object carries their
// VTIMEZONE. Rewriting to UTC lets the rest of the package treat them normally.
//
// TZIDs that time.LoadLocation already understands, DATE-only values, and
// already-UTC values are left untouched. TZIDs with no embedded VTIMEZONE are
// left untouched too (there is nothing to resolve from).
func resolveNonIANATZIDs(cal *goical.Calendar) {
	zones := map[string]*vtimezone{}
	for _, child := range cal.Children {
		if child.Name != goical.CompTimezone {
			continue
		}
		id := ""
		if p := child.Props.Get(goical.PropTimezoneID); p != nil {
			id = p.Value
		}
		if id == "" {
			continue
		}
		if _, err := time.LoadLocation(id); err == nil {
			continue // resolvable by go-ical already
		}
		if z := parseVTimezone(child); z != nil {
			zones[id] = z
		}
	}
	if len(zones) == 0 {
		return
	}
	dateProps := []string{
		goical.PropDateTimeStart, goical.PropDateTimeEnd,
		goical.PropRecurrenceID, goical.PropExceptionDates,
		goical.PropRecurrenceDates,
	}
	for _, comp := range cal.Children {
		if comp.Name == goical.CompTimezone {
			continue
		}
		for _, name := range dateProps {
			props := comp.Props[name]
			for i := range props {
				rewriteTZIDProp(&props[i], zones)
			}
			if props != nil {
				comp.Props[name] = props
			}
		}
	}
}

// rewriteTZIDProp converts a single date-time property from a non-IANA TZID to
// a UTC value, when its zone is one of the resolved VTIMEZONEs.
func rewriteTZIDProp(prop *goical.Prop, zones map[string]*vtimezone) {
	tzid := prop.Params.Get(goical.PropTimezoneID)
	if tzid == "" {
		return
	}
	z := zones[tzid]
	if z == nil {
		return
	}
	var converted []string
	for _, v := range strings.Split(prop.Value, ",") {
		wall, err := time.ParseInLocation("20060102T150405", v, time.UTC)
		if err != nil {
			return // not a floating date-time we can convert; leave as-is
		}
		off := z.offsetAt(wall)
		utc := wall.Add(-time.Duration(off) * time.Second)
		converted = append(converted, utc.Format("20060102T150405Z"))
	}
	prop.Params.Del(goical.PropTimezoneID)
	prop.Params.Del("VALUE")
	prop.Value = strings.Join(converted, ",")
}

// vtimezone holds a parsed VTIMEZONE as a set of offset transitions.
type vtimezone struct {
	transitions []tzTransition // sorted by ascending UTC instant
	// firstOffset applies before the earliest transition.
	firstOffset int
}

// tzTransition is the UTC instant at which offsetTo takes effect.
type tzTransition struct {
	at     time.Time
	offset int // seconds east of UTC
}

// offsetAt returns the offset (seconds east of UTC) in effect for a wall-clock
// time. The wall time is interpreted against the offset in effect, which is
// resolved from the transition table using the "from" offset of each rule (the
// transition instant is expressed in the pre-transition wall clock).
func (z *vtimezone) offsetAt(wall time.Time) int {
	off := z.firstOffset
	for _, tr := range z.transitions {
		// A transition at UTC instant tr.at happens at local wall clock
		// tr.at+prevOffset. Compare the target wall time against that.
		localAt := tr.at.Add(time.Duration(off) * time.Second)
		if !wall.Before(localAt) {
			off = tr.offset
		} else {
			break
		}
	}
	return off
}

// offsetAtInstant returns the offset in effect at a UTC instant.
func (z *vtimezone) offsetAtInstant(t time.Time) int {
	off := z.firstOffset
	for _, tr := range z.transitions {
		if t.Before(tr.at) {
			break
		}
		off = tr.offset
	}
	return off
}

// parseVTimezone builds a transition table from a VTIMEZONE component by
// expanding each STANDARD/DAYLIGHT sub-component's onset rule over a wide year
// range. Returns nil if the component carries no usable offsets.
func parseVTimezone(comp *goical.Component) *vtimezone {
	var raw []tzTransition
	firstOffset := 0
	haveFirst := false

	for _, sub := range comp.Children {
		if sub.Name != goical.CompTimezoneStandard && sub.Name != goical.CompTimezoneDaylight {
			continue
		}
		fromProp := sub.Props.Get(goical.PropTimezoneOffsetFrom)
		toProp := sub.Props.Get(goical.PropTimezoneOffsetTo)
		dsProp := sub.Props.Get(goical.PropDateTimeStart)
		if fromProp == nil || toProp == nil || dsProp == nil {
			continue
		}
		offFrom, err := parseUTCOffset(fromProp.Value)
		if err != nil {
			continue
		}
		offTo, err := parseUTCOffset(toProp.Value)
		if err != nil {
			continue
		}
		onset, err := time.ParseInLocation("20060102T150405", dsProp.Value, time.UTC)
		if err != nil {
			continue
		}
		if !haveFirst {
			firstOffset = offFrom
			haveFirst = true
		}

		// The onset DTSTART is in wall-clock time using offsetFrom; the UTC
		// instant of the transition is onset - offsetFrom.
		toUTC := func(local time.Time) time.Time {
			return local.Add(-time.Duration(offFrom) * time.Second)
		}

		// RDATE lists further onsets of the same observance (local time,
		// like DTSTART).
		for _, rd := range sub.Props[goical.PropRecurrenceDates] {
			for _, v := range strings.Split(rd.Value, ",") {
				if local, err := time.ParseInLocation("20060102T150405", v, time.UTC); err == nil {
					raw = append(raw, tzTransition{at: toUTC(local), offset: offTo})
				}
			}
		}
		roption, _ := sub.Props.RecurrenceRule()
		if roption == nil {
			raw = append(raw, tzTransition{at: toUTC(onset), offset: offTo})
			continue
		}
		// Expand the yearly onset rule across a wide window.
		set, err := recurrenceSet(sub, time.UTC)
		if err != nil || set == nil {
			raw = append(raw, tzTransition{at: toUTC(onset), offset: offTo})
			continue
		}
		next := set.Iterator()
		limit := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i < maxRecurrences; i++ {
			local, ok := next()
			if !ok || local.After(limit) {
				break
			}
			raw = append(raw, tzTransition{at: toUTC(local), offset: offTo})
		}
	}
	if !haveFirst {
		return nil
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].at.Before(raw[j].at) })
	return &vtimezone{transitions: raw, firstOffset: firstOffset}
}

// parseUTCOffset parses an iCalendar UTC offset ("+0200", "-0530", "+020000").
func parseUTCOffset(s string) (int, error) {
	s = strings.TrimSpace(s)
	if len(s) < 5 {
		return 0, fmt.Errorf("bad UTC offset %q", s)
	}
	sign := 1
	switch s[0] {
	case '+':
	case '-':
		sign = -1
	default:
		return 0, fmt.Errorf("bad UTC offset sign %q", s)
	}
	h, err := strconv.Atoi(s[1:3])
	if err != nil {
		return 0, err
	}
	m, err := strconv.Atoi(s[3:5])
	if err != nil {
		return 0, err
	}
	sec := 0
	if len(s) >= 7 {
		if sec, err = strconv.Atoi(s[5:7]); err != nil {
			return 0, err
		}
	}
	return sign * (h*3600 + m*60 + sec), nil
}
