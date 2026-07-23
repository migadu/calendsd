package davxml

import (
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Canonicalize reduces an XML document to a deterministic textual form for
// semantic comparison (PLAN.md §7): namespace *prefixes* are erased (every
// name renders in clark notation), xmlns declarations are dropped, attributes
// are sorted, whitespace-only text is removed, and unordered element sets are
// sorted — {DAV:}response siblings (by href) and {DAV:}prop children (by
// name), which RFC 4918 defines as order-insignificant.
//
// Two documents are semantically equal iff their canonical forms are equal;
// diffs of canonical forms are line-oriented and readable in test output.
func Canonicalize(r io.Reader) (string, error) {
	return CanonicalizeFunc(r, nil)
}

// CanonicalizeFunc canonicalizes with an optional text-node transformer,
// letting callers normalize embedded payloads (e.g. iCalendar inside
// calendar-data) before comparison.
func CanonicalizeFunc(r io.Reader, textTransform func(string) string) (string, error) {
	root, err := parseTree(r)
	if err != nil {
		return "", err
	}
	root.normalize()
	if textTransform != nil {
		root.transformText(textTransform)
	}
	var b strings.Builder
	root.write(&b, 0)
	return b.String(), nil
}

func (n *xnode) transformText(fn func(string) string) {
	if n.text != "" {
		n.text = fn(n.text)
	}
	for _, c := range n.children {
		c.transformText(fn)
	}
}

type xnode struct {
	name     xml.Name
	attrs    []xml.Attr
	text     string
	children []*xnode
}

func parseTree(r io.Reader) (*xnode, error) {
	dec := xml.NewDecoder(r)
	var root *xnode
	var stack []*xnode
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &xnode{name: t.Name}
			for _, a := range t.Attr {
				// Drop namespace declarations; the decoder has already
				// resolved prefixes into Name.Space.
				if a.Name.Space == "xmlns" || (a.Name.Space == "" && a.Name.Local == "xmlns") {
					continue
				}
				n.attrs = append(n.attrs, a)
			}
			if len(stack) == 0 {
				if root != nil {
					return nil, fmt.Errorf("multiple root elements")
				}
				root = n
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text += string(t)
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("no root element")
	}
	return root, nil
}

func (n *xnode) normalize() {
	// Mixed content does not occur in WebDAV bodies; whitespace around child
	// elements is formatting noise.
	if len(n.children) > 0 || strings.TrimSpace(n.text) == "" {
		n.text = ""
	} else {
		n.text = strings.TrimSpace(n.text)
	}
	sort.Slice(n.attrs, func(i, j int) bool {
		a, b := n.attrs[i].Name, n.attrs[j].Name
		if a.Space != b.Space {
			return a.Space < b.Space
		}
		return a.Local < b.Local
	})
	for _, c := range n.children {
		c.normalize()
	}
	n.sortUnorderedSets()
}

func (n *xnode) sortUnorderedSets() {
	switch {
	case n.name == xml.Name{Space: NSDav, Local: "multistatus"}:
		// Responses are an unordered set; key by first href.
		sort.SliceStable(n.children, func(i, j int) bool {
			return n.children[i].sortKey() < n.children[j].sortKey()
		})
	case n.name == xml.Name{Space: NSDav, Local: "prop"}:
		sort.SliceStable(n.children, func(i, j int) bool {
			return Clark(n.children[i].name) < Clark(n.children[j].name)
		})
	}
}

// sortKey orders {DAV:}response elements: the href text, falling back to the
// element name for non-response children like sync-token.
func (n *xnode) sortKey() string {
	if (n.name == xml.Name{Space: NSDav, Local: "response"}) {
		for _, c := range n.children {
			if (c.name == xml.Name{Space: NSDav, Local: "href"}) {
				return "response\x00" + c.text
			}
		}
	}
	return Clark(n.name)
}

func (n *xnode) write(b *strings.Builder, depth int) {
	indent := strings.Repeat(" ", depth)
	b.WriteString(indent)
	b.WriteString(Clark(n.name))
	for _, a := range n.attrs {
		fmt.Fprintf(b, " @%s=%q", Clark(a.Name), a.Value)
	}
	if n.text != "" {
		fmt.Fprintf(b, " %q", n.text)
	}
	b.WriteString("\n")
	for _, c := range n.children {
		c.write(b, depth+1)
	}
}
