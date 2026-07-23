package corpus

import (
	"sort"
	"strings"
)

// canonicalizeICS normalizes an iCalendar payload for semantic comparison:
// lines unfolded, PRODID masked (ours differs from sabre/vobject's), property
// order sorted within each component, sibling components sorted by their
// canonical form. Property order is not significant in iCalendar (RFC 5545
// §3.6), so this compares meaning, not byte order — the corpus contract
// (PLAN.md §7).
func canonicalizeICS(text string) string {
	if !strings.Contains(text, "BEGIN:VCALENDAR") {
		return text
	}
	lines := unfoldICS(text)
	root, _ := parseICSComponent(lines, 0)
	if root == nil {
		return text
	}
	return root.canonical()
}

func unfoldICS(text string) []string {
	raw := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var out []string
	for _, line := range raw {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(out) > 0 {
			out[len(out)-1] += line[1:]
			continue
		}
		out = append(out, line)
	}
	return out
}

type icsComponent struct {
	name     string
	props    []string
	children []*icsComponent
}

// parseICSComponent parses starting at a BEGIN line, returning the component
// and the index after its END line.
func parseICSComponent(lines []string, start int) (*icsComponent, int) {
	if start >= len(lines) || !strings.HasPrefix(lines[start], "BEGIN:") {
		return nil, start
	}
	comp := &icsComponent{name: strings.TrimPrefix(lines[start], "BEGIN:")}
	i := start + 1
	for i < len(lines) {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "BEGIN:"):
			child, next := parseICSComponent(lines, i)
			if child == nil {
				return nil, i
			}
			comp.children = append(comp.children, child)
			i = next
		case line == "END:"+comp.name:
			return comp, i + 1
		default:
			if !strings.HasPrefix(line, "PRODID:") {
				comp.props = append(comp.props, line)
			}
			i++
		}
	}
	return comp, i
}

func (c *icsComponent) canonical() string {
	props := append([]string(nil), c.props...)
	sort.Strings(props)
	kids := make([]string, len(c.children))
	for i, child := range c.children {
		kids[i] = child.canonical()
	}
	sort.Strings(kids)

	var b strings.Builder
	b.WriteString("BEGIN:" + c.name + "\n")
	for _, p := range props {
		b.WriteString(p + "\n")
	}
	for _, k := range kids {
		b.WriteString(k)
	}
	b.WriteString("END:" + c.name + "\n")
	return b.String()
}
