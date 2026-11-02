package caldav

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/dav"
	davxml "github.com/migadu/calendsd/internal/dav/xml"
)

// Free-busy visibility (VAVAILABILITY-PLAN.md §10): who may see the owner's
// busy time and working hours, as a live, owner-only property of the
// schedule inbox. The ACL plugin enforces it through acl.FreeBusyAudience.
const nsCalendsd = "urn:x-calserver"

var freeBusyVisibilityName = xml.Name{Space: nsCalendsd, Local: "free-busy-visibility"}

// maxFreeBusyAllowed bounds the allowed list (every non-owner read-free-busy
// check scans it).
const maxFreeBusyAllowed = 200

// inboxVisibilityPropFind serves free-busy-visibility. It is always present:
// without a stored setting it reads as the default (audience domain, nobody
// extra).
func (p *SchedulePlugin) inboxVisibilityPropFind(ctx context.Context, pf *dav.PropFind, inbox *Inbox) {
	vb := inbox.visibility()
	if vb == nil {
		return
	}
	pf.Handle(freeBusyVisibilityName, func() any {
		v, err := vb.FreeBusyVisibility(ctx, inbox.principalURI)
		if err != nil {
			slog.Warn("reading free-busy visibility", "principal", inbox.principalURI, "err", err)
			return nil
		}
		return p.renderVisibility(v)
	})
}

func (p *SchedulePlugin) renderVisibility(v acl.FreeBusyVisibility) davxml.Raw {
	var b strings.Builder
	audience := "domain"
	if v.Hidden {
		audience = "nobody"
	}
	b.WriteString(`<audience xmlns="` + nsCalendsd + `">` + audience + `</audience>`)
	b.WriteString(`<allow xmlns="` + nsCalendsd + `">`)
	for _, entry := range v.Allowed {
		href := entry
		if !strings.HasPrefix(entry, "mailto:") {
			href = p.server.Href(entry) + "/"
		}
		b.WriteString(`<href xmlns="DAV:">`)
		_ = xml.EscapeText(&b, []byte(href))
		b.WriteString(`</href>`)
	}
	b.WriteString(`</allow>`)
	return davxml.Raw(b.String())
}

// prePropPatchVisibility claims free-busy-visibility on an inbox. Like
// calendar-availability, the value is validated before any PROPPATCH
// callback runs, so a rejected value stores nothing.
func (p *SchedulePlugin) prePropPatchVisibility(ctx context.Context, inbox *Inbox, pp *dav.PropPatch) {
	value, ok := pp.Mutation(freeBusyVisibilityName)
	if !ok {
		return
	}
	names := []xml.Name{freeBusyVisibilityName}
	reject := func(reason string, err error) {
		slog.Debug("free-busy-visibility PROPPATCH rejected", "principal", inbox.principalURI, "reason", reason, "err", err)
		pp.SetResultCode(names, http.StatusForbidden)
	}
	vb := inbox.visibility()
	if vb == nil {
		reject("backend has no visibility store", nil)
		return
	}

	remove := value == nil
	var v acl.FreeBusyVisibility
	if !remove {
		var fragment []byte
		switch t := value.(type) {
		case []byte:
			fragment = t
		case string:
			if strings.TrimSpace(t) != "" {
				reject("value is text, not XML", nil)
				return
			}
			// An empty element: the default setting.
		}
		parsed, err := p.parseVisibility(ctx, fragment, p.principalDomain(ctx, inbox.principalURI))
		if err != nil {
			reject("invalid value", err)
			return
		}
		v = parsed
	}

	pp.Handle(names, func(map[xml.Name]any) (map[xml.Name]int, bool) {
		var err error
		if remove {
			err = vb.DeleteFreeBusyVisibility(ctx, inbox.principalURI)
		} else {
			err = vb.SetFreeBusyVisibility(ctx, inbox.principalURI, v)
		}
		if err != nil {
			slog.Error("storing free-busy visibility", "principal", inbox.principalURI, "err", err)
			return nil, false
		}
		return nil, true
	})
}

// parseVisibility reads the property's element content: an optional
// <audience> (domain | nobody) and an optional <allow> of DAV:href entries
// (see allowedEntry). ownerDomain is the inbox owner's mail domain.
func (p *SchedulePlugin) parseVisibility(ctx context.Context, fragment []byte, ownerDomain string) (acl.FreeBusyVisibility, error) {
	var v acl.FreeBusyVisibility
	dec := xml.NewDecoder(io.MultiReader(
		strings.NewReader("<v>"), bytes.NewReader(fragment), strings.NewReader("</v>")))
	var path []xml.Name
	var text strings.Builder
	seen := map[string]bool{}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return v, nil
		}
		if err != nil {
			return v, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			path = append(path, t.Name)
			text.Reset()
			switch {
			case len(path) == 2 && t.Name.Space == nsCalendsd && (t.Name.Local == "audience" || t.Name.Local == "allow"):
			case len(path) == 3 && path[1].Local == "allow" && t.Name == xml.Name{Space: davxml.NSDav, Local: "href"}:
			case len(path) == 1:
			default:
				return v, fmt.Errorf("unexpected element {%s}%s", t.Name.Space, t.Name.Local)
			}
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			value := strings.TrimSpace(text.String())
			switch {
			case len(path) == 2 && t.Name.Local == "audience":
				switch value {
				case "domain":
					v.Hidden = false
				case "nobody":
					v.Hidden = true
				default:
					return v, fmt.Errorf("unknown audience %q", value)
				}
			case len(path) == 3:
				uri := p.allowedEntry(ctx, value, ownerDomain)
				if uri == "" {
					return v, fmt.Errorf("cannot grant %q", value)
				}
				if !seen[uri] {
					seen[uri] = true
					v.Allowed = append(v.Allowed, uri)
					if len(v.Allowed) > maxFreeBusyAllowed {
						return v, fmt.Errorf("more than %d allowed principals", maxFreeBusyAllowed)
					}
				}
			}
			path = path[:len(path)-1]
			text.Reset()
		}
	}
}

// allowedEntry turns an allow-list href into the stored entry, or "" when it
// cannot be granted. It must not reveal which accounts exist outside the
// owner's domain, which the server hides everywhere else (listings, the 3.7
// free-busy answer):
//
//   - A mailto: address that belongs to a principal in the owner's domain is
//     stored as that principal's URI (so a group grant covers its members).
//     Any other well-formed address is stored as the address itself, whether
//     or not an account has it, and matched against the requester's
//     addresses when free-busy is checked.
//   - A principal path is accepted only for a principal in the owner's
//     domain; any other path is refused exactly like one that does not
//     exist.
func (p *SchedulePlugin) allowedEntry(ctx context.Context, href, ownerDomain string) string {
	if strings.HasPrefix(strings.ToLower(href), "mailto:") {
		addr := strings.ToLower(href[len("mailto:"):])
		at := strings.LastIndex(addr, "@")
		if at <= 0 || at == len(addr)-1 || strings.ContainsAny(addr, " <>") {
			return ""
		}
		if uri := principalForAddress(ctx, p.server, "mailto:"+addr); uri != "" && ownerDomain != "" &&
			p.principalDomain(ctx, uri) == ownerDomain {
			return uri
		}
		return "mailto:" + addr
	}
	path := strings.Trim(strings.TrimPrefix(href, p.server.BaseURI), "/")
	if !strings.HasPrefix(path, "principals/") || ownerDomain == "" {
		return ""
	}
	node, err := p.server.Tree.NodeForPath(ctx, path)
	if err != nil {
		return ""
	}
	ph, ok := node.(acl.PrincipalHolder)
	if !ok || p.principalDomain(ctx, ph.PrincipalURL()) != ownerDomain {
		return ""
	}
	return ph.PrincipalURL()
}

// principalDomain returns a principal's lower-cased mail domain, or "".
func (p *SchedulePlugin) principalDomain(ctx context.Context, principalURI string) string {
	node, err := p.server.Tree.NodeForPath(ctx, principalURI)
	if err != nil {
		return ""
	}
	email := principalEmail(ctx, node)
	if at := strings.LastIndex(email, "@"); at >= 0 && at < len(email)-1 {
		return strings.ToLower(email[at+1:])
	}
	return ""
}
