// Package davxml implements the WebDAV XML layer: clark-notation names,
// request parsing, multistatus serialization, and the canonicalizer used by
// corpus comparison. Port of the sabre/xml usage in sabre/dav.
package davxml

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// Well-known namespaces, matching sabre's default namespaceMap.
const (
	NSDav            = "DAV:"
	NSCalDAV         = "urn:ietf:params:xml:ns:caldav"
	NSCardDAV        = "urn:ietf:params:xml:ns:carddav"
	NSCalendarServer = "http://calendarserver.org/ns/"
	NSSabreDAV       = "http://sabredav.org/ns"
)

// Clark formats a name in clark notation: {DAV:}response.
func Clark(n xml.Name) string {
	return "{" + n.Space + "}" + n.Local
}

// ParseClark parses clark notation into an xml.Name.
func ParseClark(s string) (xml.Name, error) {
	if !strings.HasPrefix(s, "{") {
		return xml.Name{}, fmt.Errorf("not a clark-notation name: %q", s)
	}
	end := strings.Index(s, "}")
	if end < 0 || end == len(s)-1 {
		return xml.Name{}, fmt.Errorf("not a clark-notation name: %q", s)
	}
	return xml.Name{Space: s[1:end], Local: s[end+1:]}, nil
}
