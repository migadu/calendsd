package ical

import (
	"fmt"
	"sort"
	"time"

	goical "github.com/emersion/go-ical"
)

// Expand rewrites a calendar so every VEVENT occurrence within [start, end)
// becomes a standalone instance: recurrence rules stripped, overridden
// instances merged in, all touched date-times converted to UTC, VTIMEZONEs
// dropped. Port of vobject's VCalendar::expand, which backs calendar-data
// <C:expand> (RFC 4791 §9.6.5). loc resolves floating times (the calendar's
// timezone).
func Expand(cal *goical.Calendar, start, end time.Time, loc *time.Location) (*goical.Calendar, error) {
	// Resolve Outlook-style non-IANA TZIDs from any embedded VTIMEZONE before
	// expanding, since go-ical cannot load them by name.
	resolveNonIANATZIDs(cal)
	out := goical.NewCalendar()
	out.Props.SetText(goical.PropVersion, "2.0")
	out.Props.SetText(goical.PropProductID, "-//calserver//calserver//EN")
	out.Props.SetText("CALSCALE", "GREGORIAN")
	// Other calendar-level properties (X-WR-CALNAME, ...) survive
	// expansion, like vobject.
	for name, props := range cal.Props {
		switch name {
		case goical.PropVersion, goical.PropProductID, "CALSCALE":
			continue
		}
		for _, p := range props {
			cp := p
			out.Props.Add(&cp)
		}
	}

	// Group VEVENTs by UID: master + overridden instances (RECURRENCE-ID).
	type eventGroup struct {
		master    *goical.Component
		overrides map[int64]*goical.Component // keyed by recurrence-id unix
	}
	groups := map[string]*eventGroup{}
	var uidOrder []string
	var others []*goical.Component

	for _, child := range cal.Children {
		switch child.Name {
		case goical.CompTimezone:
			continue // expanded output is pure UTC
		case goical.CompEvent:
			uid := ""
			if p := child.Props.Get(goical.PropUID); p != nil {
				uid = p.Value
			}
			g, exists := groups[uid]
			if !exists {
				g = &eventGroup{overrides: map[int64]*goical.Component{}}
				groups[uid] = g
				uidOrder = append(uidOrder, uid)
			}
			if rid := child.Props.Get(goical.PropRecurrenceID); rid != nil {
				t, err := rid.DateTime(loc)
				if err != nil {
					return nil, fmt.Errorf("parsing RECURRENCE-ID: %w", err)
				}
				g.overrides[t.Unix()] = child
			} else {
				g.master = child
			}
		default:
			others = append(others, child)
		}
	}

	for _, uid := range uidOrder {
		g := groups[uid]
		instances, err := expandGroup(g.master, g.overrides, start, end, loc)
		if err != nil {
			return nil, fmt.Errorf("expanding uid %q: %w", uid, err)
		}
		for _, inst := range instances {
			out.Children = append(out.Children, inst)
		}
	}
	out.Children = append(out.Children, others...)
	return out, nil
}

func expandGroup(master *goical.Component, overrides map[int64]*goical.Component, start, end time.Time, loc *time.Location) ([]*goical.Component, error) {
	if master == nil {
		// Only overrides: keep the ones in range, converted to UTC.
		var out []*goical.Component
		var keys []int64
		for k := range overrides {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, k := range keys {
			inst, ok, err := utcInstance(overrides[k], start, end, loc, true)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, inst)
			}
		}
		return out, nil
	}

	if _, err := master.Props.RecurrenceRule(); err != nil {
		return nil, err
	}
	if !IsRecurring(master) {
		inst, ok, err := utcInstance(master, start, end, loc, false)
		if err != nil || !ok {
			return nil, err
		}
		return []*goical.Component{inst}, nil
	}

	dtstartProp := master.Props.Get(goical.PropDateTimeStart)
	if dtstartProp == nil {
		// A recurring event without DTSTART has no expandable
		// occurrences; drop it rather than panic.
		return nil, nil
	}
	dtstart, err := dtstartProp.DateTime(loc)
	if err != nil {
		return nil, err
	}
	endOf, err := occurrenceEnd(master, dtstartProp, dtstart)
	if err != nil {
		return nil, err
	}
	set, err := recurrenceSet(master, loc)
	if err != nil {
		return nil, err
	}

	var out []*goical.Component
	// Track override slots the iterator visits, so overrides whose original
	// recurrence slot falls outside the window (moved elsewhere) can still be
	// emitted afterward when their new DTSTART lands in [start, end).
	consumed := make(map[int64]bool, len(overrides))
	next := set.Iterator()
	// maxRecurrences caps the instances returned, not the ones stepped over
	// to reach the window, which would leave a long-running series (a daily
	// event from 2010) with no instances at all; the walk itself is bounded
	// by end, with maxTimeRangeScan as the backstop (see EventInTimeRange).
	for i := 0; i < maxTimeRangeScan && len(out) < maxRecurrences; i++ {
		occStart, more := next()
		if !more || !occStart.Before(end) {
			break
		}
		occEnd := endOf(occStart)
		if !occEnd.After(start) {
			continue
		}
		if override, exists := overrides[occStart.Unix()]; exists {
			consumed[occStart.Unix()] = true
			inst, ok, err := utcInstance(override, start, end, loc, true)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, inst)
			}
			continue
		}
		inst := cloneComponent(master)
		inst.Props.Del(goical.PropRecurrenceRule)
		inst.Props.Del(goical.PropRecurrenceDates)
		inst.Props.Del(goical.PropExceptionDates)
		// All-day (VALUE=DATE) starts stay DATE-valued rather than being
		// shifted to a timezone-adjusted UTC date-time, matching the override
		// path (utcInstance).
		if dtstartProp.ValueType() == goical.ValueDate {
			setDateValue(inst, goical.PropDateTimeStart, occStart)
			if master.Props.Get(goical.PropDateTimeEnd) != nil {
				setDateValue(inst, goical.PropDateTimeEnd, occEnd)
			}
			setDateValue(inst, goical.PropRecurrenceID, occStart)
		} else {
			setUTCDateTime(inst, goical.PropDateTimeStart, occStart)
			if master.Props.Get(goical.PropDateTimeEnd) != nil {
				setUTCDateTime(inst, goical.PropDateTimeEnd, occEnd)
			}
			setUTCDateTime(inst, goical.PropRecurrenceID, occStart)
		}
		out = append(out, inst)
	}
	// Emit overrides whose recurrence slot lay outside the iterated window but
	// whose (moved) DTSTART falls inside [start, end).
	var movedKeys []int64
	for k := range overrides {
		if !consumed[k] {
			movedKeys = append(movedKeys, k)
		}
	}
	sort.Slice(movedKeys, func(i, j int) bool { return movedKeys[i] < movedKeys[j] })
	for _, k := range movedKeys {
		inst, ok, err := utcInstance(overrides[k], start, end, loc, true)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, inst)
		}
	}
	return out, nil
}

// utcInstance clones a single event instance with DTSTART/DTEND (and
// RECURRENCE-ID, for overrides) converted to UTC, when it overlaps
// [start, end).
func utcInstance(comp *goical.Component, start, end time.Time, loc *time.Location, isOverride bool) (*goical.Component, bool, error) {
	in, err := EventInTimeRange(comp, start, end, loc)
	if err != nil {
		return nil, false, err
	}
	if !in {
		return nil, false, nil
	}
	inst := cloneComponent(comp)
	for _, name := range []string{goical.PropDateTimeStart, goical.PropDateTimeEnd, goical.PropRecurrenceID} {
		prop := inst.Props.Get(name)
		if prop == nil || prop.ValueType() == goical.ValueDate {
			continue
		}
		t, err := prop.DateTime(loc)
		if err != nil {
			return nil, false, err
		}
		setUTCDateTime(inst, name, t)
	}
	return inst, true, nil
}

func setUTCDateTime(comp *goical.Component, name string, t time.Time) {
	props := comp.Props.Values(name)
	prop := goical.NewProp(name)
	if len(props) > 0 {
		// Preserve position/other params by editing the existing prop.
		prop = &props[0]
	}
	prop.Params.Del(goical.PropTimezoneID)
	prop.Params.Del("VALUE")
	prop.Value = t.UTC().Format("20060102T150405Z")
	if len(props) > 0 {
		props[0] = *prop
		comp.Props[name] = props
	} else {
		comp.Props.Set(prop)
	}
}

// setDateValue writes a DATE-valued property (all-day), keeping VALUE=DATE and
// the calendar date in loc, dropping any TZID. t is the occurrence start in the
// calendar's location, whose date is the wanted calendar day.
func setDateValue(comp *goical.Component, name string, t time.Time) {
	props := comp.Props.Values(name)
	prop := goical.NewProp(name)
	if len(props) > 0 {
		prop = &props[0]
	}
	prop.Params.Del(goical.PropTimezoneID)
	prop.Params.Set("VALUE", "DATE")
	prop.Value = t.Format("20060102")
	if len(props) > 0 {
		props[0] = *prop
		comp.Props[name] = props
	} else {
		comp.Props.Set(prop)
	}
}

// cloneComponent deep-copies a component.
func cloneComponent(src *goical.Component) *goical.Component {
	dst := goical.NewComponent(src.Name)
	for name, props := range src.Props {
		copied := make([]goical.Prop, len(props))
		for i, p := range props {
			cp := goical.Prop{Name: p.Name, Value: p.Value, Params: goical.Params{}}
			for k, vs := range p.Params {
				cp.Params[k] = append([]string(nil), vs...)
			}
			copied[i] = cp
		}
		dst.Props[name] = copied
	}
	for _, child := range src.Children {
		dst.Children = append(dst.Children, cloneComponent(child))
	}
	return dst
}
