package ical

import (
	"encoding/json"
	"strings"

	goical "github.com/emersion/go-ical"
)

// JCal serializes a calendar as jCal (RFC 7265): nested
// ["name", [properties], [components]] arrays. Covers the subset sabre's
// jsonSerialize emits for export and jCal transforms.
func JCal(cal *goical.Calendar) ([]byte, error) {
	return json.Marshal(jcalComponent(cal.Component))
}

func jcalComponent(comp *goical.Component) []any {
	props := []any{}
	for name, list := range comp.Props {
		_ = name
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
	jType, value := jcalTypeValue(prop)
	return []any{strings.ToLower(prop.Name), params, jType, value}
}

// jcalTypeValue maps a property to its jCal type name and converted value.
func jcalTypeValue(prop *goical.Prop) (string, any) {
	switch prop.ValueType() {
	case goical.ValueText:
		text, err := prop.Text()
		if err != nil {
			return "text", prop.Value
		}
		return "text", text
	case goical.ValueDateTime:
		return "date-time", jcalDateTime(prop.Value)
	case goical.ValueDate:
		return "date", jcalDate(prop.Value)
	case goical.ValueDuration:
		return "duration", prop.Value
	case goical.ValueInt:
		return "integer", jsonNumber(prop.Value)
	case goical.ValueUTCOffset:
		return "utc-offset", prop.Value
	case goical.ValueURI:
		return "uri", prop.Value
	case goical.ValueBool:
		return "boolean", strings.EqualFold(prop.Value, "TRUE")
	case goical.ValueRecurrence:
		return "recur", jcalRecur(prop.Value)
	case goical.ValuePeriod:
		return "period", prop.Value
	default:
		return "unknown", prop.Value
	}
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

// jcalRecur renders an RRULE value as the jCal recur object.
func jcalRecur(v string) map[string]any {
	out := map[string]any{}
	for _, part := range strings.Split(v, ";") {
		k, val, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		out[strings.ToLower(k)] = val
	}
	return out
}

func jsonNumber(v string) any {
	var n json.Number = json.Number(v)
	if i, err := n.Int64(); err == nil {
		return i
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
