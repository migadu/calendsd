package ical

import (
	"errors"
	"fmt"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"
)

// ErrTooManyInstances is returned when a recurrence rule produces more
// instances than a scan is willing to materialise. It is the Go counterpart of
// sabre vobject's MaxInstancesExceededException; callers decide what the
// conservative answer is (time-range: match; free-busy: busy; PUT: refuse).
var ErrTooManyInstances = errors.New("recurrence rule produces too many instances")

// maxPreWindowScan bounds the instances a window scan may skip *before* the
// window starts. The skip is cheap (no allocation, one iterator step), so the
// budget is generous enough that a decade-old daily series or a years-old
// hourly one still reaches its window; it exists only so a sub-daily infinite
// rule cannot spin for minutes. Instances inside the window count against the
// shared maxRecurrences cap instead (audit item 3.1, D7).
//
// Sabre's EventIterator throws MaxInstancesExceededException after
// Settings::$maxRecurrences (3500) instances *counted from DTSTART*, including
// inside VEvent::isInTimeRange, because its fastForward iterates rather than
// jumps. Counting only in-window instances is deliberately more lenient to
// long-running series; counting pre-window ones at all is what sabre lacks.
const maxPreWindowScan = 100_000

// maxTimeRangeScan bounds the skip-ahead loops that look for one target
// instant (expand's walk to the window, iTIP's search for an occurrence):
// the same budget as the pre-window skip, for the same reason.
const maxTimeRangeScan = maxPreWindowScan

// maxAlarmRepeat caps a VALARM's REPEAT. The value is also what sabre's
// iterator would tolerate for a recurrence; a client-supplied
// REPEAT:9223372036854775807 otherwise pins a core (audit item 3.2).
const maxAlarmRepeat = 3500

// scanWindow walks a recurrence iterator over [start, end), calling visit for
// each instance that overlaps the window (an instance starting at occ and
// ending at endOf(occ) overlaps when occ < end and endOf(occ) > start). It stops when visit
// returns true, when the series ends, or when the next instance starts at or
// after end. Two budgets bound the walk: maxPreWindowScan for instances that
// end before start, and maxRecurrences for instances handed to visit. When a
// budget runs out the error is ErrTooManyInstances and last is the start of
// the last instance seen, so the caller can answer conservatively from there.
func scanWindow(next func() (time.Time, bool), endOf func(time.Time) time.Time, start, end time.Time, visit func(occ time.Time) (stop bool)) (last time.Time, err error) {
	pre, in := 0, 0
	for {
		occ, ok := next()
		if !ok || !occ.Before(end) {
			return last, nil
		}
		last = occ
		if !endOf(occ).After(start) {
			pre++
			if pre > maxPreWindowScan {
				return last, ErrTooManyInstances
			}
			continue
		}
		in++
		if in > maxRecurrences {
			return last, ErrTooManyInstances
		}
		if visit(occ) {
			return last, nil
		}
	}
}

// EventInTimeRange reports whether a VEVENT overlaps [start, end).
// Port of vobject VEvent::isInTimeRange: recurring events match if any
// occurrence overlaps; DTEND is non-inclusive. loc resolves floating times.
func EventInTimeRange(comp *goical.Component, start, end time.Time, loc *time.Location) (bool, error) {
	dtstartProp := comp.Props.Get(goical.PropDateTimeStart)
	if dtstartProp == nil {
		return false, errors.New("VEVENT is missing DTSTART")
	}
	if _, err := comp.Props.RecurrenceRule(); err != nil {
		return false, fmt.Errorf("parsing RRULE: %w", err)
	}

	if IsRecurring(comp) {
		dtstart, err := dtstartProp.DateTime(loc)
		if err != nil {
			return false, err
		}
		endOf, err := occurrenceEnd(comp, dtstartProp, dtstart)
		if err != nil {
			return false, err
		}
		set, err := recurrenceSet(comp, loc)
		if err != nil {
			return false, err
		}
		// The first instance overlapping the window is a match. A series too
		// dense to scan is reported as matching: the client gets an object it
		// may not have needed rather than silently losing one (audit 3.1).
		matched := false
		_, err = scanWindow(set.Iterator(), endOf, start, end, func(time.Time) bool {
			matched = true
			return true
		})
		if errors.Is(err, ErrTooManyInstances) {
			return true, nil
		}
		return matched, err
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
		dtend, err := dtendProp.DateTime(loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("parsing DTEND: %w", err)
		}
		return dtend, nil
	}
	if durProp := comp.Props.Get(goical.PropDuration); durProp != nil {
		end, err := AddDuration(dtstart, durProp.Value)
		if err != nil {
			return time.Time{}, fmt.Errorf("parsing DURATION: %w", err)
		}
		return end, nil
	}
	if dtstartProp.ValueType() == goical.ValueDate {
		return dtstart.AddDate(0, 0, 1), nil
	}
	return dtstart, nil
}

// occurrenceEnd returns the function from an occurrence's start to its end,
// following the master's DTEND/DURATION. An all-day (DATE) series spans whole
// days: a fixed duration would end an occurrence on the far side of a DST
// change in the start's location an hour early or late, which for a one-day
// event is the wrong date.
func occurrenceEnd(comp *goical.Component, dtstartProp *goical.Prop, dtstart time.Time) (func(time.Time) time.Time, error) {
	end, err := eventEnd(comp, dtstartProp, dtstart, dtstart.Location())
	if err != nil {
		return nil, err
	}
	if dtstartProp.ValueType() != goical.ValueDate {
		dur := end.Sub(dtstart)
		return func(t time.Time) time.Time { return t.Add(dur) }, nil
	}
	y1, m1, d1 := dtstart.Date()
	y2, m2, d2 := end.Date()
	days := int(time.Date(y2, m2, d2, 0, 0, 0, 0, time.UTC).Sub(time.Date(y1, m1, d1, 0, 0, 0, 0, time.UTC)) / (24 * time.Hour))
	rest := end.Sub(dtstart.AddDate(0, 0, days))
	return func(t time.Time) time.Time { return t.AddDate(0, 0, days).Add(rest) }, nil
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
			effectiveEnd, err := AddDuration(dtstart, durProp.Value)
			if err != nil {
				return false, err
			}
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
// VEVENT or VTODO. Port of vobject VAlarm::getEffectiveTriggerTime.
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
	if parent.Name == goical.CompToDo {
		anchor, err := todoAlarmAnchor(parent, strings.EqualFold(trigger.Params.Get("RELATED"), "END"), loc)
		if err != nil {
			return time.Time{}, err
		}
		return anchor.Add(triggerDur), nil
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

// todoAlarmAnchor is the time a VTODO's relative trigger counts from: its
// DTSTART, or for RELATED=END its DUE, else DTSTART+DURATION (RFC 5545
// §3.8.6.3). A todo without DTSTART anchors a START-related trigger on DUE,
// so the alarm of a todo with only a due date still fires.
func todoAlarmAnchor(todo *goical.Component, relatedEnd bool, loc *time.Location) (time.Time, error) {
	var start, due time.Time
	var err error
	sp, dp := todo.Props.Get(goical.PropDateTimeStart), todo.Props.Get("DUE")
	if sp != nil {
		if start, err = sp.DateTime(loc); err != nil {
			return time.Time{}, err
		}
	}
	if dp != nil {
		if due, err = dp.DateTime(loc); err != nil {
			return time.Time{}, err
		}
	}
	switch {
	case sp != nil && !relatedEnd:
		return start, nil
	case dp != nil:
		return due, nil
	case sp != nil:
		if durProp := todo.Props.Get(goical.PropDuration); durProp != nil {
			return AddDuration(start, durProp.Value)
		}
		return start, nil
	}
	return time.Time{}, errors.New("VALARM parent VTODO has neither DTSTART nor DUE")
}

// AlarmsInTimeRangeRecurring evaluates the VALARMs of a recurring VEVENT,
// porting sabre's recurring branch of validateTimeRange (the Issue 220
// behavior) exactly: instances are walked in order with overridden instances
// (RECURRENCE-ID) substituted; an instance *without* alarms aborts the whole
// search as a no-match; the walk also stops once the earliest relative
// trigger of an instance passes the range end.
func AlarmsInTimeRangeRecurring(cal *goical.Calendar, master *goical.Component, start, end time.Time, loc *time.Location) (bool, error) {
	overrides := overridesOf(cal, master, loc)

	dtstartProp := master.Props.Get(goical.PropDateTimeStart)
	if dtstartProp == nil {
		return false, nil
	}
	dtstart, err := dtstartProp.DateTime(loc)
	if err != nil {
		return false, err
	}
	endOf, err := occurrenceEnd(master, dtstartProp, dtstart)
	if err != nil {
		return false, err
	}
	set, err := recurrenceSet(master, loc)
	if err != nil || set == nil {
		return false, err
	}

	next := set.Iterator()
	// Sabre relies on firstAlarm growing past $end for termination; the
	// cap guards against pathological rules it would spin on.
	for i := 0; i < maxRecurrences; i++ {
		occStart, more := next()
		if !more {
			return false, nil
		}
		instance := master
		instStart, instEnd := occStart, endOf(occStart)
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

// alarmRepetition reads a VALARM's DURATION and REPEAT: how far apart its
// repetitions are and how many follow the trigger. Without DURATION there are
// none; with one, REPEAT defaults to 1 (sabre's DatePeriod with
// recurrences=N yields the start plus N repetitions, and PHP reads a missing,
// unparseable or zero REPEAT as 1). REPEAT is capped at maxAlarmRepeat so a
// client-supplied REPEAT:9223372036854775807 cannot pin a core (audit 3.2).
func alarmRepetition(alarm *goical.Component) (time.Duration, int, error) {
	durProp := alarm.Props.Get(goical.PropDuration)
	if durProp == nil {
		return 0, 0, nil
	}
	dur, err := durProp.Duration()
	if err != nil {
		return 0, 0, err
	}
	repeat := 1
	if repProp := alarm.Props.Get("REPEAT"); repProp != nil {
		if n, err := repProp.Int(); err == nil && n > 0 {
			repeat = n
		}
	}
	if repeat > maxAlarmRepeat {
		repeat = maxAlarmRepeat
	}
	return dur, repeat, nil
}

// alarmRepeatsInRange checks a trigger (plus its REPEAT/DURATION
// repetitions) against [start, end). A malformed DURATION counts as no
// repetition.
func alarmRepeatsInRange(alarm *goical.Component, trigger, start, end time.Time) bool {
	dur, repeat, err := alarmRepetition(alarm)
	if err != nil {
		dur, repeat = 0, 0
	}
	return repetitionInRange(trigger, dur, repeat, start, end)
}

// repetitionInRange walks the trigger and its repetitions. The walk stops as
// soon as an occurrence reaches end, since the sequence is monotonic for a
// positive DURATION; a zero or negative DURATION (invalid per RFC 5545) is
// treated as a single occurrence so the loop cannot stand still (audit 3.2).
func repetitionInRange(trigger time.Time, dur time.Duration, repeat int, start, end time.Time) bool {
	if dur <= 0 {
		repeat = 0
	}
	occ := trigger
	for i := 0; i <= repeat; i++ {
		if !occ.Before(end) {
			return false
		}
		if !start.After(occ) {
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
	dur, repeat, err := alarmRepetition(alarm)
	if err != nil {
		return false, err
	}
	return repetitionInRange(trigger, dur, repeat, start, end), nil
}
