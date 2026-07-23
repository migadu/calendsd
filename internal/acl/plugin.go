package acl

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"strings"

	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
)

// CurrentPrincipalSource decouples the ACL plugin from the auth plugin: any
// plugin exposing the authenticated principal works.
type CurrentPrincipalSource interface {
	CurrentPrincipal() string
}

// Plugin implements access control: principal properties, principal reports,
// and (as the port grows) privilege checks. Port of sabre's DAVACL\Plugin.
type Plugin struct {
	server *dav.Server

	// AdminPrincipals get an implicit {DAV:}all ACE on every node,
	// mirroring sabre's adminPrincipals.
	AdminPrincipals []string
	// AllowUnauthenticatedAccess mirrors sabre's flag; when false,
	// requests without a principal fail once privilege checks land.
	AllowUnauthenticatedAccess bool
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
}

func (p *Plugin) Features() []string {
	return []string{"access-control", "calendarserver-principal-property-search"}
}

// currentPrincipal finds the authenticated principal via the auth plugin.
func (p *Plugin) currentPrincipal() string {
	for _, plugin := range p.server.Plugins() {
		if src, ok := plugin.(CurrentPrincipalSource); ok {
			if principal := src.CurrentPrincipal(); principal != "" {
				return principal
			}
		}
	}
	return ""
}

// CurrentUserPrincipals returns the current principal plus its group
// membership, mirroring sabre's getCurrentUserPrincipals.
func (p *Plugin) CurrentUserPrincipals() ([]string, error) {
	current := p.currentPrincipal()
	if current == "" {
		return nil, nil
	}
	out := []string{current}
	node, err := p.server.Tree.NodeForPath(current)
	if err != nil {
		return out, nil // principal may not be in the tree
	}
	if ph, ok := node.(PrincipalHolder); ok {
		groups, err := ph.GroupMembership()
		if err != nil {
			return nil, err
		}
		out = append(out, groups...)
	}
	return out, nil
}

// ACLForNode merges a node's own ACL with the admin ACEs, mirroring sabre's
// Plugin::getACL.
func (p *Plugin) ACLForNode(node dav.Node) []ACE {
	var acl []ACE
	if holder, ok := node.(ACLHolder); ok {
		acl = append(acl, holder.ACL()...)
	}
	for _, admin := range p.AdminPrincipals {
		acl = append(acl, ACE{Privilege: "{DAV:}all", Principal: admin, Protected: true})
	}
	return acl
}

// PropFind implements dav.PropFindHook: principal properties plus
// current-user-principal.
func (p *Plugin) PropFind(pf *dav.PropFind, node dav.Node) error {
	davName := func(local string) xml.Name { return xml.Name{Space: davxml.NSDav, Local: local} }

	if ph, isPrincipal := node.(PrincipalHolder); isPrincipal {
		pf.Handle(davName("alternate-URI-set"), func() any {
			return davxml.Href(ph.AlternateURISet())
		})
		pf.Handle(davName("principal-URL"), func() any {
			return davxml.Href{p.server.BaseURI + ph.PrincipalURL() + "/"}
		})
		pf.Handle(davName("group-member-set"), func() any {
			return p.principalHrefs(ph.GroupMemberSet)
		})
		pf.Handle(davName("group-membership"), func() any {
			return p.principalHrefs(ph.GroupMembership)
		})
	}

	pf.Handle(davName("current-user-principal"), func() any {
		if current := p.currentPrincipal(); current != "" {
			return davxml.Href{p.server.BaseURI + current + "/"}
		}
		return davxml.Raw(`<d:unauthenticated/>`)
	})
	pf.Handle(davName("owner"), func() any {
		if owned, ok := node.(interface{ Owner() string }); ok {
			return davxml.Href{p.server.BaseURI + owned.Owner() + "/"}
		}
		return nil
	})
	return nil
}

func (p *Plugin) principalHrefs(fn func() ([]string, error)) any {
	paths, err := fn()
	if err != nil {
		return nil
	}
	hrefs := make(davxml.Href, len(paths))
	for i, path := range paths {
		hrefs[i] = p.server.BaseURI + path + "/"
	}
	return hrefs
}

// Report implements dav.ReportHandler for the principal reports.
func (p *Plugin) Report(name xml.Name, body []byte, path string, w http.ResponseWriter, r *http.Request) (bool, error) {
	switch name {
	case xml.Name{Space: davxml.NSDav, Local: "principal-match"}:
		return true, p.principalMatchReport(body, path, w, r)
	case xml.Name{Space: davxml.NSDav, Local: "acl-principal-prop-set"}:
		return true, p.aclPrincipalPropSetReport(body, path, w, r)
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

	currentPrincipals, err := p.CurrentUserPrincipals()
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
		candidates, err := p.server.PropertiesForPath(path, []xml.Name{propName}, 1)
		if err != nil {
			return err
		}
		for _, cand := range candidates {
			hrefs := okHrefs(cand, propName)
			for _, href := range hrefs {
				href = strings.Trim(strings.TrimPrefix(href, p.server.BaseURI), "/")
				for _, principal := range currentPrincipals {
					if href == principal {
						matches = append(matches, strings.Trim(strings.TrimPrefix(cand.Href, p.server.BaseURI), "/"))
					}
				}
			}
		}
	}

	// Sabre serializes these responses without the {DAV:}response wrapper.
	ms := &davxml.MultiStatus{}
	for _, match := range matches {
		if req.Prop != nil && len(req.Prop.Names) > 0 {
			var names []xml.Name
			for _, n := range req.Prop.Names {
				names = append(names, n.XMLName)
			}
			responses, err := p.server.PropertiesForPath(match, names, 0)
			if err != nil {
				return err
			}
			for _, resp := range responses {
				resp.Bare = true
				ms.Responses = append(ms.Responses, resp)
			}
		} else {
			ms.Responses = append(ms.Responses, davxml.Response{
				Href:   p.server.BaseURI + match,
				Status: http.StatusOK,
				Bare:   true,
			})
		}
	}
	return writeMultiStatus(w, ms)
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

	node, err := p.server.Tree.NodeForPath(path)
	if err != nil {
		return err
	}
	var principals []string
	seen := map[string]bool{}
	for _, ace := range p.ACLForNode(node) {
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
		responses, err := p.server.PropertiesForPath(principal, names, 0)
		if err != nil {
			return err
		}
		ms.Responses = append(ms.Responses, responses...)
	}
	return writeMultiStatus(w, ms)
}

func writeMultiStatus(w http.ResponseWriter, ms *davxml.MultiStatus) error {
	body, err := ms.Marshal()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, err = w.Write(body)
	return err
}
