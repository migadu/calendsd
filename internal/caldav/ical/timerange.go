package ical

import (
	"errors"
	"fmt"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"
)

// EventInTimeRange reports whether a VEVENT overlaps [start, end).
// Port of vobject VEvent::isInTimeRange: recurring events match if any
// occurrence overlaps; DTEND is non-inclusive. loc resolves floating times.
func EventInTimeRange(comp *goical.Component, start, end time.Time, loc *time.Location) (bool, error) {
	dtstartProp := comp.Props.Get(goical.PropDateTimeStart)
	if dtstartProp == nil {
		return false, errors.New("VEVENT is missing DTSTART")
	}
	roption, err := comp.Props.RecurrenceRule()
	if err != nil {
		return false, fmt.Errorf("parsing RRULE: %w", err)
	}

	if roption != nil {
		dtstart, err := dtstartProp.DateTime(loc)
		if err != nil {
			return false, err
		}
		dur, err := masterDurationIn(comp, dtstartProp, dtstart)
		if err != nil {
			return false, err
		}
		set, err := comp.RecurrenceSet(loc)
		if err != nil {
			return false, err
		}
		next := set.Iterator()
		for {
			occStart, ok := next()
			if !ok || !occStart.Before(end) {
				return false, nil
			}
			if occStart.Add(dur).After(start) {
				return true, nil
			}
		}
	}

	effStart, err := dtstartProp.DateTime(loc)
	if err != nil {
		return false, err
	}
	effEnd, err := eventEnd(comp, dtstartProp, effStart, loc)
	if err != nil {
		return false, err
	}
	return start.Before(effEnd) && end.After(effStart), nil
}

// eventEnd resolves a VEVENT's non-inclusive end: DTEND, DTSTART+DURATION,
// +1 day for all-day, else DTSTART.
func eventEnd(comp *goical.Component, dtstartProp *goical.Prop, dtstart time.Time, loc *time.Location) (time.Time, error) {
	if dtendProp := comp.Props.Get(goical.PropDateTimeEnd); dtendProp != nil {
		return dtendProp.DateTime(loc)
	}
	if durProp := comp.Props.Get(goical.PropDuration); durProp != nil {
		dur, err := durProp.Duration()
		if err != nil {
			return time.Time{}, err
		}
		return dtstart.Add(dur), nil
	}
	if dtstartProp.ValueType() == goical.ValueDate {
		return dtstart.AddDate(0, 0, 1), nil
	}
	return dtstart, nil
}

// masterDurationIn is masterDuration with an explicit location (the UTC-only
// variant lives in denormalize.go).
func masterDurationIn(comp *goical.Component, dtstartProp *goical.Prop, dtstart time.Time) (time.Duration, error) {
	end, err := eventEnd(comp, dtstartProp, dtstart, dtstart.Location())
	if err != nil {
		return 0, err
	}
	return end.Sub(dtstart), nil
}

// TodoInTimeRange ports vobject VTodo::isInTimeRange: DTSTART/DURATION/DUE
// combinations, falling back to COMPLETED/CREATED, and matching everything
// for a bare VTODO.
func TodoInTimeRange(comp *goical.Component, start, end time.Time, loc *time.Location) (bool, error) {
	get := func(name string) (time.Time, bool, error) {
		prop := comp.Props.Get(name)
		if prop == nil {
			return time.Time{}, false, nil
		}
		t, err := prop.DateTime(loc)
		return t, err == nil, err
	}
	dtstart, hasStart, err := get(goical.PropDateTimeStart)
	if err != nil {
		return false, err
	}
	due, hasDue, err := get("DUE")
	if err != nil {
		return false, err
	}
	completed, hasCompleted, err := get("COMPLETED")
	if err != nil {
		return false, err
	}
	created, hasCreated, err := get("CREATED")
	if err != nil {
		return false, err
	}

	if hasStart {
		if durProp := comp.Props.Get(goical.PropDuration); durProp != nil {
			dur, err := durProp.Duration()
			if err != nil {
				return false, err
			}
			effectiveEnd := dtstart.Add(dur)
			return !start.After(effectiveEnd) && end.After(dtstart), nil
		}
		if hasDue {
			return (start.Before(due) || !start.After(dtstart)) &&
				(end.After(dtstart) || !end.Before(due)), nil
		}
		return !start.After(dtstart) && end.After(dtstart), nil
	}
	if hasDue {
		return start.Before(due) && !end.Before(due), nil
	}
	if hasCompleted && hasCreated {
		return (!start.After(created) || !start.After(completed)) &&
			(!end.Before(created) || !end.Before(completed)), nil
	}
	if hasCompleted {
		return !start.After(completed) && !end.Before(completed), nil
	}
	if hasCreated {
		return end.After(created), nil
	}
	return true, nil
}

// JournalInTimeRange ports vobject VJournal::isInTimeRange: DTSTART-based
// (all-day entries span one day); journals without DTSTART never match.
func JournalInTimeRange(comp *goical.Component, start, end time.Time, loc *time.Location) (bool, error) {
	prop := comp.Props.Get(goical.PropDateTimeStart)
	if prop == nil {
		return false, nil
	}
	dtstart, err := prop.DateTime(loc)
	if err != nil {
		return false, err
	}
	effectiveEnd := dtstart
	if prop.ValueType() == goical.ValueDate {
		effectiveEnd = dtstart.AddDate(0, 0, 1)
	}
	return !start.After(effectiveEnd) && end.After(dtstart), nil
}

// EffectiveTriggerTime resolves a VALARM's trigger, relative to its parent
// event. Port of vobject VAlarm::getEffectiveTriggerTime (VEVENT parents;
// VTODO alarms come with the todo port).
func EffectiveTriggerTime(alarm, parent *goical.Component, loc *time.Location) (time.Time, error) {
	trigger := alarm.Props.Get("TRIGGER")
	if trigger == nil {
		return time.Time{}, errors.New("VALARM is missing TRIGGER")
	}
	if trigger.ValueType() == goical.ValueDateTime {
		return trigger.DateTime(loc)
	}
	triggerDur, err := trigger.Duration()
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing TRIGGER duration: %w", err)
	}

	dtstartProp := parent.Props.Get(goical.PropDateTimeStart)
	if dtstartProp == nil {
		return time.Time{}, errors.New("VALARM parent is missing DTSTART")
	}
	dtstart, err := dtstartProp.DateTime(loc)
	if err != nil {
		return time.Time{}, err
	}

	if strings.EqualFold(trigger.Params.Get("RELATED"), "END") {
		end, err := eventEnd(parent, dtstartProp, dtstart, loc)
		if err != nil {
			return time.Time{}, err
		}
		return end.Add(triggerDur), nil
	}
	return dtstart.Add(triggerDur), nil
}

// AlarmsInTimeRangeRecurring evaluates the VALARMs of a recurring VEVENT,
// porting sabre's recurring branch of validateTimeRange (the Issue 220
// behavior) exactly: instances are walked in order with overridden instances
// (RECURRENCE-ID) substituted; an instance *without* alarms aborts the whole
// search as a no-match; the walk also stops once the earliest relative
// trigger of an instance passes the range end.
func AlarmsInTimeRangeRecurring(cal *goical.Calendar, master *goical.Component, start, end time.Time, loc *time.Location) (bool, error) {
	uid := ""
	if p := master.Props.Get(goical.PropUID); p != nil {
		uid = p.Value
	}
	overrides := map[int64]*goical.Component{}
	for _, child := range cal.Children {
		if child.Name != goical.CompEvent || child == master {
			continue
		}
		if p := child.Props.Get(goical.PropUID); p == nil || p.Value != uid {
			continue
		}
		if rid := child.Props.Get(goical.PropRecurrenceID); rid != nil {
			if t, err := rid.DateTime(loc); err == nil {
				overrides[t.Unix()] = child
			}
		}
	}

	dtstartProp := master.Props.Get(goical.PropDateTimeStart)
	if dtstartProp == nil {
		return false, nil
	}
	dtstart, err := dtstartProp.DateTime(loc)
	if err != nil {
		return false, err
	}
	dur, err := masterDurationIn(master, dtstartProp, dtstart)
	if err != nil {
		return false, err
	}
	set, err := master.RecurrenceSet(loc)
	if err != nil || set == nil {
		return false, err
	}

	next := set.Iterator()
	// Sabre relies on firstAlarm growing past $end for termination; the
	// cap guards against pathological rules it would spin on.
	for i := 0; i < 100000; i++ {
		occStart, more := next()
		if !more {
			return false, nil
		}
		instance := master
		instStart, instEnd := occStart, occStart.Add(dur)
		if override, exists := overrides[occStart.Unix()]; exists {
			instance = override
			if sp := override.Props.Get(goical.PropDateTimeStart); sp != nil {
				if t, err := sp.DateTime(loc); err == nil {
					instStart = t
					if e, err := eventEnd(override, sp, t, loc); err == nil {
						instEnd = e
					}
				}
			}
		}

		var alarms []*goical.Component
		for _, child := range instance.Children {
			if child.Name == "VALARM" {
				alarms = append(alarms, child)
			}
		}
		if len(alarms) == 0 {
			// "No alarm that will change for every instance was found" —
			// sabre assumes no match and stops here.
			return false, nil
		}

		var firstAlarm *time.Time
		for _, alarm := range alarms {
			trigger := alarm.Props.Get("TRIGGER")
			if trigger == nil {
				continue
			}
			var effective time.Time
			absolute := trigger.ValueType() == goical.ValueDateTime
			if absolute {
				if effective, err = trigger.DateTime(loc); err != nil {
					continue
				}
			} else {
				triggerDur, err := trigger.Duration()
				if err != nil {
					continue
				}
				if strings.EqualFold(trigger.Params.Get("RELATED"), "END") {
					effective = instEnd.Add(triggerDur)
				} else {
					effective = instStart.Add(triggerDur)
				}
			}
			if alarmRepeatsInRange(alarm, effective, start, end) {
				return true, nil
			}
			if !absolute {
				if firstAlarm == nil || effective.Before(*firstAlarm) {
					t := effective
					firstAlarm = &t
				}
			}
		}
		if firstAlarm == nil {
			return false, nil
		}
		if firstAlarm.After(end) {
			return false, nil
		}
	}
	return false, nil
}

// alarmRepeatsInRange checks a trigger (plus its REPEAT/DURATION
// repetitions) against [start, end).
func alarmRepeatsInRange(alarm *goical.Component, trigger, start, end time.Time) bool {
	repeat := 0
	var dur time.Duration
	if durProp := alarm.Props.Get(goical.PropDuration); durProp != nil {
		if d, err := durProp.Duration(); err == nil {
			dur = d
			repeat = 1
			if repProp := alarm.Props.Get("REPEAT"); repProp != nil {
				if n, err := repProp.Int(); err == nil {
					repeat = n
				}
			}
		}
	}
	occ := trigger
	for i := 0; i <= repeat; i++ {
		if !start.After(occ) && end.After(occ) {
			return true
		}
		occ = occ.Add(dur)
	}
	return false
}

// AlarmInTimeRange reports whether a VALARM's (possibly repeating) trigger
// falls within [start, end). Port of vobject VAlarm::isInTimeRange.
func AlarmInTimeRange(alarm, parent *goical.Component, start, end time.Time, loc *time.Location) (bool, error) {
	trigger, err := EffectiveTriggerTime(alarm, parent, loc)
	if err != nil {
		return false, err
	}
	durProp := alarm.Props.Get(goical.PropDuration)
	if durProp == nil {
		return !start.After(trigger) && end.After(trigger), nil
	}
	dur, err := durProp.Duration()
	if err != nil {
		return false, err
	}
	repeat := 1
	if repProp := alarm.Props.Get("REPEAT"); repProp != nil {
		if n, err := repProp.Int(); err == nil {
			repeat = n
		}
	}
	// PHP's DatePeriod with recurrences=N yields N+1 values (the start
	// plus N repetitions).
	occ := trigger
	for i := 0; i <= repeat; i++ {
		if !start.After(occ) && end.After(occ) {
			return true, nil
		}
		occ = occ.Add(dur)
	}
	return false, nil
}
