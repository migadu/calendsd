package acl

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"slices"
	"strings"

	"calendsd/internal/dav"
	davxml "calendsd/internal/dav/xml"
	"calendsd/internal/pathutil"
)

// Plugin implements access control: principal properties, principal reports,
// and privilege enforcement. Port of sabre's DAVACL\Plugin.
type Plugin struct {
	server *dav.Server

	// AdminPrincipals get an implicit {DAV:}all ACE on every node,
	// mirroring sabre's adminPrincipals.
	AdminPrincipals []string
	// PrincipalsReadableByAuthenticated grants every authenticated user
	// {DAV:}read on principal resources (Calendar-Server behavior needed by
	// delegation clients). Off by default: stock sabre keeps principals
	// owner-only.
	PrincipalsReadableByAuthenticated bool
	// AllowUnauthenticatedAccess mirrors sabre's flag. When true, a
	// privilege check that fails for an unauthenticated request returns
	// 401 (prompting login) rather than 403, so anonymous clients get a
	// chance to authenticate. When false, the failure is a plain 403.
	AllowUnauthenticatedAccess bool
	// FreeBusyPolicy resolves the FreeBusyAudience pseudo-principal from
	// each owner's free-busy visibility. Nil treats every owner as the
	// default (their mail domain).
	FreeBusyPolicy FreeBusyPolicy
}

func (p *Plugin) PluginName() string { return "acl" }

func (p *Plugin) Init(s *dav.Server) {
	p.server = s
	s.AddResourceTypeMapping(func(n dav.Node) []xml.Name {
		if _, isPrincipal := n.(PrincipalHolder); isPrincipal {
			return []xml.Name{{Space: davxml.NSDav, Local: "principal"}}
		}
		return nil
	})
	s.AddResourceTypeMapping(func(n dav.Node) []xml.Name {
		if proxy, ok := n.(*ProxyPrincipalNode); ok {
			return []xml.Name{proxy.ProxyResourceType()}
		}
		return nil
	})
}

func (p *Plugin) Features() []string {
	return []string{"access-control", "calendarserver-principal-property-search"}
}

// CurrentUserPrincipals returns the current principal plus its group
// membership, mirroring sabre's getCurrentUserPrincipals.
func (p *Plugin) CurrentUserPrincipals(ctx context.Context) ([]string, error) {
	current := dav.Principal(ctx)
	if current == "" {
		return nil, nil
	}
	out := []string{current}
	node, err := p.server.Tree.NodeForPath(ctx, current)
	if err != nil {
		return out, nil // principal may not be in the tree
	}
	if ph, ok := node.(PrincipalHolder); ok {
		groups, err := ph.GroupMembership(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, groups...)
	}
	return out, nil
}

// ACLForNode returns the effective ACL of a node — the one privilege checks
// evaluate (see nodeACL) — for the admin simulator's ACL view.
func (p *Plugin) ACLForNode(node dav.Node) []ACE {
	return p.nodeACL(node)
}

// PropFind implements dav.PropFindHook: read-gating, principal properties,
// current-user-principal.
func (p *Plugin) PropFind(pf *dav.PropFind, node dav.Node) error {
	davName := func(local string) xml.Name { return xml.Name{Space: davxml.NSDav, Local: local} }

	// Without {DAV:}read every requested property reports 403, mirroring
	// sabre's propFind gating (hideNodesFromListings stays off). The gate
	// fails closed: an error computing privileges denies, never grants.
	held, err := p.CurrentUserPrivileges(pf.Context(), node)
	readable := err == nil && slices.Contains(held, "{DAV:}read")
	if !readable {
		// Latch the gate closed first so the later node-dead-property and
		// property-storage phases cannot re-add values at 200 (they run after
		// this hook and are otherwise blind to the read denial).
		pf.Deny()
		for _, name := range pf.Requested() {
			pf.Set(name, nil, http.StatusForbidden)
		}
		return nil
	}

	if ph, isPrincipal := node.(PrincipalHolder); isPrincipal {
		pf.Handle(davName("alternate-URI-set"), func() any {
			return davxml.Href(ph.AlternateURISet())
		})
		pf.Handle(davName("principal-URL"), func() any {
			return davxml.Href{p.server.Href(ph.PrincipalURL()) + "/"}
		})
		pf.Handle(davName("group-member-set"), func() any {
			return p.principalHrefs(pf.Context(), ph.GroupMemberSet)
		})
		pf.Handle(davName("group-membership"), func() any {
			return p.principalHrefs(pf.Context(), ph.GroupMembership)
		})
		// User principals advertise which principals' calendars they may
		// proxy (calendarserver delegation); proxy nodes themselves do not.
		if _, isProxy := node.(*ProxyPrincipalNode); !isProxy {
			cs := func(local string) xml.Name { return xml.Name{Space: davxml.NSCalendarServer, Local: local} }
			pf.Handle(cs("calendar-proxy-read-for"), func() any {
				return p.proxyFor(pf.Context(), ph, proxyReadName)
			})
			pf.Handle(cs("calendar-proxy-write-for"), func() any {
				return p.proxyFor(pf.Context(), ph, proxyWriteName)
			})
		}
	}

	pf.Handle(davName("current-user-principal"), func() any {
		if current := dav.Principal(pf.Context()); current != "" {
			return davxml.Href{p.server.Href(current) + "/"}
		}
		return davxml.Raw(`<d:unauthenticated/>`)
	})
	pf.Handle(davName("owner"), func() any {
		if owned, ok := node.(interface{ Owner() string }); ok {
			return davxml.Href{p.server.Href(owned.Owner()) + "/"}
		}
		return nil
	})
	pf.Handle(davName("current-user-privilege-set"), func() any {
		held, err := p.CurrentUserPrivileges(pf.Context(), node)
		if err != nil {
			return nil
		}
		set := davxml.PrivilegeSet{}
		for _, priv := range held {
			if name, err := davxml.ParseClark(priv); err == nil {
				set = append(set, name)
			}
		}
		return set
	})
	pf.Handle(davName("supported-privilege-set"), func() any {
		return davxml.SupportedPrivilegeSet{toSupportedPrivilege(supportedPrivileges(node))}
	})
	return nil
}

// Methods contributes no methods to the Allow header: sabre registers the ACL
// handler but does not advertise ACL in OPTIONS, so neither do we.
func (p *Plugin) Methods(ctx context.Context, path string) []string { return nil }

// HandleMethod implements the ACL method (RFC 3744 §8.1), mirroring sabre's
// DAVACL\Plugin::httpAcl.
func (p *Plugin) HandleMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	if r.Method != "ACL" {
		return false, nil
	}
	return true, p.httpACL(w, r)
}

func (p *Plugin) httpACL(w http.ResponseWriter, r *http.Request) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return dav.ErrBadRequest("XML body expected in ACL request")
	}
	// sabre's xml->expect('{DAV:}acl', body) rejects a body that is not a
	// well-formed {DAV:}acl document with a 400.
	var doc struct {
		XMLName xml.Name `xml:"DAV: acl"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil || doc.XMLName.Local != "acl" || doc.XMLName.Space != davxml.NSDav {
		return dav.ErrBadRequest("The body did not contain a valid {DAV:}acl element")
	}

	path, err := p.server.Path(r)
	if err != nil {
		return err
	}
	node, err := p.server.Tree.NodeForPath(r.Context(), path)
	if err != nil {
		return err // NodeForPath surfaces a 404 for a missing node
	}
	if _, isACL := node.(ACLHolder); !isACL {
		return dav.ErrMethodNotAllowed("This node does not support the ACL method")
	}
	// Every node in this server carries a fixed, owner-derived ACL and does not
	// implement setACL — exactly like sabre's ACLTrait, whose setACL throws
	// Forbidden. So a well-formed ACL request is recognized but cannot be
	// applied, yielding the same 403 stock sabre returns against this backend.
	return dav.ErrForbidden("Setting ACL is not supported on this node")
}

// toSupportedPrivilege converts an internal privilege tree into its davxml
// rendering form (clark names resolved to xml.Name).
func toSupportedPrivilege(p Privilege) davxml.SupportedPrivilege {
	name, _ := davxml.ParseClark(p.Name)
	sp := davxml.SupportedPrivilege{Name: name, Abstract: p.Abstract}
	for _, sub := range p.Aggregates {
		sp.Aggregates = append(sp.Aggregates, toSupportedPrivilege(sub))
	}
	return sp
}

// SupportedReports advertises the implemented principal reports.
func (p *Plugin) SupportedReports(node dav.Node, path string) []xml.Name {
	return []xml.Name{
		{Space: davxml.NSDav, Local: "principal-match"},
		{Space: davxml.NSDav, Local: "acl-principal-prop-set"},
		{Space: davxml.NSDav, Local: "principal-property-search"},
		{Space: davxml.NSDav, Local: "principal-search-property-set"},
		{Space: davxml.NSDav, Local: "expand-property"},
	}
}

// principalSearchProps are the properties this server allows searching over,
// with human descriptions (sabre's default $principalSearchPropertySet).
var principalSearchProps = []struct {
	name        xml.Name
	description string
}{
	{xml.Name{Space: davxml.NSDav, Local: "displayname"}, "Display name"},
	{xml.Name{Space: davxml.NSSabreDAV, Local: "email-address"}, "Email address"},
}

type principalPropertySearchXML struct {
	XMLName                       xml.Name  `xml:"DAV: principal-property-search"`
	ApplyToPrincipalCollectionSet *struct{} `xml:"apply-to-principal-collection-set"`
	Test                          string    `xml:"test,attr"`
	PropertySearch                []struct {
		Prop struct {
			Names []struct{ XMLName xml.Name } `xml:",any"`
		} `xml:"prop"`
		Match string `xml:"match"`
	} `xml:"property-search"`
	Prop *struct {
		Names []struct{ XMLName xml.Name } `xml:",any"`
	} `xml:"prop"`
}

// principalPropertySearchReport implements {DAV:}principal-property-search:
// the attendee/contact directory search clients use (RFC 3744 §9.4).
func (p *Plugin) principalPropertySearchReport(body []byte, path string, w http.ResponseWriter, r *http.Request) error {
	var req principalPropertySearchXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		return dav.ErrBadRequest("%s", err)
	}
	if d := r.Header.Get("Depth"); d != "" && d != "0" {
		return dav.ErrBadRequest("Depth must be 0")
	}
	test := "allof"
	if req.Test == "anyof" {
		test = "anyof"
	}
	search := map[xml.Name]string{}
	for _, ps := range req.PropertySearch {
		for _, n := range ps.Prop.Names {
			search[n.XMLName] = ps.Match
		}
	}
	var props []xml.Name
	if req.Prop != nil {
		for _, n := range req.Prop.Names {
			props = append(props, n.XMLName)
		}
	}

	// Search each principal collection (the request path, or all of them
	// when apply-to-principal-collection-set is set).
	prefixes := []string{strings.Trim(path, "/")}
	if req.ApplyToPrincipalCollectionSet != nil {
		prefixes = p.principalCollectionPrefixes()
	}
	seen := map[string]bool{}
	ms := &davxml.MultiStatus{}
	for _, prefix := range prefixes {
		matches, err := p.backendSearch(r.Context(), prefix, search, test)
		if err != nil {
			return err
		}
		// Scope directory results to the requester's own email domain, so the
		// attendee-search UI cannot enumerate users of other domains by
		// displayname/email substring (matches the domain scoping applied to
		// PROPFIND child listings and free-busy).
		matches = p.sameDomainPrincipals(r.Context(), matches)
		for _, principal := range matches {
			if seen[principal] {
				continue
			}
			seen[principal] = true
			responses, err := p.server.PropertiesForPath(r.Context(), principal, props, 0)
			if err != nil {
				return err
			}
			ms.Responses = append(ms.Responses, responses...)
		}
	}
	return p.server.WriteMultiStatus(w, ms)
}

// backendSearch runs a principal search against the collection at prefix.
func (p *Plugin) backendSearch(ctx context.Context, prefix string, search map[xml.Name]string, test string) ([]string, error) {
	node, err := p.server.Tree.NodeForPath(ctx, prefix)
	if err != nil {
		return nil, nil
	}
	searcher, ok := node.(PrincipalSearcher)
	if !ok {
		return nil, nil
	}
	return searcher.SearchPrincipals(ctx, search, test)
}

// principalCollectionPrefixes returns the known principal collection paths.
func (p *Plugin) principalCollectionPrefixes() []string {
	return []string{"principals"}
}

// principalSearchPropertySetXML is the principal-search-property-set report
// body (RFC 3744 §9.5).
type principalSearchPropertySetXML struct {
	XMLName    xml.Name                     `xml:"DAV: principal-search-property-set"`
	Properties []principalSearchPropertyXML `xml:"DAV: principal-search-property"`
}

type principalSearchPropertyXML struct {
	Prop struct {
		Name struct{ XMLName xml.Name }
	} `xml:"DAV: prop"`
	Description struct {
		Lang string `xml:"http://www.w3.org/XML/1998/namespace lang,attr"`
		Text string `xml:",chardata"`
	} `xml:"DAV: description"`
}

func (p *Plugin) principalSearchPropertySetReport(w http.ResponseWriter) error {
	var doc principalSearchPropertySetXML
	for _, sp := range principalSearchProps {
		var prop principalSearchPropertyXML
		prop.Prop.Name.XMLName = sp.name
		prop.Description.Lang = "en"
		prop.Description.Text = sp.description
		doc.Properties = append(doc.Properties, prop)
	}
	out, err := xml.MarshalIndent(doc, "", " ")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(append([]byte(xml.Header), append(out, '\n')...))
	return err
}

type expandPropertyXML struct {
	XMLName  xml.Name          `xml:"DAV: expand-property"`
	Property []expandPropEntry `xml:"property"`
}

type expandPropEntry struct {
	Name      string            `xml:"name,attr"`
	Namespace string            `xml:"namespace,attr"`
	Children  []expandPropEntry `xml:"property"`
}

// expandPropertyReport implements {DAV:}expand-property: href-valued
// properties are replaced inline with the child's expanded properties,
// saving round-trips (RFC 3253 §3.8).
func (p *Plugin) expandPropertyReport(body []byte, path string, w http.ResponseWriter, r *http.Request) error {
	var req expandPropertyXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		return dav.ErrBadRequest("%s", err)
	}
	depth := 0
	if d := r.Header.Get("Depth"); d == "1" {
		depth = 1
	}
	responses, err := p.expandProperties(r.Context(), path, req.Property, depth)
	if err != nil {
		return err
	}
	return p.server.WriteMultiStatus(w, &davxml.MultiStatus{Responses: responses})
}

func (p *Plugin) expandProperties(ctx context.Context, path string, requested []expandPropEntry, depth int) ([]davxml.Response, error) {
	var names []xml.Name
	childReq := map[xml.Name][]expandPropEntry{}
	for _, entry := range requested {
		n := entry.xmlName()
		names = append(names, n)
		if len(entry.Children) > 0 {
			childReq[n] = entry.Children
		}
	}
	responses, err := p.server.PropertiesForPath(ctx, path, names, depth)
	if err != nil {
		return nil, err
	}
	for ri := range responses {
		for pi := range responses[ri].Propstats {
			ps := &responses[ri].Propstats[pi]
			if ps.Status != http.StatusOK {
				continue
			}
			for i, prop := range ps.Props {
				children, expand := childReq[prop.Name]
				hrefs, isHref := prop.Value.(davxml.Href)
				if !expand || !isHref {
					continue
				}
				var nested []davxml.Response
				for _, href := range hrefs {
					childPath, err := p.server.ResolveHref(href)
					if err != nil {
						continue // not a resource on this server
					}
					sub, err := p.expandProperties(ctx, childPath, children, 0)
					if err != nil {
						return nil, err
					}
					nested = append(nested, sub...)
				}
				ps.Props[i].Value = davxml.ResponseList(nested)
			}
		}
	}
	return responses, nil
}

func (e expandPropEntry) xmlName() xml.Name {
	// sabre's ExpandPropertyReport defaults an absent namespace to DAV:.
	ns := e.Namespace
	if ns == "" {
		ns = davxml.NSDav
	}
	return xml.Name{Space: ns, Local: e.Name}
}

// PrincipalSearcher is implemented by principal collections that can search.
type PrincipalSearcher interface {
	SearchPrincipals(ctx context.Context, search map[xml.Name]string, test string) ([]string, error)
}

// proxyFor lists the user principals whose calendars this principal may act as
// a proxy for: the parents of the proxy groups it belongs to. Backs the
// calendarserver calendar-proxy-read-for / calendar-proxy-write-for properties.
func (p *Plugin) proxyFor(ctx context.Context, ph PrincipalHolder, proxySuffix string) any {
	groups, err := ph.GroupMembership(ctx)
	if err != nil {
		return nil
	}
	hrefs := davxml.Href{}
	for _, g := range groups {
		if parent, base := pathutil.Split(g); base == proxySuffix {
			hrefs = append(hrefs, p.server.Href(parent)+"/")
		}
	}
	return hrefs
}

func (p *Plugin) principalHrefs(ctx context.Context, fn func(context.Context) ([]string, error)) any {
	paths, err := fn(ctx)
	if err != nil {
		return nil
	}
	hrefs := make(davxml.Href, len(paths))
	for i, path := range paths {
		hrefs[i] = p.server.Href(path) + "/"
	}
	return hrefs
}

// Report implements dav.ReportHandler for the principal reports.
func (p *Plugin) Report(name xml.Name, body []byte, path string, w http.ResponseWriter, r *http.Request) (bool, error) {
	dav := func(local string) xml.Name { return xml.Name{Space: davxml.NSDav, Local: local} }
	switch name {
	case dav("principal-match"):
		return true, p.principalMatchReport(body, path, w, r)
	case dav("acl-principal-prop-set"):
		return true, p.aclPrincipalPropSetReport(body, path, w, r)
	case dav("principal-property-search"):
		return true, p.principalPropertySearchReport(body, path, w, r)
	case dav("principal-search-property-set"):
		return true, p.principalSearchPropertySetReport(w)
	case dav("expand-property"):
		return true, p.expandPropertyReport(body, path, w, r)
	}
	return false, nil
}

type principalMatchXML struct {
	XMLName           xml.Name  `xml:"DAV: principal-match"`
	Self              *struct{} `xml:"self"`
	PrincipalProperty *struct {
		Names []struct {
			XMLName xml.Name
		} `xml:",any"`
	} `xml:"principal-property"`
	Prop *struct {
		Names []struct {
			XMLName xml.Name
		} `xml:",any"`
	} `xml:"prop"`
}

func (p *Plugin) principalMatchReport(body []byte, path string, w http.ResponseWriter, r *http.Request) error {
	if d := r.Header.Get("Depth"); d != "" && d != "0" {
		return dav.ErrBadRequest("The principal-match report is only defined on Depth: 0")
	}
	var req principalMatchXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		return dav.ErrBadRequest("%s", err)
	}

	currentPrincipals, err := p.CurrentUserPrincipals(r.Context())
	if err != nil {
		return err
	}

	var matches []string
	if req.Self != nil {
		for _, principal := range currentPrincipals {
			if principal == path || strings.HasPrefix(principal, path+"/") {
				matches = append(matches, principal)
			}
		}
	} else if req.PrincipalProperty != nil && len(req.PrincipalProperty.Names) > 0 {
		propName := req.PrincipalProperty.Names[0].XMLName
		candidates, err := p.server.PropertiesForPath(r.Context(), path, []xml.Name{propName}, 1)
		if err != nil {
			return err
		}
		for _, cand := range candidates {
			candPath, err := p.server.ResolveHref(cand.Href)
			if err != nil {
				continue
			}
			for _, href := range okHrefs(cand, propName) {
				target, err := p.server.ResolveHref(href)
				if err == nil && slices.Contains(currentPrincipals, target) {
					matches = append(matches, candPath)
				}
			}
		}
	}

	ms := &davxml.MultiStatus{}
	for _, match := range matches {
		if req.Prop != nil && len(req.Prop.Names) > 0 {
			var names []xml.Name
			for _, n := range req.Prop.Names {
				names = append(names, n.XMLName)
			}
			responses, err := p.server.PropertiesForPath(r.Context(), match, names, 0)
			if err != nil {
				return err
			}
			ms.Responses = append(ms.Responses, responses...)
		} else {
			ms.Responses = append(ms.Responses, davxml.Response{
				Href:   p.server.Href(match),
				Status: http.StatusOK,
			})
		}
	}
	return p.server.WriteMultiStatus(w, ms)
}

// okHrefs extracts the 200-status Href value of a property from a response.
func okHrefs(resp davxml.Response, name xml.Name) []string {
	for _, ps := range resp.Propstats {
		if ps.Status != http.StatusOK {
			continue
		}
		for _, prop := range ps.Props {
			if prop.Name == name {
				if hrefs, ok := prop.Value.(davxml.Href); ok {
					return hrefs
				}
			}
		}
	}
	return nil
}

type aclPrincipalPropSetXML struct {
	XMLName xml.Name `xml:"DAV: acl-principal-prop-set"`
	Prop    *struct {
		Names []struct {
			XMLName xml.Name
		} `xml:",any"`
	} `xml:"prop"`
}

func (p *Plugin) aclPrincipalPropSetReport(body []byte, path string, w http.ResponseWriter, r *http.Request) error {
	if d := r.Header.Get("Depth"); d != "" && d != "0" {
		return dav.ErrBadRequest("The {DAV:}acl-principal-prop-set REPORT only supports Depth 0")
	}
	var req aclPrincipalPropSetXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		return dav.ErrBadRequest("%s", err)
	}

	node, err := p.server.Tree.NodeForPath(r.Context(), path)
	if err != nil {
		return err
	}
	var principals []string
	seen := map[string]bool{}
	for _, ace := range p.nodeACL(node) {
		// Pseudo-principals like {DAV:}owner carry no href.
		if strings.HasPrefix(ace.Principal, "{") || seen[ace.Principal] {
			continue
		}
		seen[ace.Principal] = true
		principals = append(principals, ace.Principal)
	}

	var names []xml.Name
	if req.Prop != nil {
		for _, n := range req.Prop.Names {
			names = append(names, n.XMLName)
		}
	}
	ms := &davxml.MultiStatus{}
	for _, principal := range principals {
		responses, err := p.server.PropertiesForPath(r.Context(), principal, names, 0)
		if err != nil {
			return err
		}
		ms.Responses = append(ms.Responses, responses...)
	}
	return p.server.WriteMultiStatus(w, ms)
}
