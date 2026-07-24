package carddav

import (
	"bytes"
	"context"
	"encoding/xml"
	"net/http"
	"regexp"
	"strings"
	"time"

	vcard "github.com/emersion/go-vcard"

	"calserver/internal/acl"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
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
			_, name := splitPath(ph.PrincipalURL())
			return davxml.Href{p.server.BaseURI + "addressbooks/" + name + "/"}
		})
		pf.Handle(card("principal-address"), func() any { return nil })
	}
	if _, isBook := node.(*AddressBook); isBook {
		pf.Handle(card("supported-address-data"), func() any {
			return davxml.Raw(`<card:address-data-type content-type="text/vcard" version="3.0"/>` +
				`<card:address-data-type content-type="text/vcard" version="4.0"/>`)
		})
		pf.Handle(card("max-resource-size"), func() any { return 10000000 })
	}
	if c, isCard := node.(*Card); isCard {
		pf.Handle(nameAddressData, func() any {
			return string(c.Data())
		})
	}
	return nil
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
	return p.writeCardResponses(r.Context(), w, matches, query.props)
}

func (p *Plugin) multigetReport(body []byte, w http.ResponseWriter, r *http.Request) error {
	query, err := parseAddressbookMultiget(body)
	if err != nil {
		return dav.ErrBadRequest("%s", err)
	}
	var paths []string
	for _, href := range query.hrefs {
		paths = append(paths, strings.Trim(strings.TrimPrefix(href, p.server.BaseURI), "/"))
	}
	return p.writeCardResponses(r.Context(), w, paths, query.props)
}

func (p *Plugin) writeCardResponses(ctx context.Context, w http.ResponseWriter, paths []string, props []xml.Name) error {
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
		ms.Responses = append(ms.Responses, responses...)
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

func splitPath(path string) (parent, base string) {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return "", path
	}
	return path[:i], path[i+1:]
}

// decodeVCard parses vCard bytes.
func decodeVCard(data []byte) (vcard.Card, error) {
	return vcard.NewDecoder(bytes.NewReader(data)).Decode()
}
