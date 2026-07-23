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
