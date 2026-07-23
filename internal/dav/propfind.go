package dav

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"time"

	davxml "calserver/internal/dav/xml"
)

// PropFind tracks requested properties and their resolved values for one
// node. Port of sabre's PropFind. Handlers claim properties they can produce;
// unclaimed ones report 404 in the multistatus (except for allprop requests,
// which omit them).
type PropFind struct {
	path     string
	depth    int
	allProps bool
	// order preserves output ordering: requested order, then extras in the
	// order they were set.
	order  []xml.Name
	result map[xml.Name]propEntry
}

type propEntry struct {
	status int
	value  any
}

// allPropDefaults is the property set an allprop request expands to,
// matching sabre (RFC 4918 allows omitting expensive properties).
var allPropDefaults = []xml.Name{
	{Space: davxml.NSDav, Local: "getlastmodified"},
	{Space: davxml.NSDav, Local: "getcontentlength"},
	{Space: davxml.NSDav, Local: "resourcetype"},
	{Space: davxml.NSDav, Local: "quota-used-bytes"},
	{Space: davxml.NSDav, Local: "quota-available-bytes"},
	{Space: davxml.NSDav, Local: "getetag"},
	{Space: davxml.NSDav, Local: "getcontenttype"},
}

// NewPropFind creates a PropFind for one node. Empty names means allprop
// (sabre treats propname the same way).
func NewPropFind(path string, names []xml.Name, depth int) *PropFind {
	pf := &PropFind{
		path:     strings.Trim(path, "/"),
		depth:    depth,
		allProps: len(names) == 0,
		result:   map[xml.Name]propEntry{},
	}
	if pf.allProps {
		names = allPropDefaults
	}
	for _, n := range names {
		if _, dup := pf.result[n]; !dup {
			pf.order = append(pf.order, n)
			pf.result[n] = propEntry{status: 404}
		}
	}
	return pf
}

// Path returns the node path this PropFind is for.
func (pf *PropFind) Path() string { return pf.path }

// Depth returns the request depth.
func (pf *PropFind) Depth() int { return pf.depth }

// IsAllProps reports whether this is an allprop request.
func (pf *PropFind) IsAllProps() bool { return pf.allProps }

// clone returns a fresh PropFind for another node of the same request.
func (pf *PropFind) clone(path string) *PropFind {
	names := make([]xml.Name, len(pf.order))
	copy(names, pf.order)
	c := NewPropFind(path, names, pf.depth)
	c.allProps = pf.allProps
	return c
}

// Handle resolves a still-missing property with fn's non-nil result.
func (pf *PropFind) Handle(name xml.Name, fn func() any) {
	if e, requested := pf.result[name]; requested && e.status == 404 {
		if v := fn(); v != nil {
			pf.result[name] = propEntry{status: 200, value: v}
		}
	}
}

// Set forces a property value and status. Unrequested properties are only
// admitted for allprop requests, like sabre.
func (pf *PropFind) Set(name xml.Name, value any, status int) {
	if _, requested := pf.result[name]; !requested {
		if !pf.allProps {
			return
		}
		pf.order = append(pf.order, name)
	}
	pf.result[name] = propEntry{status: status, value: value}
}

// Get returns the currently resolved value of a property, or nil.
func (pf *PropFind) Get(name xml.Name) any {
	return pf.result[name].value
}

// Props404 lists requested properties nothing has resolved yet.
func (pf *PropFind) Props404() []xml.Name {
	var out []xml.Name
	for _, n := range pf.order {
		if pf.result[n].status == 404 {
			out = append(out, n)
		}
	}
	return out
}

// response converts the result into a multistatus response element for the
// given href. Statuses are grouped 200 first, then 404 (dropped for allprop),
// then others ascending — matching sabre's getResultForMultiStatus.
func (pf *PropFind) response(href string) davxml.Response {
	byStatus := map[int][]davxml.Property{}
	var statuses []int
	add := func(status int, p davxml.Property) {
		if _, seen := byStatus[status]; !seen {
			statuses = append(statuses, status)
		}
		byStatus[status] = append(byStatus[status], p)
	}
	for _, n := range pf.order {
		e := pf.result[n]
		if e.status == 404 && pf.allProps {
			continue
		}
		add(e.status, davxml.Property{Name: n, Value: toXMLValue(e.value)})
	}

	// 200 first, then 404, then the rest ascending.
	rank := func(s int) int {
		switch s {
		case 200:
			return 0
		case 404:
			return 1
		default:
			return 2 + s
		}
	}
	for i := 1; i < len(statuses); i++ {
		for j := i; j > 0 && rank(statuses[j]) < rank(statuses[j-1]); j-- {
			statuses[j], statuses[j-1] = statuses[j-1], statuses[j]
		}
	}

	resp := davxml.Response{Href: href}
	for _, s := range statuses {
		resp.Propstats = append(resp.Propstats, davxml.Propstat{Status: s, Props: byStatus[s]})
	}
	if len(resp.Propstats) == 0 {
		// A response element requires at least one propstat.
		resp.Propstats = []davxml.Propstat{{Status: http.StatusOK}}
	}
	return resp
}

// toXMLValue converts handler results to serializable property content.
func toXMLValue(v any) davxml.Value {
	switch val := v.(type) {
	case nil:
		return nil
	case davxml.Value:
		return val
	case string:
		return davxml.Text(val)
	case []byte:
		return davxml.Raw(val)
	case int:
		return davxml.Text(strconv.Itoa(val))
	case int64:
		return davxml.Text(strconv.FormatInt(val, 10))
	case time.Time:
		return davxml.Text(val.UTC().Format(http.TimeFormat))
	case []xml.Name:
		return davxml.NameSet(val)
	default:
		return nil
	}
}
