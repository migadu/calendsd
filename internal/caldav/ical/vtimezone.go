package ical

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	goical "github.com/emersion/go-ical"
)

// resolveNonIANATZIDs rewrites, in place, every date-time property in cal whose
// TZID time.LoadLocation cannot resolve. go-ical's Prop.DateTime resolves TZIDs
// only through time.LoadLocation, so Outlook-style zone names (e.g. "W. Europe
// Standard Time") otherwise hard-error.
//
// A TZID that names an IANA zone, by the Windows zone table or by an embedded
// VTIMEZONE whose rules match one, is renamed to it. The value keeps its wall
// clock, so a recurring event keeps its local time across DST changes. Any
// other TZID with an embedded VTIMEZONE has its values converted to UTC with
// that VTIMEZONE's offsets: right for each single instant, but a rule then
// repeats in UTC.
//
// TZIDs that time.LoadLocation already understands, DATE-only values, and
// already-UTC values are left untouched, as are unknown TZIDs with no embedded
// VTIMEZONE (there is nothing to resolve from).
func resolveNonIANATZIDs(cal *goical.Calendar) {
	embedded := map[string]*goical.Component{}
	for _, child := range cal.Children {
		if child.Name != goical.CompTimezone {
			continue
		}
		if p := child.Props.Get(goical.PropTimezoneID); p != nil && p.Value != "" {
			embedded[p.Value] = child
		}
	}
	type resolution struct {
		iana string     // rename to this zone
		vtz  *vtimezone // else flatten with this one (nil: leave as is)
	}
	resolved := map[string]resolution{}
	resolve := func(tzid string) resolution {
		if r, ok := resolved[tzid]; ok {
			return r
		}
		var r resolution
		if _, err := time.LoadLocation(tzid); err != nil {
			comp := embedded[tzid]
			var vtz *vtimezone
			if comp != nil {
				vtz = parseVTimezone(comp)
			}
			if r.iana = ianaZoneFor(tzid, vtz); r.iana == "" {
				r.vtz = vtz
			}
		}
		resolved[tzid] = r
		return r
	}
	dateProps := []string{
		goical.PropDateTimeStart, goical.PropDateTimeEnd, "DUE",
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
				tzid := props[i].Params.Get(goical.PropTimezoneID)
				if tzid == "" {
					continue
				}
				switch r := resolve(tzid); {
				case r.iana != "":
					props[i].Params.Set(goical.PropTimezoneID, r.iana)
				case r.vtz != nil:
					flattenTZIDProp(&props[i], r.vtz)
				}
			}
		}
	}
}

// ianaZoneFor names the IANA zone a TZID that time.LoadLocation cannot load
// stands for: the Windows zone table's entry, else the zone whose transitions
// match those of its embedded VTIMEZONE (vtz, nil when there is none). It
// returns "" when neither applies.
func ianaZoneFor(tzid string, vtz *vtimezone) string {
	if name, ok := windowsZones[tzid]; ok {
		if _, err := time.LoadLocation(name); err == nil {
			return name
		}
	}
	if vtz == nil {
		return ""
	}
	return matchIANAZone(vtz)
}

// matchWindow is the span over which an embedded VTIMEZONE must agree with an
// IANA zone to be renamed to it. Outlook's definitions carry only the rule
// current when they were written, so older history cannot be compared.
var matchWindow = [2]time.Time{
	time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC),
	time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC),
}

// candidateZones are the IANA zones an embedded VTIMEZONE is compared with:
// one per Windows zone, which covers every rule set in current use.
var candidateZones = sync.OnceValue(func() []*time.Location {
	names := map[string]bool{}
	for _, name := range windowsZones {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	slices.Sort(sorted)
	var out []*time.Location
	for _, name := range sorted {
		if loc, err := time.LoadLocation(name); err == nil {
			out = append(out, loc)
		}
	}
	return out
})

// matchIANAZone returns the first candidate zone with the same offset as vtz
// at the start of matchWindow and the same offset changes within it, or "".
// A VTIMEZONE without changes in the window is not matched: flattening a
// fixed offset to UTC is already exact, and a rename could pick up some
// zone's DST outside the window.
func matchIANAZone(vtz *vtimezone) string {
	from, to := matchWindow[0], matchWindow[1]
	type change struct {
		at  int64
		off int
	}
	var want []change
	off := vtz.offsetAtInstant(from)
	for _, tr := range vtz.transitions {
		if !tr.at.After(from) || !tr.at.Before(to) || tr.offset == off {
			continue
		}
		off = tr.offset
		want = append(want, change{tr.at.Unix(), off})
	}
	if len(want) == 0 {
		return ""
	}
	start := vtz.offsetAtInstant(from)
next:
	for _, loc := range candidateZones() {
		if _, o := from.In(loc).Zone(); o != start {
			continue
		}
		var got []change
		for _, tr := range zoneTransitions(loc, from, to) {
			if tr.from != tr.to {
				got = append(got, change{tr.at.Unix(), tr.to})
			}
		}
		if len(got) != len(want) {
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				continue next
			}
		}
		return loc.String()
	}
	return ""
}

// flattenTZIDProp converts a single date-time property from its TZID to a UTC
// value, using the offsets of the zone's VTIMEZONE.
func flattenTZIDProp(prop *goical.Prop, z *vtimezone) {
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
		// Expand the yearly onset rule across a wide window. rrule-go stops a
		// series about 292 years (the range of a time.Duration) after its
		// DTSTART, and Outlook anchors its rules in 1601, which would end the
		// transitions in 1893 and leave every later date on the last offset
		// seen. An anchor before 1970 is moved to 1970: the rule, not the
		// anchor, decides each year's onset, and the extra instance the moved
		// DTSTART itself adds lies decades in the past.
		ruleSub := sub
		if onset.Year() < 1970 {
			ruleSub = &goical.Component{Name: sub.Name, Props: goical.Props{}, Children: sub.Children}
			for name, props := range sub.Props {
				ruleSub.Props[name] = props
			}
			moved := cloneProp(*dsProp)
			moved.Value = onset.AddDate(1970-onset.Year(), 0, 0).Format("20060102T150405")
			ruleSub.Props[goical.PropDateTimeStart] = []goical.Prop{moved}
		}
		set, err := recurrenceSet(ruleSub, time.UTC)
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
	slices.SortFunc(raw, func(a, b tzTransition) int { return a.at.Compare(b.at) })
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
