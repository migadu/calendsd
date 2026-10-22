package ical

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	goical "github.com/emersion/go-ical"
)

// JCal serializes a calendar as jCal (RFC 7265): nested
// ["name", [properties], [components]] arrays.
func JCal(cal *goical.Calendar) ([]byte, error) {
	return json.Marshal(jcalComponent(cal.Component))
}

func jcalComponent(comp *goical.Component) []any {
	props := []any{}
	for _, list := range comp.Props {
		for _, prop := range list {
			props = append(props, jcalProp(&prop))
		}
	}
	// go-ical's prop map loses source order; sort for determinism (the
	// corpus comparator is order-insensitive).
	sortJSONSlice(props)

	comps := []any{}
	for _, child := range comp.Children {
		comps = append(comps, jcalComponent(child))
	}
	return []any{strings.ToLower(comp.Name), props, comps}
}

func jcalProp(prop *goical.Prop) []any {
	params := map[string]any{}
	for name, values := range prop.Params {
		if strings.EqualFold(name, "VALUE") {
			continue // the type slot carries this in jCal
		}
		if len(values) == 1 {
			params[strings.ToLower(name)] = values[0]
		} else {
			params[strings.ToLower(name)] = values
		}
	}
	jType := jcalType(prop.ValueType())
	out := []any{strings.ToLower(prop.Name), params, jType}
	for _, v := range jcalValues(prop, jType) {
		out = append(out, jcalValue(jType, v))
	}
	return out
}

// jcalType maps an iCalendar value type to its jCal type name.
func jcalType(t goical.ValueType) string {
	switch t {
	case goical.ValueDefault:
		return "unknown"
	case goical.ValueBool, goical.ValueCalendarAddress, goical.ValueDate,
		goical.ValueDateTime, goical.ValueDuration, goical.ValueFloat,
		goical.ValueInt, goical.ValuePeriod, goical.ValueRecurrence,
		goical.ValueText, goical.ValueTime, goical.ValueURI,
		goical.ValueUTCOffset, goical.ValueBinary:
		return strings.ToLower(string(t))
	}
	return "unknown"
}

// listProps are the properties whose comma-separated value is a list, each
// item a separate jCal value (RFC 7265 §3.4.1.2).
var listProps = map[string]bool{
	goical.PropCategories: true, goical.PropResources: true,
	goical.PropExceptionDates: true, goical.PropRecurrenceDates: true,
	goical.PropFreeBusy: true,
}

// jcalValues splits a property value into its jCal values: one per list
// item for listProps, else the whole value.
func jcalValues(prop *goical.Prop, jType string) []string {
	if !listProps[prop.Name] {
		return []string{prop.Value}
	}
	if jType == "text" {
		return splitText(prop.Value)
	}
	return strings.Split(prop.Value, ",")
}

// splitText splits a TEXT list on the commas that are not escaped.
func splitText(v string) []string {
	var out []string
	start := 0
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\\':
			i++
		case ',':
			out = append(out, v[start:i])
			start = i + 1
		}
	}
	return append(out, v[start:])
}

// jcalValue converts one iCalendar value to its jCal form.
func jcalValue(jType, v string) any {
	switch jType {
	case "text":
		if text, err := (&goical.Prop{Value: v}).Text(); err == nil {
			return text
		}
		return v
	case "date-time":
		return jcalDateTime(v)
	case "date":
		return jcalDate(v)
	case "time":
		return jcalTime(v)
	case "integer", "float":
		if parts := strings.Split(v, ";"); len(parts) > 1 {
			// GEO is a structured float pair.
			out := make([]any, len(parts))
			for i, p := range parts {
				out[i] = jsonNumber(p)
			}
			return out
		}
		return jsonNumber(v)
	case "boolean":
		return strings.EqualFold(v, "TRUE")
	case "utc-offset":
		return jcalUTCOffset(v)
	case "period":
		start, end, ok := strings.Cut(v, "/")
		if !ok {
			return v
		}
		if !strings.HasPrefix(end, "P") && !strings.HasPrefix(end, "+P") && !strings.HasPrefix(end, "-P") {
			end = jcalDateTime(end)
		}
		return []any{jcalDateTime(start), end}
	case "recur":
		return jcalRecur(v)
	}
	return v
}

// jcalDateTime converts 20151020T000000[Z] to 2015-10-20T00:00:00[Z].
func jcalDateTime(v string) string {
	utc := strings.HasSuffix(v, "Z")
	v = strings.TrimSuffix(v, "Z")
	if len(v) != 15 || v[8] != 'T' {
		return v
	}
	out := v[0:4] + "-" + v[4:6] + "-" + v[6:8] + "T" + v[9:11] + ":" + v[11:13] + ":" + v[13:15]
	if utc {
		out += "Z"
	}
	return out
}

func jcalDate(v string) string {
	if len(v) != 8 {
		return v
	}
	return v[0:4] + "-" + v[4:6] + "-" + v[6:8]
}

// jcalTime converts 103000[Z] to 10:30:00[Z].
func jcalTime(v string) string {
	utc := strings.HasSuffix(v, "Z")
	v = strings.TrimSuffix(v, "Z")
	if len(v) != 6 {
		return v
	}
	out := v[0:2] + ":" + v[2:4] + ":" + v[4:6]
	if utc {
		out += "Z"
	}
	return out
}

// jcalUTCOffset converts -0500 to -05:00 (and +053045 to +05:30:45).
func jcalUTCOffset(v string) string {
	if len(v) != 5 && len(v) != 7 {
		return v
	}
	out := v[0:3] + ":" + v[3:5]
	if len(v) == 7 {
		out += ":" + v[5:7]
	}
	return out
}

// recurIntParts are the RRULE parts whose values are integers.
var recurIntParts = map[string]bool{
	"count": true, "interval": true, "bysecond": true, "byminute": true,
	"byhour": true, "bymonthday": true, "byyearday": true, "byweekno": true,
	"bymonth": true, "bysetpos": true,
}

// jcalRecur renders an RRULE value as the jCal recur object (RFC 7265
// §3.6.10): integer parts as numbers, a multi-valued part as an array, and
// UNTIL as a jCal date or date-time.
func jcalRecur(v string) map[string]any {
	out := map[string]any{}
	for _, part := range strings.Split(v, ";") {
		k, val, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		k = strings.ToLower(k)
		if k == "until" {
			if len(val) == 8 {
				out[k] = jcalDate(val)
			} else {
				out[k] = jcalDateTime(val)
			}
			continue
		}
		items := strings.Split(val, ",")
		values := make([]any, len(items))
		for i, item := range items {
			if recurIntParts[k] {
				values[i] = jsonNumber(item)
			} else {
				values[i] = item
			}
		}
		if len(values) == 1 {
			out[k] = values[0]
		} else {
			out[k] = values
		}
	}
	return out
}

// jsonNumber renders a numeric value as a JSON number, or leaves it a string
// when it isn't one.
func jsonNumber(v string) any {
	if i, err := strconv.ParseInt(v, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
		return f
	}
	return v
}

// sortJSONSlice orders entries by their JSON form for determinism.
func sortJSONSlice(items []any) {
	keys := make([]string, len(items))
	for i, item := range items {
		b, _ := json.Marshal(item)
		keys[i] = string(b)
	}
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
