package ical

import (
	"fmt"
	"sort"
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
// events expand within the range, and overlapping same-type periods coalesce
// (pinned against sabre's output in testdata/freebusy-golden.ics).
func GenerateFreeBusy(objects [][]byte, start, end time.Time, loc *time.Location, now time.Time) ([]byte, error) {
	var periods []busyPeriod
	for _, data := range objects {
		cal, err := Decode(data)
		if err != nil {
			continue // unparseable objects contribute nothing, like sabre
		}
		for _, comp := range cal.Children {
			if comp.Name != goical.CompEvent {
				continue
			}
			p, err := eventBusyPeriods(comp, start, end, loc)
			if err != nil {
				return nil, err
			}
			periods = append(periods, p...)
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
		sort.Slice(group, func(i, j int) bool { return group[i].start.Before(group[j].start) })
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
	sort.Slice(out, func(i, j int) bool { return out[i].start.Before(out[j].start) })
	return out
}

func eventBusyPeriods(comp *goical.Component, start, end time.Time, loc *time.Location) ([]busyPeriod, error) {
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
	dur, err := masterDurationIn(comp, dtstartProp, dtstart)
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

	roption, err := comp.Props.RecurrenceRule()
	if err != nil {
		return nil, err
	}
	if roption == nil {
		if p := clamp(dtstart, dtstart.Add(dur)); p != nil {
			return []busyPeriod{*p}, nil
		}
		return nil, nil
	}

	set, err := comp.RecurrenceSet(loc)
	if err != nil {
		return nil, err
	}
	var out []busyPeriod
	next := set.Iterator()
	for {
		occStart, more := next()
		if !more || !occStart.Before(end) {
			return out, nil
		}
		if p := clamp(occStart, occStart.Add(dur)); p != nil {
			out = append(out, *p)
		}
	}
}
