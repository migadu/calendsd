package carddav

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"

	vcard "github.com/emersion/go-vcard"
)

// addressbookQuery is a parsed addressbook-query REPORT. Port of the filter
// subset of sabre's Plugin::validateFilters.
type addressbookQuery struct {
	props    []xml.Name
	test     string // anyof | allof
	filters  []propFilter
	limit    int              // <card:limit><card:nresults>; 0 means no limit
	addrData *addressDataSpec // <card:address-data>; nil when not requested
}

// addressDataSpec captures the requested address-data transformation from a
// REPORT: a target vCard version and, for addressbook-query, an optional
// property whitelist. Port of sabre's address-data content-type/version/prop
// handling.
type addressDataSpec struct {
	version string   // "3.0" or "4.0"; sabre defaults to 3.0
	props   []string // property whitelist; nil => whole card
}

type addressDataXML struct {
	Version string `xml:"version,attr"`
	Props   []struct {
		Name string `xml:"name,attr"`
	} `xml:"urn:ietf:params:xml:ns:carddav prop"`
}

// parseAddressData builds the spec from a parsed <address-data> element.
// withProps is false for addressbook-multiget, which — matching sabre — only
// negotiates the version and ignores any nested <prop> filters.
func parseAddressData(in *addressDataXML, withProps bool) *addressDataSpec {
	if in == nil {
		return nil
	}
	spec := &addressDataSpec{version: "3.0"}
	if in.Version != "" {
		spec.version = in.Version
	}
	if withProps {
		for _, p := range in.Props {
			if p.Name != "" {
				spec.props = append(spec.props, p.Name)
			}
		}
	}
	return spec
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
		AddressData *addressDataXML `xml:"urn:ietf:params:xml:ns:carddav address-data"`
	} `xml:"DAV: prop"`
	Filter *filterXML `xml:"filter"`
	Limit  *struct {
		NResults int `xml:"urn:ietf:params:xml:ns:carddav nresults"`
	} `xml:"urn:ietf:params:xml:ns:carddav limit"`
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
		// The typed AddressData field consumes the <address-data> element, so
		// re-add it to the requested-property list to keep the node emitting
		// the card body that convertCardData then transforms.
		if q.Prop.AddressData != nil {
			out.props = append(out.props, nameAddressData)
			out.addrData = parseAddressData(q.Prop.AddressData, true)
		}
	}
	if q.Filter != nil {
		if q.Filter.Test != "" {
			out.test = q.Filter.Test
		}
		for _, pf := range q.Filter.PropFilters {
			pfilter, err := convertPropFilter(pf)
			if err != nil {
				return nil, err
			}
			out.filters = append(out.filters, pfilter)
		}
	}
	if q.Limit != nil && q.Limit.NResults > 0 {
		out.limit = q.Limit.NResults
	}
	return out, nil
}

func convertPropFilter(in propFilterXML) (propFilter, error) {
	out := propFilter{name: in.Name, isNotDefined: in.IsNotDefined != nil, test: in.Test}
	if out.test == "" {
		out.test = "anyof"
	}
	for _, tm := range in.TextMatches {
		conv, err := convertTextMatch(tm)
		if err != nil {
			return propFilter{}, err
		}
		out.textMatches = append(out.textMatches, conv)
	}
	for _, pf := range in.ParamFilters {
		pfilter := paramFilter{name: pf.Name, isNotDefined: pf.IsNotDefined != nil}
		if pf.TextMatch != nil {
			tm, err := convertTextMatch(*pf.TextMatch)
			if err != nil {
				return propFilter{}, err
			}
			pfilter.textMatch = &tm
		}
		out.paramFilters = append(out.paramFilters, pfilter)
	}
	return out, nil
}

// convertTextMatch mirrors sabre's parse-time validation: an unknown match-type
// or collation is a 400 rather than being silently treated as contains /
// case-insensitive. (Sabre rejects match-type in the XML deserializer and the
// collation in StringUtil::textMatch; both surface as BadRequest.)
func convertTextMatch(in textMatchXML) (textMatch, error) {
	matchType := in.MatchType
	if matchType == "" {
		matchType = "contains"
	}
	switch matchType {
	case "contains", "equals", "starts-with", "ends-with":
	default:
		return textMatch{}, fmt.Errorf("unknown match-type: %s", matchType)
	}
	collation := in.Collation
	if collation == "" {
		collation = "i;unicode-casemap"
	}
	switch collation {
	case "i;octet", "i;ascii-casemap", "i;unicode-casemap":
	default:
		return textMatch{}, fmt.Errorf("collation type: %s is not supported", collation)
	}
	return textMatch{
		collation: collation, matchType: matchType,
		negate: in.Negate == "yes", value: in.Value,
	}, nil
}

// multigetQuery is a parsed addressbook-multiget REPORT.
type multigetQuery struct {
	props    []xml.Name
	hrefs    []string
	addrData *addressDataSpec
}

type multigetXML struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:carddav addressbook-multiget"`
	Prop    *struct {
		Names []struct {
			XMLName xml.Name
		} `xml:",any"`
		AddressData *addressDataXML `xml:"urn:ietf:params:xml:ns:carddav address-data"`
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
		// Multiget negotiates only the version, never a property filter
		// (sabre passes no propertiesFilter to convertVCard here).
		if q.Prop.AddressData != nil {
			out.props = append(out.props, nameAddressData)
			out.addrData = parseAddressData(q.Prop.AddressData, false)
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
	h, needle := foldCollation(haystack, tm.collation), foldCollation(tm.value, tm.collation)
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

// foldCollation case-folds s per the RFC 4790 collation, matching sabre's
// StringUtil::textMatch: i;ascii-casemap uppercases ASCII a-z only (non-ASCII
// bytes untouched), i;unicode-casemap uppercases with full Unicode semantics,
// and i;octet is byte-exact.
func foldCollation(s, collation string) string {
	switch collation {
	case "i;octet":
		return s
	case "i;ascii-casemap":
		return asciiUpper(s)
	default: // i;unicode-casemap
		return strings.ToUpper(s)
	}
}

func asciiUpper(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r - ('a' - 'A')
		}
		return r
	}, s)
}

// forcedCardProps are the properties sabre always keeps when a property filter
// is applied (convertVCard), regardless of the requested set.
var forcedCardProps = []string{"UID", "VERSION", "FN"}

// convertCardData applies sabre's convertVCard to a stored vCard: it optionally
// restricts the card to a whitelist of properties (always keeping UID/VERSION/FN)
// and converts it to the requested version. When neither a property filter nor a
// version change is needed, the original bytes are returned untouched — matching
// sabre's raw passthrough for the common case.
//
// Note on downgrade: go-vcard has no native 4.0->3.0 conversion, so that
// direction is a best-effort remap (toVCard3); 3.0->4.0 uses go-vcard's ToV4.
func convertCardData(data []byte, spec *addressDataSpec) ([]byte, error) {
	card, err := decodeVCard(normalizeVCard(data))
	if err != nil {
		return data, nil // unparseable stored data is returned untouched
	}
	stored := majorVCardVersion(card.Value(vcard.FieldVersion))
	target := majorVCardVersion(spec.version)

	filtered := false
	if len(spec.props) > 0 {
		keep := map[string]bool{}
		for _, p := range append(append([]string{}, forcedCardProps...), spec.props...) {
			keep[strings.ToUpper(p)] = true
		}
		for k := range card {
			if !keep[strings.ToUpper(k)] {
				delete(card, k)
			}
		}
		filtered = true
	}

	switch target {
	case "4":
		if stored == "4" {
			if !filtered {
				return data, nil
			}
		} else {
			vcard.ToV4(card)
		}
	default: // "3" (sabre's default target)
		if stored == "3" {
			if !filtered {
				return data, nil
			}
		} else {
			toVCard3(card)
		}
	}

	var buf bytes.Buffer
	if err := vcard.NewEncoder(&buf).Encode(card); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// majorVCardVersion returns the leading version digit ("3" or "4"), defaulting
// to "3" when the version is missing or unrecognized (sabre treats an absent
// version as vCard 3).
func majorVCardVersion(v string) string {
	if strings.HasPrefix(v, "4") {
		return "4"
	}
	return "3"
}

// toVCard3 is a best-effort vCard 4.0 -> 3.0 downgrade. go-vcard ships no native
// downgrade, so this stamps the version and remaps KIND to the
// X-ADDRESSBOOKSERVER-KIND property that sabre and Apple clients expect on 3.0;
// other 4.0-only constructs are left as-is.
func toVCard3(card vcard.Card) {
	card.SetValue(vcard.FieldVersion, "3.0")
	if kind := card.Value("KIND"); kind != "" {
		delete(card, "KIND")
		card.SetValue("X-ADDRESSBOOKSERVER-KIND", kind)
	}
}
