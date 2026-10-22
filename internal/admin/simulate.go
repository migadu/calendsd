package admin

// Privilege simulator (ADMIN-PLAN.md §3.5): answers "what can user X do on
// path Y" through the production composition (server.NewLoggedIn). The
// privilege check runs as a synthetic PROPFIND through the full plugin
// chain — a 403/404 status is itself the answer to "why can't X see Y" —
// while the effective-ACL view walks the real node tree and renders the
// acl plugin's ACLForNode.

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"

	"calserver/internal/acl"
	davxml "calserver/internal/dav/xml"
)

// adminViewPrincipal matches the AdminPrincipals list hardcoded in
// server.compose; the ACL/owner view runs as it.
const adminViewPrincipal = "principals/admin"

const cupsBody = `<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:"><d:prop><d:current-user-privilege-set/><d:resourcetype/></d:prop></d:propfind>`

// ACEView is one effective-ACL entry.
type ACEView struct {
	Principal string `json:"principal"`
	Privilege string `json:"privilege"`
	Protected bool   `json:"protected"`
}

type SimulateData struct {
	Principal string `json:"principal"`
	Path      string `json:"path"`
	// Requested is false when the form page loads without parameters.
	Requested bool `json:"requested"`
	// Unavailable is set when the app runs without a simulator.
	Unavailable bool `json:"unavailable"`

	// Status/Privileges: the PROPFIND as the target principal.
	Status     int      `json:"status"`
	Privileges []string `json:"privileges"`
	RawBody    string   `json:"rawBody"`

	// Owner/ACL: the node's effective ACL from the real tree.
	Owner   string    `json:"owner"`
	ACL     []ACEView `json:"acl"`
	ACLNote string    `json:"aclNote"`
}

func (a *App) simulateData(r *http.Request) (any, error) {
	q := r.URL.Query()
	d := &SimulateData{Principal: q.Get("principal"), Path: q.Get("path")}
	if d.Principal == "" && d.Path == "" {
		return d, nil // bare form page
	}
	if d.Principal == "" || d.Path == "" {
		return nil, fmt.Errorf("%w: need both ?principal= and ?path=", errBadRequest)
	}
	if a.NewSimulator == nil {
		d.Unavailable = true
		return d, nil
	}
	d.Requested = true
	if !strings.HasPrefix(d.Path, "/") {
		d.Path = "/" + d.Path
	}

	// As the target principal: what privileges do they hold here?
	w := davRequest(a.NewSimulator(d.Principal), "PROPFIND", d.Path, cupsBody, "0")
	d.Status = w.Code
	d.RawBody = w.Body.String()
	if w.Code == http.StatusMultiStatus {
		privs, err := parsePrivileges(w.Body.Bytes())
		if err != nil {
			return nil, fmt.Errorf("parsing privilege response: %w", err)
		}
		d.Privileges = privs
	}

	// Effective ACL: resolve the node in the real tree and render the acl
	// plugin's view of it (the plan's "walk the tree, render ACL()").
	srv := a.NewSimulator(adminViewPrincipal)
	node, err := srv.Tree.NodeForPath(r.Context(), strings.Trim(d.Path, "/"))
	if err != nil {
		d.ACLNote = "node not found: " + err.Error()
		return d, nil
	}
	plugin, _ := srv.Plugin("acl").(*acl.Plugin)
	if plugin == nil {
		d.ACLNote = "acl plugin not in the composition"
		return d, nil
	}
	if owned, ok := node.(interface{ Owner() string }); ok {
		d.Owner = owned.Owner()
	}
	for _, ace := range plugin.ACLForNode(node) {
		d.ACL = append(d.ACL, ACEView{
			Principal: ace.Principal, Privilege: ace.Privilege, Protected: ace.Protected})
	}
	return d, nil
}

// Multistatus subset for the privilege PROPFIND. Privileges arrive as
// empty child elements of d:privilege; xmlAny captures their names.
type xmlAny struct {
	XMLName xml.Name
}

type xmlPrivilege struct {
	Inner []xmlAny `xml:",any"`
}

type xmlMultistatus struct {
	Responses []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Status string `xml:"status"`
			Prop   struct {
				CUPS *struct {
					Privileges []xmlPrivilege `xml:"privilege"`
				} `xml:"current-user-privilege-set"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

func parsePrivileges(body []byte) ([]string, error) {
	var ms xmlMultistatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return nil, err
	}
	for _, resp := range ms.Responses {
		for _, ps := range resp.Propstat {
			if !strings.Contains(ps.Status, "200") || ps.Prop.CUPS == nil {
				continue
			}
			var out []string
			for _, p := range ps.Prop.CUPS.Privileges {
				for _, inner := range p.Inner {
					out = append(out, davxml.Clark(inner.XMLName))
				}
			}
			return out, nil
		}
	}
	return nil, nil
}
