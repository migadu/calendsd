package ical

import (
	"encoding/json"
	"reflect"
	"testing"
)

// jcalProps returns the jCal properties of the first component under the
// root, by name.
func jcalProps(t *testing.T, data []byte) map[string][]any {
	t.Helper()
	var root []any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	out := map[string][]any{}
	for _, comp := range root[2].([]any) {
		c := comp.([]any)
		if c[0] != "vevent" && c[0] != "vtimezone" {
			continue
		}
		for _, p := range c[1].([]any) {
			prop := p.([]any)
			out[prop[0].(string)] = prop
		}
		for _, sub := range c[2].([]any) {
			for _, p := range sub.([]any)[1].([]any) {
				prop := p.([]any)
				out[prop[0].(string)] = prop
			}
		}
	}
	return out
}

// jCal used to pass several value types through in their iCalendar form: a
// list as one comma-joined string, ATTENDEE as "unknown", a UTC offset
// without its colon, a PERIOD as "start/end", and every RRULE part (UNTIL
// included) as a string. Decoding didn't undo the jCal forms either.
func TestJCalRFC7265(t *testing.T) {
	data := ics(
		"BEGIN:VTIMEZONE", "TZID:Custom",
		"BEGIN:STANDARD", "DTSTART:19701101T020000",
		"TZOFFSETFROM:-0400", "TZOFFSETTO:-0500", "END:STANDARD",
		"END:VTIMEZONE",
		"BEGIN:VEVENT", "UID:j", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260302T090000Z",
		"RRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE;UNTIL=20261231T000000Z",
		"EXDATE:20260304T090000Z,20260316T090000Z",
		"RDATE;VALUE=PERIOD:20260401T090000Z/20260401T100000Z",
		`CATEGORIES:a,b\,c`,
		"GEO:37.386013;-122.082932",
		"ATTENDEE:mailto:a@example.org",
		"END:VEVENT",
	)
	cal, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	out, err := JCal(cal)
	if err != nil {
		t.Fatal(err)
	}
	got := jcalProps(t, out)
	for name, want := range map[string]any{
		"exdate":       []any{"exdate", map[string]any{}, "date-time", "2026-03-04T09:00:00Z", "2026-03-16T09:00:00Z"},
		"rdate":        []any{"rdate", map[string]any{}, "period", []any{"2026-04-01T09:00:00Z", "2026-04-01T10:00:00Z"}},
		"categories":   []any{"categories", map[string]any{}, "text", "a", "b,c"},
		"geo":          []any{"geo", map[string]any{}, "float", []any{37.386013, -122.082932}},
		"attendee":     []any{"attendee", map[string]any{}, "cal-address", "mailto:a@example.org"},
		"tzoffsetfrom": []any{"tzoffsetfrom", map[string]any{}, "utc-offset", "-04:00"},
		"rrule": []any{"rrule", map[string]any{}, "recur", map[string]any{
			"freq": "WEEKLY", "interval": 2.0, "byday": []any{"MO", "WE"}, "until": "2026-12-31T00:00:00Z",
		}},
	} {
		if !reflect.DeepEqual(got[name], want) {
			t.Errorf("%s:\n got %#v\nwant %#v", name, got[name], want)
		}
	}

	back, err := DecodeJCal(out)
	if err != nil {
		t.Fatal(err)
	}
	orig, decoded := cal.Children, back.Children
	for i, comp := range orig {
		for name, props := range comp.Props {
			for _, p := range props {
				if name == "RRULE" {
					continue // part order differs; checked below
				}
				dp := decoded[i].Props.Get(name)
				if dp == nil || dp.Value != p.Value || dp.Params.Get("VALUE") != p.Params.Get("VALUE") {
					t.Errorf("%s round-trip = %+v, want %+v", name, dp, p)
				}
			}
		}
	}
	for _, sub := range back.Children[0].Children {
		if v := sub.Props.Get("TZOFFSETFROM").Value; v != "-0400" {
			t.Errorf("TZOFFSETFROM round-trip = %q, want -0400", v)
		}
	}
	if rr := back.Children[1].Props.Get("RRULE").Value; rr != "FREQ=WEEKLY;BYDAY=MO,WE;INTERVAL=2;UNTIL=20261231T000000Z" {
		t.Errorf("RRULE round-trip = %q", rr)
	}
}
