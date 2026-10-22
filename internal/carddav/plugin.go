package carddav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	vcard "github.com/emersion/go-vcard"

	"calserver/internal/acl"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
	"calserver/internal/pathutil"
)

// NSCardDAV is the CardDAV namespace.
const NSCardDAV = "urn:ietf:params:xml:ns:carddav"

var (
	nameAddressBook = xml.Name{Space: NSCardDAV, Local: "addressbook"}
	nameAddressData = xml.Name{Space: NSCardDAV, Local: "address-data"}
	nameQuery       = xml.Name{Space: NSCardDAV, Local: "addressbook-query"}
	nameMultiget    = xml.Name{Space: NSCardDAV, Local: "addressbook-multiget"}
)

// Plugin implements the CardDAV protocol layer. Port of sabre's
// CardDAV\Plugin.
type Plugin struct {
	server *dav.Server
}

func (p *Plugin) PluginName() string { return "carddav" }

func (p *Plugin) Init(s *dav.Server) {
	p.server = s
	s.AddResourceTypeMapping(func(n dav.Node) []xml.Name {
		if _, ok := n.(*AddressBook); ok {
			return []xml.Name{nameAddressBook}
		}
		return nil
	})
}

func (p *Plugin) Features() []string { return []string{"addressbook"} }

// PropFind provides the CardDAV properties.
func (p *Plugin) PropFind(pf *dav.PropFind, node dav.Node) error {
	card := func(local string) xml.Name { return xml.Name{Space: NSCardDAV, Local: local} }

	if ph, isPrincipal := node.(acl.PrincipalHolder); isPrincipal {
		pf.Handle(card("addressbook-home-set"), func() any {
			_, name := pathutil.Split(ph.PrincipalURL())
			return davxml.Href{p.server.BaseURI + "addressbooks/" + name + "/"}
		})
		pf.Handle(card("principal-address"), func() any { return nil })
	}
	if _, isBook := node.(*AddressBook); isBook {
		pf.Handle(card("supported-address-data"), func() any {
			return davxml.Raw(`<card:address-data-type content-type="text/vcard" version="3.0"/>` +
				`<card:address-data-type content-type="text/vcard" version="4.0"/>`)
		})
		pf.Handle(card("supported-collation-set"), func() any {
			return davxml.Raw(`<card:supported-collation>i;ascii-casemap</card:supported-collation>` +
				`<card:supported-collation>i;octet</card:supported-collation>` +
				`<card:supported-collation>i;unicode-casemap</card:supported-collation>`)
		})
		pf.Handle(card("max-resource-size"), func() any { return MaxCardSize })
	}
	if c, isCard := node.(*Card); isCard {
		pf.Handle(nameAddressData, func() any {
			return string(c.Data())
		})
	}
	return nil
}

// InterceptWrite validates vCard bodies on PUT/create with ValidateCard, the
// check the contacts API applies too: a body that is not exactly one vCard
// with a UID is rejected with 415 rather than stored raw, and one over
// max-resource-size with 403. Only writes whose parent is an address book are
// inspected; anything else is left to other handlers.
func (p *Plugin) InterceptWrite(r *http.Request, path string, data []byte, isNew bool) ([]byte, error) {
	parentPath, _ := pathutil.Split(path)
	parent, err := p.server.Tree.NodeForPath(parentPath)
	if err != nil {
		return data, nil // not an address-book object write
	}
	if _, isBook := parent.(*AddressBook); !isBook {
		return data, nil
	}
	_, err = ValidateCard(data)
	switch {
	case errors.Is(err, ErrCardTooLarge):
		e := dav.ErrForbidden("The vCard exceeds the maximum resource size of %d bytes", MaxCardSize)
		e.Precondition = []xml.Name{{Space: NSCardDAV, Local: "max-resource-size"}}
		return nil, e
	case err != nil:
		return nil, dav.ErrUnsupportedMediaType(
			"This resource only supports valid vCard or jCard data. Parse error: %s", err)
	}
	return data, nil
}

// SupportedReports advertises the CardDAV reports.
func (p *Plugin) SupportedReports(node dav.Node, path string) []xml.Name {
	switch node.(type) {
	case *AddressBook, *Card:
		return []xml.Name{nameMultiget, nameQuery}
	}
	return nil
}

// Report implements dav.ReportHandler.
func (p *Plugin) Report(name xml.Name, body []byte, path string, w http.ResponseWriter, r *http.Request) (bool, error) {
	switch name {
	case nameQuery:
		return true, p.addressbookQueryReport(body, path, w, r)
	case nameMultiget:
		return true, p.multigetReport(body, w, r)
	}
	return false, nil
}

// HandleMethod serves the ?export VCF download (GET on an address book).
func (p *Plugin) Methods(path string) []string { return nil }

func (p *Plugin) HandleMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	if r.Method != "GET" || !r.URL.Query().Has("export") {
		return false, nil
	}
	path, err := p.server.Path(r)
	if err != nil {
		return false, nil
	}
	node, err := p.server.Tree.NodeForPath(path)
	if err != nil {
		return false, nil
	}
	book, isBook := node.(*AddressBook)
	if !isBook {
		return false, nil
	}
	cards, err := book.backend.Cards(book.Info.ID)
	if err != nil {
		return true, err
	}
	var out bytes.Buffer
	for _, c := range cards {
		out.Write(normalizeVCard(c.CardData))
		if !bytes.HasSuffix(c.CardData, []byte("\n")) {
			out.WriteString("\r\n")
		}
	}
	filename := exportFilenameRe.ReplaceAllString(book.Name(), "") + "-" + time.Now().Format("2006-01-02") + ".vcf"
	h := w.Header()
	h.Set("Content-Type", "text/directory")
	h.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(out.Bytes())
	return true, err
}

var exportFilenameRe = regexp.MustCompile(`[^a-zA-Z0-9-_ ]`)

// normalizeVCard ensures CRLF endings.
func normalizeVCard(data []byte) []byte {
	s := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n", "\r\n")
	return []byte(s)
}

func (p *Plugin) addressbookQueryReport(body []byte, path string, w http.ResponseWriter, r *http.Request) error {
	query, err := parseAddressbookQuery(body)
	if err != nil {
		return dav.ErrBadRequest("%s", err)
	}
	node, err := p.server.Tree.NodeForPath(path)
	if err != nil {
		return err
	}

	var matches []string
	switch n := node.(type) {
	case *AddressBook:
		cards, err := n.Children()
		if err != nil {
			return err
		}
		for _, child := range cards {
			c := child.(*Card)
			ok, err := query.match(c.Data())
			if err != nil {
				return err
			}
			if ok {
				matches = append(matches, path+"/"+c.Name())
				if query.limit > 0 && len(matches) >= query.limit {
					break
				}
			}
		}
	case *Card:
		ok, err := query.match(n.Data())
		if err != nil {
			return err
		}
		if ok {
			matches = append(matches, path)
		}
	}
	return p.writeCardResponses(r.Context(), w, matches, query.props, query.addrData)
}

func (p *Plugin) multigetReport(body []byte, w http.ResponseWriter, r *http.Request) error {
	query, err := parseAddressbookMultiget(body)
	if err != nil {
		return dav.ErrBadRequest("%s", err)
	}
	var paths []string
	for _, href := range query.hrefs {
		// Normalize each href through the same base-stripping logic as the
		// request path (gopath.Clean + base-prefix enforcement + percent-
		// decoding) rather than a bare TrimPrefix, so encoded and absolute-URL
		// hrefs resolve and out-of-mount hrefs are rejected.
		path, err := p.server.ResolveHref(href)
		if err != nil {
			continue
		}
		paths = append(paths, path)
	}
	return p.writeCardResponses(r.Context(), w, paths, query.props, query.addrData)
}

// convertResponseAddressData rewrites the {CARDDAV:}address-data property of a
// response with its version-converted / property-filtered form, mirroring
// sabre's post-processing of getPropertiesForPath output.
func convertResponseAddressData(resp *davxml.Response, spec *addressDataSpec) error {
	for pi := range resp.Propstats {
		ps := &resp.Propstats[pi]
		if ps.Status != http.StatusOK {
			continue
		}
		for i, prop := range ps.Props {
			if prop.Name != nameAddressData {
				continue
			}
			text, isText := prop.Value.(davxml.Text)
			if !isText {
				continue
			}
			converted, err := convertCardData([]byte(text), spec)
			if err != nil {
				return dav.ErrBadRequest("The vCard object could not be converted: %s", err)
			}
			ps.Props[i].Value = davxml.Text(converted)
		}
	}
	return nil
}

func (p *Plugin) writeCardResponses(ctx context.Context, w http.ResponseWriter, paths []string, props []xml.Name, spec *addressDataSpec) error {
	ms := &davxml.MultiStatus{}
	for _, cardPath := range paths {
		responses, err := p.server.PropertiesForPath(ctx, cardPath, props, 0)
		if err != nil {
			if dav.AsError(err).Status == http.StatusNotFound {
				ms.Responses = append(ms.Responses, davxml.Response{
					Href:   p.server.BaseURI + cardPath,
					Status: http.StatusNotFound,
				})
				continue
			}
			return err
		}
		for _, resp := range responses {
			if spec != nil {
				if err := convertResponseAddressData(&resp, spec); err != nil {
					return err
				}
			}
			ms.Responses = append(ms.Responses, resp)
		}
	}
	h := w.Header()
	h.Set("Content-Type", "application/xml; charset=utf-8")
	h.Set("Vary", "Brief,Prefer")
	out, err := ms.Marshal()
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusMultiStatus)
	_, err = w.Write(out)
	return err
}

// decodeVCard parses vCard bytes.
func decodeVCard(data []byte) (vcard.Card, error) {
	return vcard.NewDecoder(bytes.NewReader(data)).Decode()
}
