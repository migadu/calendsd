package carddav

import (
	"bytes"
	"encoding/xml"
	"strings"

	vcard "github.com/emersion/go-vcard"
)

// addressbookQuery is a parsed addressbook-query REPORT. Port of the filter
// subset of sabre's Plugin::validateFilters.
type addressbookQuery struct {
	props   []xml.Name
	test    string // anyof | allof
	filters []propFilter
}

type propFilter struct {
	name         string
	isNotDefined bool
	test         string
	textMatches  []textMatch
	paramFilters []paramFilter
}

type paramFilter struct {
	name         string
	isNotDefined bool
	textMatch    *textMatch
}

type textMatch struct {
	collation string
	matchType string // contains | equals | starts-with | ends-with
	negate    bool
	value     string
}

type queryXML struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:carddav addressbook-query"`
	Prop    *struct {
		Names []struct {
			XMLName xml.Name
		} `xml:",any"`
	} `xml:"DAV: prop"`
	Filter *filterXML `xml:"filter"`
}

type filterXML struct {
	Test        string          `xml:"test,attr"`
	PropFilters []propFilterXML `xml:"prop-filter"`
}

type propFilterXML struct {
	Name         string           `xml:"name,attr"`
	Test         string           `xml:"test,attr"`
	IsNotDefined *struct{}        `xml:"is-not-defined"`
	TextMatches  []textMatchXML   `xml:"text-match"`
	ParamFilters []paramFilterXML `xml:"param-filter"`
}

type paramFilterXML struct {
	Name         string        `xml:"name,attr"`
	IsNotDefined *struct{}     `xml:"is-not-defined"`
	TextMatch    *textMatchXML `xml:"text-match"`
}

type textMatchXML struct {
	Collation string `xml:"collation,attr"`
	MatchType string `xml:"match-type,attr"`
	Negate    string `xml:"negate-condition,attr"`
	Value     string `xml:",chardata"`
}

func parseAddressbookQuery(body []byte) (*addressbookQuery, error) {
	var q queryXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&q); err != nil {
		return nil, err
	}
	out := &addressbookQuery{test: "anyof"}
	if q.Prop != nil {
		for _, n := range q.Prop.Names {
			out.props = append(out.props, n.XMLName)
		}
	}
	if q.Filter != nil {
		if q.Filter.Test != "" {
			out.test = q.Filter.Test
		}
		for _, pf := range q.Filter.PropFilters {
			out.filters = append(out.filters, convertPropFilter(pf))
		}
	}
	return out, nil
}

func convertPropFilter(in propFilterXML) propFilter {
	out := propFilter{name: in.Name, isNotDefined: in.IsNotDefined != nil, test: in.Test}
	if out.test == "" {
		out.test = "anyof"
	}
	for _, tm := range in.TextMatches {
		out.textMatches = append(out.textMatches, convertTextMatch(tm))
	}
	for _, pf := range in.ParamFilters {
		pfilter := paramFilter{name: pf.Name, isNotDefined: pf.IsNotDefined != nil}
		if pf.TextMatch != nil {
			tm := convertTextMatch(*pf.TextMatch)
			pfilter.textMatch = &tm
		}
		out.paramFilters = append(out.paramFilters, pfilter)
	}
	return out
}

func convertTextMatch(in textMatchXML) textMatch {
	matchType := in.MatchType
	if matchType == "" {
		matchType = "contains"
	}
	collation := in.Collation
	if collation == "" {
		collation = "i;unicode-casemap"
	}
	return textMatch{
		collation: collation, matchType: matchType,
		negate: in.Negate == "yes", value: in.Value,
	}
}

// multigetQuery is a parsed addressbook-multiget REPORT.
type multigetQuery struct {
	props []xml.Name
	hrefs []string
}

type multigetXML struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:carddav addressbook-multiget"`
	Prop    *struct {
		Names []struct {
			XMLName xml.Name
		} `xml:",any"`
	} `xml:"DAV: prop"`
	Hrefs []string `xml:"DAV: href"`
}

func parseAddressbookMultiget(body []byte) (*multigetQuery, error) {
	var q multigetXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&q); err != nil {
		return nil, err
	}
	out := &multigetQuery{hrefs: q.Hrefs}
	if q.Prop != nil {
		for _, n := range q.Prop.Names {
			out.props = append(out.props, n.XMLName)
		}
	}
	return out, nil
}

// match reports whether a vCard satisfies the query filters. Port of
// Plugin::validateFilters.
func (q *addressbookQuery) match(data []byte) (bool, error) {
	if len(q.filters) == 0 {
		return true, nil
	}
	card, err := decodeVCard(data)
	if err != nil {
		return false, nil // unparseable cards never match, like sabre
	}
	for _, filter := range q.filters {
		success := q.matchFilter(card, filter)
		if q.test == "anyof" && success {
			return true, nil
		}
		if q.test == "allof" && !success {
			return false, nil
		}
	}
	return q.test == "allof", nil
}

func (q *addressbookQuery) matchFilter(card vcard.Card, filter propFilter) bool {
	fields := card[strings.ToUpper(filter.name)]
	defined := len(fields) > 0

	if filter.isNotDefined {
		return !defined
	}
	if (len(filter.paramFilters) == 0 && len(filter.textMatches) == 0) || !defined {
		return defined
	}

	var results []bool
	if len(filter.paramFilters) > 0 {
		results = append(results, matchParamFilters(fields, filter.paramFilters, filter.test))
	}
	if len(filter.textMatches) > 0 {
		var texts []string
		for _, f := range fields {
			texts = append(texts, f.Value)
		}
		results = append(results, matchTextMatches(texts, filter.textMatches, filter.test))
	}
	if len(results) == 1 {
		return results[0]
	}
	if filter.test == "anyof" {
		return results[0] || results[1]
	}
	return results[0] && results[1]
}

func matchParamFilters(fields []*vcard.Field, filters []paramFilter, test string) bool {
	for _, pf := range filters {
		success := false
		for _, field := range fields {
			values := field.Params[strings.ToUpper(pf.name)]
			if pf.isNotDefined {
				success = len(values) == 0
			} else if pf.textMatch == nil {
				success = len(values) > 0
			} else {
				// Compute the aggregate "does any value match?" first, then
				// let negate flip the aggregate (sabre's validateTextMatches:
				// negate is applied after the haystack loop, not per value).
				anyMatch := false
				for _, v := range values {
					if pf.textMatch.matches(v) {
						anyMatch = true
						break
					}
				}
				success = pf.textMatch.negate != anyMatch
			}
			if success {
				break
			}
		}
		if test == "anyof" && success {
			return true
		}
		if test == "allof" && !success {
			return false
		}
	}
	return test == "allof"
}

func matchTextMatches(texts []string, matches []textMatch, test string) bool {
	for _, tm := range matches {
		// Compute the aggregate "does any instance match?" first, then let
		// negate flip the aggregate (sabre's validateTextMatches applies
		// negate after the haystack loop, not per instance).
		anyMatch := false
		for _, text := range texts {
			if tm.matches(text) {
				anyMatch = true
				break
			}
		}
		success := tm.negate != anyMatch
		if test == "anyof" && success {
			return true
		}
		if test == "allof" && !success {
			return false
		}
	}
	return test == "allof"
}

// matches applies a text-match's match-type to a single instance, WITHOUT
// negation: negate is a quantifier over "does any instance match?" and is
// applied by the callers after the instance loop (sabre's validateTextMatches).
// Both mandatory collations are case-insensitive; i;octet is case-sensitive.
func (tm textMatch) matches(haystack string) bool {
	h, needle := haystack, tm.value
	if tm.collation != "i;octet" {
		h, needle = strings.ToLower(h), strings.ToLower(needle)
	}
	switch tm.matchType {
	case "equals":
		return h == needle
	case "starts-with":
		return strings.HasPrefix(h, needle)
	case "ends-with":
		return strings.HasSuffix(h, needle)
	default: // contains
		return strings.Contains(h, needle)
	}
}
