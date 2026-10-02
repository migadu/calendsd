package ical

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"
)

// VAVAILABILITY (RFC 7953) support: a user's working hours, stored as the
// calendar-availability property of their schedule inbox, turn the time
// outside those hours into BUSY-UNAVAILABLE free-busy periods
// (VAVAILABILITY-PLAN.md §3–§4).

const (
	compAvailability = "VAVAILABILITY"
	compAvailable    = "AVAILABLE"

	// fbBusyUnavailable is the FBTYPE every unavailable period is reported
	// with; other BUSY-TYPE values are accepted but not distinguished (v1).
	fbBusyUnavailable = "BUSY-UNAVAILABLE"

	// MaxAvailabilitySize bounds a stored calendar-availability value.
	MaxAvailabilitySize = 64 << 10
)

// ErrInvalidAvailability wraps every reason an availability value is
// rejected or cannot be evaluated. Callers computing free-busy treat it as
// "no availability" rather than as a server error.
var ErrInvalidAvailability = errors.New("invalid calendar-availability")

func invalidAvailability(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidAvailability, fmt.Sprintf(format, args...))
}

// ValidateAvailability checks a calendar-availability value before it is
// stored (VAVAILABILITY-PLAN.md §3.3). data must already have CRLF line
// endings (NormalizeCRLF). Every date-time must be anchored to a zone: a
// floating time cannot say whose 09:00 it means.
func ValidateAvailability(data []byte) error {
	if len(data) > MaxAvailabilitySize {
		return invalidAvailability("value exceeds %d bytes", MaxAvailabilitySize)
	}
	cal, err := Decode(data)
	if err != nil {
		return invalidAvailability("not an iCalendar object: %v", err)
	}
	zones := availabilityZones(cal)
	found := false
	for _, child := range cal.Children {
		switch child.Name {
		case goical.CompTimezone:
		case compAvailability:
			found = true
			if err := validateVAvailability(child, zones); err != nil {
				return err
			}
		default:
			return invalidAvailability("unexpected %s component", child.Name)
		}
	}
	if !found {
		return invalidAvailability("no VAVAILABILITY component")
	}
	// Expansion problems a static check misses (a malformed RRULE value, an
	// unparseable EXDATE) surface here rather than at free-busy time.
	now := time.Now()
	if _, err := unavailablePeriods(data, now, now.AddDate(0, 0, 7)); err != nil {
		return err
	}
	return nil
}

func validateVAvailability(comp *goical.Component, zones availabilityZoneSet) error {
	if err := requireIdentity(comp); err != nil {
		return err
	}
	if err := checkDateTimes(comp, zones); err != nil {
		return err
	}
	var start, end time.Time
	if p := comp.Props.Get(goical.PropDateTimeStart); p != nil {
		start, _ = zones.instant(p)
	}
	if p := comp.Props.Get(goical.PropDateTimeEnd); p != nil {
		end, _ = zones.instant(p)
	}
	if !start.IsZero() && !end.IsZero() && !end.After(start) {
		return invalidAvailability("VAVAILABILITY ends before it starts")
	}
	masters := map[string]bool{}
	for _, sub := range comp.Children {
		if sub.Name != compAvailable {
			return invalidAvailability("unexpected %s inside VAVAILABILITY", sub.Name)
		}
		if err := validateAvailable(sub, zones); err != nil {
			return err
		}
		if sub.Props.Get(goical.PropRecurrenceID) == nil {
			uid := sub.Props.Get(goical.PropUID).Value
			if masters[uid] {
				// Expansion keeps one master per UID; a second one would
				// silently be dropped.
				return invalidAvailability("two AVAILABLE components share UID %q", uid)
			}
			masters[uid] = true
		}
	}
	return nil
}

func validateAvailable(comp *goical.Component, zones availabilityZoneSet) error {
	if err := requireIdentity(comp); err != nil {
		return err
	}
	if err := checkDateTimes(comp, zones); err != nil {
		return err
	}
	dtstart := comp.Props.Get(goical.PropDateTimeStart)
	if dtstart == nil {
		return invalidAvailability("AVAILABLE without DTSTART")
	}
	if comp.Props.Get(goical.PropDateTimeEnd) == nil && comp.Props.Get(goical.PropDuration) == nil {
		return invalidAvailability("AVAILABLE without DTEND or DURATION")
	}
	start, end, err := zones.span(comp)
	if err != nil {
		return invalidAvailability("AVAILABLE: %v", err)
	}
	if !end.After(start) {
		return invalidAvailability("AVAILABLE ends before it starts")
	}
	roption, err := comp.Props.RecurrenceRule()
	if err != nil {
		return invalidAvailability("RRULE: %v", err)
	}
	if roption != nil {
		freq := strings.ToUpper(comp.Props.Get(goical.PropRecurrenceRule).Value)
		// Working hours repeat daily at most often. A sub-daily rule anchored
		// years back would also need more than maxTimeRangeScan steps to
		// reach today.
		if strings.Contains(freq, "FREQ=SECONDLY") || strings.Contains(freq, "FREQ=MINUTELY") ||
			strings.Contains(freq, "FREQ=HOURLY") {
			return invalidAvailability("RRULE frequency below DAILY is not supported")
		}
	}
	return nil
}

// requireIdentity enforces RFC 7953's required UID and DTSTAMP.
func requireIdentity(comp *goical.Component) error {
	if p := comp.Props.Get(goical.PropUID); p == nil || p.Value == "" {
		return invalidAvailability("%s without UID", comp.Name)
	}
	if comp.Props.Get(goical.PropDateTimeStamp) == nil {
		return invalidAvailability("%s without DTSTAMP", comp.Name)
	}
	return nil
}

// checkDateTimes rejects DATE values, floating date-times, PERIOD-valued
// RDATEs and TZIDs that resolve neither by name nor through an embedded
// VTIMEZONE.
func checkDateTimes(comp *goical.Component, zones availabilityZoneSet) error {
	for _, name := range []string{
		goical.PropDateTimeStart, goical.PropDateTimeEnd, goical.PropRecurrenceID,
		goical.PropExceptionDates, goical.PropRecurrenceDates,
	} {
		for _, p := range comp.Props[name] {
			switch p.ValueType() {
			case goical.ValueDate:
				return invalidAvailability("%s %s is a DATE; working hours need a time and zone", comp.Name, name)
			case goical.ValuePeriod:
				return invalidAvailability("%s %s: PERIOD values are not supported", comp.Name, name)
			}
			tzid := p.Params.Get(goical.PropTimezoneID)
			for _, v := range strings.Split(p.Value, ",") {
				if len(v) == len("20060102") {
					return invalidAvailability("%s %s is a DATE; working hours need a time and zone", comp.Name, name)
				}
				if !strings.HasSuffix(v, "Z") && tzid == "" {
					return invalidAvailability("%s %s %s is a floating time; give it a TZID", comp.Name, name, v)
				}
			}
			if tzid != "" && zones[tzid] == nil {
				if _, err := time.LoadLocation(tzid); err != nil {
					return invalidAvailability("unknown TZID %q", tzid)
				}
			}
		}
	}
	return nil
}

// UnavailablePeriods returns the parts of [start, end) that the availability
// marks as unavailable: for each VAVAILABILITY, its coverage (DTSTART/DTEND,
// either unbounded) minus the union of its AVAILABLE instances, unioned across
// components (v1 ignores PRIORITY). Every instance is computed in its own
// zone before conversion to UTC, which is what keeps a 09:00 day at 09:00
// local across DST (VAVAILABILITY-PLAN.md §4.1). Errors wrap
// ErrInvalidAvailability.
func unavailablePeriods(data []byte, start, end time.Time) ([]busyPeriod, error) {
	cal, err := Decode([]byte(NormalizeCRLF(string(data))))
	if err != nil {
		return nil, invalidAvailability("not an iCalendar object: %v", err)
	}
	zones := availabilityZones(cal)
	var out []busyPeriod
	for _, comp := range cal.Children {
		if comp.Name != compAvailability {
			continue
		}
		p, err := zones.componentUnavailable(comp, start, end)
		if err != nil {
			return nil, err
		}
		out = append(out, p...)
	}
	return coalesce(out), nil
}

// interval is a half-open [start, end) span of absolute time.
type interval struct{ start, end time.Time }

func (z availabilityZoneSet) componentUnavailable(comp *goical.Component, start, end time.Time) ([]busyPeriod, error) {
	covStart, covEnd := start, end
	if p := comp.Props.Get(goical.PropDateTimeStart); p != nil {
		t, err := z.instant(p)
		if err != nil {
			return nil, invalidAvailability("VAVAILABILITY DTSTART: %v", err)
		}
		if t.After(covStart) {
			covStart = t
		}
		if d := comp.Props.Get(goical.PropDuration); d != nil && comp.Props.Get(goical.PropDateTimeEnd) == nil {
			dur, err := d.Duration()
			if err != nil {
				return nil, invalidAvailability("VAVAILABILITY DURATION: %v", err)
			}
			if e := t.Add(dur); e.Before(covEnd) {
				covEnd = e
			}
		}
	}
	if p := comp.Props.Get(goical.PropDateTimeEnd); p != nil {
		t, err := z.instant(p)
		if err != nil {
			return nil, invalidAvailability("VAVAILABILITY DTEND: %v", err)
		}
		if t.Before(covEnd) {
			covEnd = t
		}
	}
	if !covStart.Before(covEnd) {
		return nil, nil
	}

	// Group AVAILABLE by UID: one master plus RECURRENCE-ID overrides.
	type group struct {
		master    *goical.Component
		overrides []*goical.Component
	}
	groups := map[string]*group{}
	var order []string
	for _, sub := range comp.Children {
		if sub.Name != compAvailable {
			continue
		}
		uid := ""
		if p := sub.Props.Get(goical.PropUID); p != nil {
			uid = p.Value
		}
		g := groups[uid]
		if g == nil {
			g = &group{}
			groups[uid] = g
			order = append(order, uid)
		}
		if sub.Props.Get(goical.PropRecurrenceID) != nil {
			g.overrides = append(g.overrides, sub)
		} else if g.master == nil {
			// Validation rejects a second master; one that got in anyway
			// (an imported value) is ignored, not allowed to replace the
			// first.
			g.master = sub
		}
	}

	var avail []interval
	for _, uid := range order {
		g := groups[uid]
		replaced := map[int64]bool{}
		for _, o := range g.overrides {
			rid, err := z.instant(o.Props.Get(goical.PropRecurrenceID))
			if err != nil {
				return nil, invalidAvailability("AVAILABLE RECURRENCE-ID: %v", err)
			}
			replaced[rid.Unix()] = true
			s, e, err := z.span(o)
			if err != nil {
				return nil, invalidAvailability("AVAILABLE: %v", err)
			}
			avail = append(avail, interval{s, e})
		}
		if g.master != nil {
			inst, err := z.expand(g.master, covStart, covEnd, replaced)
			if err != nil {
				return nil, err
			}
			avail = append(avail, inst...)
		}
	}
	var out []busyPeriod
	for _, iv := range subtract(interval{covStart, covEnd}, avail) {
		out = append(out, busyPeriod{start: iv.start, end: iv.end, fbType: fbBusyUnavailable})
	}
	return out, nil
}

// expand lists a master AVAILABLE's instances overlapping [start, end),
// skipping the ones an override replaces (keyed by unix instant).
func (z availabilityZoneSet) expand(comp *goical.Component, start, end time.Time, replaced map[int64]bool) ([]interval, error) {
	roption, err := comp.Props.RecurrenceRule()
	if err != nil {
		return nil, invalidAvailability("RRULE: %v", err)
	}
	if roption == nil {
		s, e, err := z.span(comp)
		if err != nil {
			return nil, invalidAvailability("AVAILABLE: %v", err)
		}
		if replaced[s.Unix()] {
			return nil, nil
		}
		return []interval{{s, e}}, nil
	}

	// Expand in wall-clock time. An IANA TZID gives rrule a real location,
	// which keeps every instance at its local time. An embedded (non-IANA)
	// VTIMEZONE has no time.Location, so the rule is expanded on the bare
	// wall clock (as UTC) and each instance is placed with the VTIMEZONE's
	// offset for that wall time; a UTC UNTIL is moved onto the same wall
	// clock so it ends the series at the right instance.
	dtstartProp := comp.Props.Get(goical.PropDateTimeStart)
	custom := z[dtstartProp.Params.Get(goical.PropTimezoneID)]
	ruleComp := comp
	if custom != nil {
		ruleComp = stripTZID(comp, dtstartProp.Params.Get(goical.PropTimezoneID))
		if rr := ruleComp.Props.Get(goical.PropRecurrenceRule); rr != nil {
			rr.Value = untilOnWallClock(rr.Value, custom)
		}
	}
	dtstart, err := ruleComp.Props.Get(goical.PropDateTimeStart).DateTime(time.UTC)
	if err != nil {
		return nil, invalidAvailability("AVAILABLE DTSTART: %v", err)
	}
	dur, err := masterDurationIn(ruleComp, ruleComp.Props.Get(goical.PropDateTimeStart), dtstart)
	if err != nil {
		return nil, invalidAvailability("AVAILABLE: %v", err)
	}
	set, err := recurrenceSet(ruleComp, time.UTC)
	if err != nil {
		return nil, invalidAvailability("AVAILABLE recurrence: %v", err)
	}
	place := func(t time.Time) time.Time {
		if custom == nil {
			return t
		}
		return t.Add(-time.Duration(custom.offsetAt(t)) * time.Second)
	}

	var out []interval
	next := set.Iterator()
	// A weekly rule anchored years ago is normal for working hours, so the
	// cap is the window-bounded maxTimeRangeScan, not maxRecurrences, which
	// would stop a long-running rule before it reaches the window.
	for i := 0; ; i++ {
		if i == maxTimeRangeScan {
			// Stopping here would report the rest of the window as
			// unavailable; an unusable value is the honest answer.
			return nil, invalidAvailability("AVAILABLE recurrence does not reach the window within %d instances", maxTimeRangeScan)
		}
		occ, more := next()
		if !more {
			break
		}
		s, e := place(occ), place(occ.Add(dur))
		if !s.Before(end) {
			break
		}
		if !e.After(start) || replaced[s.Unix()] {
			continue
		}
		out = append(out, interval{s, e})
	}
	return out, nil
}

// subtract returns cover minus the union of holes, in order.
func subtract(cover interval, holes []interval) []interval {
	sort.Slice(holes, func(i, j int) bool { return holes[i].start.Before(holes[j].start) })
	var out []interval
	cur := cover.start
	for _, h := range holes {
		if !h.end.After(cur) {
			continue
		}
		if !h.start.Before(cover.end) {
			break
		}
		if h.start.After(cur) {
			out = append(out, interval{cur, h.start})
		}
		cur = h.end
		if !cur.Before(cover.end) {
			return out
		}
	}
	if cur.Before(cover.end) {
		out = append(out, interval{cur, cover.end})
	}
	return out
}

// availabilityZoneSet maps the TZIDs of embedded VTIMEZONEs that
// time.LoadLocation cannot resolve (Outlook-style names) to their parsed
// transitions. Unlike resolveNonIANATZIDs, which flattens such times to fixed
// UTC values, availability keeps them as wall-clock times so a recurring rule
// follows the zone's DST changes.
type availabilityZoneSet map[string]*vtimezone

func availabilityZones(cal *goical.Calendar) availabilityZoneSet {
	zones := availabilityZoneSet{}
	for _, child := range cal.Children {
		if child.Name != goical.CompTimezone {
			continue
		}
		p := child.Props.Get(goical.PropTimezoneID)
		if p == nil || p.Value == "" {
			continue
		}
		if _, err := time.LoadLocation(p.Value); err == nil {
			continue
		}
		if tz := parseVTimezone(child); tz != nil {
			zones[p.Value] = tz
		}
	}
	return zones
}

// instant resolves a single date-time property to an absolute time.
func (z availabilityZoneSet) instant(p *goical.Prop) (time.Time, error) {
	if tz := z[p.Params.Get(goical.PropTimezoneID)]; tz != nil {
		wall, err := time.ParseInLocation("20060102T150405", p.Value, time.UTC)
		if err != nil {
			return time.Time{}, err
		}
		return wall.Add(-time.Duration(tz.offsetAt(wall)) * time.Second), nil
	}
	return p.DateTime(time.UTC)
}

// span resolves a single (non-recurring) component's [DTSTART, end).
func (z availabilityZoneSet) span(comp *goical.Component) (time.Time, time.Time, error) {
	sp := comp.Props.Get(goical.PropDateTimeStart)
	if sp == nil {
		return time.Time{}, time.Time{}, errors.New("missing DTSTART")
	}
	s, err := z.instant(sp)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if ep := comp.Props.Get(goical.PropDateTimeEnd); ep != nil {
		e, err := z.instant(ep)
		return s, e, err
	}
	if dp := comp.Props.Get(goical.PropDuration); dp != nil {
		dur, err := dp.Duration()
		return s, s.Add(dur), err
	}
	return time.Time{}, time.Time{}, errors.New("missing DTEND or DURATION")
}

// untilOnWallClock rewrites an RRULE's UTC UNTIL to the zone's wall-clock
// time (written as UTC, the clock the rule is expanded on). Other values
// are returned unchanged.
func untilOnWallClock(rule string, tz *vtimezone) string {
	parts := strings.Split(rule, ";")
	for i, part := range parts {
		name, value, ok := strings.Cut(part, "=")
		if !ok || !strings.EqualFold(name, "UNTIL") || !strings.HasSuffix(value, "Z") {
			continue
		}
		until, err := time.Parse("20060102T150405Z", value)
		if err != nil {
			continue
		}
		wall := until.Add(time.Duration(tz.offsetAtInstant(until)) * time.Second)
		parts[i] = name + "=" + wall.Format("20060102T150405Z")
	}
	return strings.Join(parts, ";")
}

// stripTZID returns a shallow copy of comp whose date-time properties in zone
// tzid become floating, so they can be expanded on the bare wall clock.
func stripTZID(comp *goical.Component, tzid string) *goical.Component {
	cp := &goical.Component{Name: comp.Name, Props: goical.Props{}, Children: comp.Children}
	for name, props := range comp.Props {
		out := make([]goical.Prop, len(props))
		for i, p := range props {
			out[i] = cloneProp(p)
			if out[i].Params.Get(goical.PropTimezoneID) == tzid {
				out[i].Params.Del(goical.PropTimezoneID)
			}
		}
		cp.Props[name] = out
	}
	return cp
}
