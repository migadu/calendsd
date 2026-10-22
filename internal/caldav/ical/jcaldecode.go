package ical

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	goical "github.com/emersion/go-ical"
)

// IsJCal reports whether data looks like a jCal (RFC 7265) body: a JSON array
// rather than an iCalendar "BEGIN:VCALENDAR" stream.
func IsJCal(data []byte) bool {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '['
}

// DecodeJCal parses a jCal document into a calendar, the inverse of JCal.
// It is deliberately strict: any structural problem returns an error so the
// caller rejects the request rather than storing an unusable body. sabre
// performs the same jCal-to-iCalendar conversion on PUT.
func DecodeJCal(data []byte) (*goical.Calendar, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root []any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("invalid jCal: %w", err)
	}
	comp, err := decodeJCalComponent(root, 0)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(comp.Name, "VCALENDAR") {
		return nil, fmt.Errorf("jCal root component must be vcalendar, got %q", comp.Name)
	}
	return &goical.Calendar{Component: comp}, nil
}

// maxJCalDepth caps jCal subcomponent nesting so a deeply-nested body cannot
// exhaust the stack during the recursive decode.
const maxJCalDepth = 100

func decodeJCalComponent(node []any, depth int) (*goical.Component, error) {
	if depth > maxJCalDepth {
		return nil, fmt.Errorf("jCal component nesting exceeds %d levels", maxJCalDepth)
	}
	if len(node) != 3 {
		return nil, errors.New("jCal component must be a 3-element array")
	}
	name, ok := node[0].(string)
	if !ok {
		return nil, errors.New("jCal component name must be a string")
	}
	comp := &goical.Component{Name: strings.ToUpper(name), Props: goical.Props{}}

	props, ok := node[1].([]any)
	if !ok {
		return nil, fmt.Errorf("jCal component %q properties must be an array", name)
	}
	for _, p := range props {
		pa, ok := p.([]any)
		if !ok {
			return nil, errors.New("jCal property must be an array")
		}
		prop, err := decodeJCalProp(pa)
		if err != nil {
			return nil, err
		}
		comp.Props[prop.Name] = append(comp.Props[prop.Name], *prop)
	}

	comps, ok := node[2].([]any)
	if !ok {
		return nil, fmt.Errorf("jCal component %q subcomponents must be an array", name)
	}
	for _, c := range comps {
		ca, ok := c.([]any)
		if !ok {
			return nil, errors.New("jCal subcomponent must be an array")
		}
		child, err := decodeJCalComponent(ca, depth+1)
		if err != nil {
			return nil, err
		}
		comp.Children = append(comp.Children, child)
	}
	return comp, nil
}

func decodeJCalProp(node []any) (*goical.Prop, error) {
	if len(node) < 4 {
		return nil, errors.New("jCal property must have at least 4 elements")
	}
	name, ok := node[0].(string)
	if !ok {
		return nil, errors.New("jCal property name must be a string")
	}
	prop := &goical.Prop{Name: strings.ToUpper(name), Params: goical.Params{}}

	if paramsObj, ok := node[1].(map[string]any); ok {
		for k, v := range paramsObj {
			key := strings.ToUpper(k)
			switch vv := v.(type) {
			case string:
				prop.Params[key] = append(prop.Params[key], vv)
			case []any:
				for _, item := range vv {
					if s, ok := item.(string); ok {
						prop.Params[key] = append(prop.Params[key], s)
					}
				}
			}
		}
	}

	jType, _ := node[2].(string)
	jType = strings.ToLower(jType)
	value, err := decodeJCalValue(jType, node[3:])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prop.Name, err)
	}
	// A type other than the property's default (TEXT for one go-ical doesn't
	// know) is signalled with a VALUE parameter (RFC 7265 §4.2): DATE for an
	// all-day DTSTART, PERIOD for an RDATE, DATE-TIME for a TRIGGER.
	def := (&goical.Prop{Name: prop.Name}).ValueType()
	if def == goical.ValueDefault {
		def = goical.ValueText
	}
	if jType != "unknown" && jType != "" && !strings.EqualFold(jType, string(def)) {
		prop.Params["VALUE"] = []string{strings.ToUpper(jType)}
	}
	prop.Value = value
	return prop, nil
}

// decodeJCalValue converts the jCal value slot(s) to an iCalendar value
// string. Multiple values are joined with commas as in iCalendar.
func decodeJCalValue(jType string, values []any) (string, error) {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		s, err := decodeJCalScalar(jType, v)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ","), nil
}

func decodeJCalScalar(jType string, v any) (string, error) {
	switch jType {
	case "date-time":
		s, ok := v.(string)
		if !ok {
			return "", errors.New("date-time value must be a string")
		}
		return icalDateTime(s), nil
	case "time", "utc-offset":
		// 10:30:00 -> 103000, -05:00 -> -0500
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("%s value must be a string", jType)
		}
		return strings.ReplaceAll(s, ":", ""), nil
	case "date":
		s, ok := v.(string)
		if !ok {
			return "", errors.New("date value must be a string")
		}
		return strings.ReplaceAll(s, "-", ""), nil
	case "period":
		// [start, end or duration]; the start is a jCal date-time.
		pair, ok := v.([]any)
		if !ok || len(pair) != 2 {
			return "", errors.New("period value must be a [start, end] array")
		}
		start, ok1 := pair[0].(string)
		end, ok2 := pair[1].(string)
		if !ok1 || !ok2 {
			return "", errors.New("period value must be a [start, end] array of strings")
		}
		if !strings.HasPrefix(strings.TrimLeft(end, "+-"), "P") {
			end = icalDateTime(end)
		}
		return icalDateTime(start) + "/" + end, nil
	case "float":
		if pair, ok := v.([]any); ok {
			// GEO: [latitude, longitude].
			items := make([]string, len(pair))
			for i, item := range pair {
				items[i] = fmt.Sprintf("%v", item)
			}
			return strings.Join(items, ";"), nil
		}
		return fmt.Sprintf("%v", v), nil
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return "", errors.New("boolean value must be true or false")
		}
		if b {
			return "TRUE", nil
		}
		return "FALSE", nil
	case "integer":
		return fmt.Sprintf("%v", v), nil
	case "recur":
		m, ok := v.(map[string]any)
		if !ok {
			return "", errors.New("recur value must be an object")
		}
		return icalRecur(m), nil
	case "text":
		s, ok := v.(string)
		if !ok {
			return "", errors.New("text value must be a string")
		}
		return escapeICalText(s), nil
	default:
		// duration, uri, cal-address, utc-offset, period, binary, unknown:
		// the jCal form already matches the iCalendar string form.
		if s, ok := v.(string); ok {
			return s, nil
		}
		return fmt.Sprintf("%v", v), nil
	}
}

// icalDateTime converts 2015-10-20T00:00:00[Z] back to 20151020T000000[Z].
func icalDateTime(v string) string {
	r := strings.NewReplacer("-", "", ":", "")
	return r.Replace(v)
}

// icalRecur reassembles an RRULE string from the jCal recur object, FREQ first.
func icalRecur(m map[string]any) string {
	part := func(k string, v any) string {
		if s, ok := v.(string); ok && strings.EqualFold(k, "until") {
			return "UNTIL=" + icalDateTime(s)
		}
		switch vv := v.(type) {
		case []any:
			items := make([]string, 0, len(vv))
			for _, item := range vv {
				items = append(items, fmt.Sprintf("%v", item))
			}
			return strings.ToUpper(k) + "=" + strings.Join(items, ",")
		default:
			return strings.ToUpper(k) + "=" + fmt.Sprintf("%v", vv)
		}
	}
	var out []string
	if freq, ok := m["freq"]; ok {
		out = append(out, part("freq", freq))
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if strings.EqualFold(k, "freq") {
			continue
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		out = append(out, part(k, m[k]))
	}
	return strings.Join(out, ";")
}

// escapeICalText applies RFC 5545 §3.3.11 escaping to a text value.
func escapeICalText(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "\n", "\\n", ";", "\\;", ",", "\\,")
	return r.Replace(s)
}
