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
	out := goical.NewCalendar()
	out.Props.SetText(goical.PropVersion, "2.0")
	out.Props.SetText(goical.PropProductID, "-//calserver//calserver//EN")
	out.Props.SetText("CALSCALE", "GREGORIAN")

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

	roption, err := master.Props.RecurrenceRule()
	if err != nil {
		return nil, err
	}
	if roption == nil {
		inst, ok, err := utcInstance(master, start, end, loc, false)
		if err != nil || !ok {
			return nil, err
		}
		return []*goical.Component{inst}, nil
	}

	dtstartProp := master.Props.Get(goical.PropDateTimeStart)
	dtstart, err := dtstartProp.DateTime(loc)
	if err != nil {
		return nil, err
	}
	dur, err := masterDurationIn(master, dtstartProp, dtstart)
	if err != nil {
		return nil, err
	}
	set, err := master.RecurrenceSet(loc)
	if err != nil {
		return nil, err
	}

	var out []*goical.Component
	next := set.Iterator()
	for {
		occStart, more := next()
		if !more || !occStart.Before(end) {
			break
		}
		occEnd := occStart.Add(dur)
		if !occEnd.After(start) {
			continue
		}
		if override, exists := overrides[occStart.Unix()]; exists {
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
		setUTCDateTime(inst, goical.PropDateTimeStart, occStart)
		if master.Props.Get(goical.PropDateTimeEnd) != nil {
			setUTCDateTime(inst, goical.PropDateTimeEnd, occEnd)
		}
		setUTCDateTime(inst, goical.PropRecurrenceID, occStart)
		out = append(out, inst)
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
