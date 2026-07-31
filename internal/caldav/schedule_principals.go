// Principal/address resolution and group-attendee expansion for scheduling.

package caldav

import (
	"encoding/xml"
	"strings"

	goical "github.com/emersion/go-ical"

	"calserver/internal/acl"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
	"calserver/internal/pathutil"
)

// defaultCalendarPath picks the principal's first VEVENT-capable calendar,
// like sabre's default-calendar search.
func (p *SchedulePlugin) defaultCalendarPath(principalURI string) string {
	_, name := pathutil.Split(principalURI)
	home, err := p.server.Tree.NodeForPath("calendars/" + name)
	if err != nil {
		return ""
	}
	coll, ok := home.(dav.Collection)
	if !ok {
		return ""
	}
	children, err := coll.Children()
	if err != nil {
		return ""
	}
	for _, child := range children {
		if calNode, isCal := child.(*Calendar); isCal {
			return "calendars/" + name + "/" + calNode.Name()
		}
	}
	return ""
}

// addressesForPrincipal returns a principal's calendar user addresses.
func (p *SchedulePlugin) addressesForPrincipal(principalURI string) []string {
	node, err := p.server.Tree.NodeForPath(principalURI)
	if err != nil {
		return nil
	}
	ph, ok := node.(acl.PrincipalHolder)
	if !ok {
		return nil
	}
	return append(ph.AlternateURISet(), p.server.BaseURI+ph.PrincipalURL()+"/")
}

// principalEmail returns a principal node's email address, if any.
func principalEmail(node dav.Node) string {
	pn, ok := node.(dav.PropNode)
	if !ok {
		return ""
	}
	props, err := pn.Props([]xml.Name{{Space: davxml.NSSabreDAV, Local: "email-address"}})
	if err != nil {
		return ""
	}
	if v, ok := props[xml.Name{Space: davxml.NSSabreDAV, Local: "email-address"}].(string); ok {
		return v
	}
	return ""
}

// principalName returns a principal node's display name, if any.
func principalName(node dav.Node) string {
	pn, ok := node.(dav.PropNode)
	if !ok {
		return ""
	}
	props, err := pn.Props([]xml.Name{{Space: davxml.NSDav, Local: "displayname"}})
	if err != nil {
		return ""
	}
	if v, ok := props[xml.Name{Space: davxml.NSDav, Local: "displayname"}].(string); ok {
		return v
	}
	return ""
}

// principalForAddress finds the principal owning a calendar user address
// (sabre's getPrincipalByUri via calendar-user-address-set).
func (p *SchedulePlugin) principalForAddress(address string) string {
	principals, err := p.server.Tree.NodeForPath("principals")
	if err != nil {
		return ""
	}
	coll, ok := principals.(dav.Collection)
	if !ok {
		return ""
	}
	children, err := coll.Children()
	if err != nil {
		return ""
	}
	for _, child := range children {
		ph, isPrincipal := child.(acl.PrincipalHolder)
		if !isPrincipal {
			continue
		}
		for _, candidate := range ph.AlternateURISet() {
			if strings.EqualFold(candidate, address) {
				return ph.PrincipalURL()
			}
		}
	}
	return ""
}

// groupMemberInfo is one expanded member of a group attendee.
type groupMemberInfo struct{ address, name, email string }

// groupMembers reports whether the calendar-user address is a group
// principal, and if so its members' scheduling info. A principal counts as a
// group when it carries a non-empty {DAV:}group-member-set (only groups and
// proxy sub-principals do, and proxy sub-principals are never attendees).
func (p *SchedulePlugin) groupMembers(address string) (members []groupMemberInfo, groupEmail string, isGroup bool) {
	uri := p.principalForAddress(address)
	if uri == "" {
		return nil, "", false
	}
	node, err := p.server.Tree.NodeForPath(uri)
	if err != nil {
		return nil, "", false
	}
	holder, ok := node.(acl.PrincipalHolder)
	if !ok {
		return nil, "", false
	}
	set, err := holder.GroupMemberSet()
	if err != nil || len(set) == 0 {
		return nil, "", false
	}
	groupEmail = principalEmail(node)
	for _, memberURI := range set {
		mnode, err := p.server.Tree.NodeForPath(memberURI)
		if err != nil {
			continue
		}
		mh, ok := mnode.(acl.PrincipalHolder)
		if !ok {
			continue
		}
		addr := ""
		for _, a := range mh.AlternateURISet() {
			if strings.HasPrefix(strings.ToLower(a), "mailto:") {
				addr = a
				break
			}
		}
		if addr == "" {
			addr = p.server.BaseURI + memberURI + "/"
		}
		members = append(members, groupMemberInfo{
			address: addr, name: principalName(mnode), email: principalEmail(mnode),
		})
	}
	return members, groupEmail, true
}

// expandGroupAttendees rewrites each group ATTENDEE into a display-only
// X-SERVER-GROUP placeholder and appends one real ATTENDEE per member,
// mirroring Calendar Server's GroupAttendeeReconciliation. Returns whether it
// changed the calendar.
// maxExpandedAttendees caps how many members a single event's group ATTENDEEs
// expand into, bounding scheduling/iMIP fan-out (mail-amplification defense).
const maxExpandedAttendees = 250

func (p *SchedulePlugin) expandGroupAttendees(cal *goical.Calendar) bool {
	changed := false
	for _, comp := range cal.Children {
		if comp.Name != goical.CompEvent {
			continue
		}
		atts := comp.Props["ATTENDEE"]
		// Existing attendee addresses, so a member already invited directly
		// is not added twice.
		present := map[string]bool{}
		for i := range atts {
			present[strings.ToLower(atts[i].Value)] = true
		}
		var added []goical.Prop
		for i := range atts {
			if atts[i].Params.Get("CUTYPE") == "X-SERVER-GROUP" {
				continue // already expanded
			}
			members, groupEmail, isGroup := p.groupMembers(atts[i].Value)
			if !isGroup {
				continue
			}
			changed = true
			// The group attendee becomes a non-schedulable placeholder.
			if atts[i].Params == nil {
				atts[i].Params = make(goical.Params)
			}
			atts[i].Params.Set("CUTYPE", "X-SERVER-GROUP")
			atts[i].Params.Set("SCHEDULE-STATUS", "2.7")
			if groupEmail != "" {
				atts[i].Params.Set("EMAIL", groupEmail)
			}
			atts[i].Params.Del("PARTSTAT")
			atts[i].Params.Del("RSVP")

			for _, m := range members {
				// Bound fan-out: a huge (or maliciously crafted) group must not
				// expand into an unbounded number of attendees — each becomes an
				// iTIP message and potentially an iMIP email. Past the cap the
				// group stays a placeholder with the remaining members
				// uninvited; narrow the group instead.
				if len(added) >= maxExpandedAttendees {
					atts[i].Params.Set("SCHEDULE-STATUS", "5.3")
					break
				}
				if present[strings.ToLower(m.address)] {
					continue
				}
				present[strings.ToLower(m.address)] = true
				member := goical.NewProp("ATTENDEE")
				member.Value = m.address
				member.Params.Set("CUTYPE", "INDIVIDUAL")
				member.Params.Set("PARTSTAT", "NEEDS-ACTION")
				member.Params.Set("RSVP", "TRUE")
				member.Params.Set("MEMBER", atts[i].Value)
				if m.name != "" {
					member.Params.Set("CN", m.name)
				}
				if m.email != "" {
					member.Params.Set("EMAIL", m.email)
				}
				added = append(added, *member)
			}
		}
		if len(added) > 0 {
			comp.Props["ATTENDEE"] = append(atts, added...)
		}
	}
	return changed
}
