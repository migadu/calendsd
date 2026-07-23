// iTIP scheduling broker: generates scheduling messages from calendar-object
// changes and applies incoming messages to recipient copies. Port of the
// core flows of sabre/vobject's ITip\Broker (new invites, updates,
// cancellations, attendee replies). Per-instance attendee handling
// (RECURRENCE-ID-scoped invitations) is simplified to whole-object
// granularity — TODO(parity): port the per-instance matrix.
package ical

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"
)

// Message is one iTIP scheduling message. Port of ITip\Message.
type Message struct {
	UID    string
	Method string // REQUEST, CANCEL, REPLY
	// Component is the scheduled component type (VEVENT).
	Component string
	// Sender/Recipient are calendar user addresses (mailto:...).
	Sender        string
	SenderName    string
	Recipient     string
	RecipientName string
	// SignificantChange marks changes attendees must be re-invited for.
	SignificantChange bool
	// ScheduleStatus is filled by delivery (e.g. "1.2", "3.7;...").
	ScheduleStatus string
	// Calendar is the iTIP payload (VCALENDAR with METHOD).
	Calendar *goical.Calendar
}

// ScheduleStatusCode returns the numeric part of the schedule status.
func (m *Message) ScheduleStatusCode() string {
	code, _, _ := strings.Cut(m.ScheduleStatus, ";")
	return code
}

// attendeeInfo captures one ATTENDEE property.
type attendeeInfo struct {
	href     string
	name     string
	partStat string
	prop     goical.Prop
}

// eventSummary captures the scheduling-relevant view of an object.
type eventSummary struct {
	uid       string
	organizer string
	orgName   string
	sequence  string
	attendees []attendeeInfo
	master    *goical.Component
	cal       *goical.Calendar
}

func summarize(cal *goical.Calendar) *eventSummary {
	if cal == nil {
		return nil
	}
	out := &eventSummary{cal: cal, sequence: "0"}
	seen := map[string]bool{}
	for _, comp := range cal.Children {
		if comp.Name != goical.CompEvent {
			continue
		}
		if out.master == nil {
			out.master = comp
		}
		if p := comp.Props.Get(goical.PropUID); p != nil && out.uid == "" {
			out.uid = p.Value
		}
		if p := comp.Props.Get("ORGANIZER"); p != nil && out.organizer == "" {
			out.organizer = strings.ToLower(p.Value)
			out.orgName = p.Params.Get("CN")
		}
		if p := comp.Props.Get("SEQUENCE"); p != nil {
			out.sequence = p.Value
		}
		// Attendees union across instances (sabre does per-instance).
		for _, att := range comp.Props.Values("ATTENDEE") {
			href := strings.ToLower(att.Value)
			if seen[href] {
				continue
			}
			seen[href] = true
			partStat := att.Params.Get("PARTSTAT")
			if partStat == "" {
				partStat = "NEEDS-ACTION"
			}
			out.attendees = append(out.attendees, attendeeInfo{
				href: href, name: att.Params.Get("CN"), partStat: partStat, prop: att,
			})
		}
	}
	if out.master == nil {
		return nil
	}
	return out
}

// ParseEvent computes the scheduling messages a change requires. newCal is
// nil for deletions, oldCal nil for creations; addresses are the acting
// user's calendar-user addresses. Port of Broker::parseEvent.
func ParseEvent(newCal *goical.Calendar, addresses []string, oldCal *goical.Calendar) ([]*Message, error) {
	newInfo, oldInfo := summarize(newCal), summarize(oldCal)
	base := newInfo
	if base == nil {
		base = oldInfo
	}
	if base == nil || base.organizer == "" {
		return nil, nil // not a scheduled object
	}

	lowered := make(map[string]bool, len(addresses))
	for _, a := range addresses {
		lowered[strings.ToLower(a)] = true
	}

	if lowered[base.organizer] {
		return parseAsOrganizer(newInfo, oldInfo)
	}
	return parseAsAttendee(newInfo, oldInfo, lowered)
}

func parseAsOrganizer(newInfo, oldInfo *eventSummary) ([]*Message, error) {
	var out []*Message

	significant := significantChange(newInfo, oldInfo)
	current := map[string]attendeeInfo{}
	if newInfo != nil {
		for _, att := range newInfo.attendees {
			current[att.href] = att
			if att.href == newInfo.organizer {
				continue
			}
			msg := &Message{
				UID: newInfo.uid, Method: "REQUEST", Component: "VEVENT",
				Sender: newInfo.organizer, SenderName: newInfo.orgName,
				Recipient: att.href, RecipientName: att.name,
				SignificantChange: significant,
				Calendar:          itipPayload(newInfo.cal, "REQUEST"),
			}
			out = append(out, msg)
		}
	}
	// Attendees removed (or the whole event deleted) get CANCEL.
	if oldInfo != nil {
		for _, att := range oldInfo.attendees {
			if att.href == oldInfo.organizer {
				continue
			}
			if _, still := current[att.href]; still {
				continue
			}
			out = append(out, &Message{
				UID: oldInfo.uid, Method: "CANCEL", Component: "VEVENT",
				Sender: oldInfo.organizer, SenderName: oldInfo.orgName,
				Recipient: att.href, RecipientName: att.name,
				SignificantChange: true,
				Calendar:          cancelPayload(oldInfo),
			})
		}
	}
	return out, nil
}

func parseAsAttendee(newInfo, oldInfo *eventSummary, addresses map[string]bool) ([]*Message, error) {
	// Find the acting attendee and their old/new participation state.
	find := func(info *eventSummary) *attendeeInfo {
		if info == nil {
			return nil
		}
		for i := range info.attendees {
			if addresses[info.attendees[i].href] {
				return &info.attendees[i]
			}
		}
		return nil
	}
	newAtt, oldAtt := find(newInfo), find(oldInfo)
	base := newInfo
	if base == nil {
		base = oldInfo
	}
	if newAtt == nil && oldAtt == nil {
		return nil, nil
	}

	partStat := "DECLINED" // deletion by an attendee declines
	var att *attendeeInfo
	if newAtt != nil {
		att = newAtt
		partStat = newAtt.partStat
		if oldAtt != nil && strings.EqualFold(oldAtt.partStat, partStat) {
			return nil, nil // no participation change, no REPLY
		}
	} else {
		att = oldAtt
	}

	reply := goical.NewCalendar()
	reply.Props.SetText(goical.PropVersion, "2.0")
	setPlainProp(reply.Props, goical.PropProductID, "-//calserver//calserver//EN")
	setPlainProp(reply.Props, "METHOD", "REPLY")
	ev := goical.NewComponent(goical.CompEvent)
	setPlainProp(ev.Props, goical.PropUID, base.uid)
	setPlainProp(ev.Props, "SEQUENCE", base.sequence)
	setPlainProp(ev.Props, "DTSTAMP", time.Now().UTC().Format("20060102T150405Z"))
	org := goical.NewProp("ORGANIZER")
	org.Value = base.organizer
	if base.orgName != "" {
		org.Params.Set("CN", base.orgName)
	}
	ev.Props.Set(org)
	attendee := goical.NewProp("ATTENDEE")
	attendee.Value = att.href
	attendee.Params.Set("PARTSTAT", partStat)
	if att.name != "" {
		attendee.Params.Set("CN", att.name)
	}
	ev.Props.Set(attendee)
	reply.Children = append(reply.Children, ev)

	return []*Message{{
		UID: base.uid, Method: "REPLY", Component: "VEVENT",
		Sender: att.href, SenderName: att.name,
		Recipient: base.organizer, RecipientName: base.orgName,
		SignificantChange: true,
		Calendar:          reply,
	}}, nil
}

// significantChange decides whether attendees must act again: sequence,
// recurrence, or time changes. Subset of sabre's significant-change hash.
func significantChange(newInfo, oldInfo *eventSummary) bool {
	if newInfo == nil || oldInfo == nil {
		return true
	}
	get := func(info *eventSummary, name string) string {
		if p := info.master.Props.Get(name); p != nil {
			return p.Value
		}
		return ""
	}
	for _, name := range []string{"SEQUENCE", "RRULE", "DTSTART", "DTEND", "DURATION", "STATUS", "EXDATE", "RDATE"} {
		if get(newInfo, name) != get(oldInfo, name) {
			return true
		}
	}
	return false
}

// itipPayload clones a calendar as an iTIP message with the given METHOD.
func itipPayload(cal *goical.Calendar, method string) *goical.Calendar {
	out := goical.NewCalendar()
	for name, props := range cal.Props {
		if name == "METHOD" {
			continue
		}
		for _, p := range props {
			cp := p
			out.Props.Add(&cp)
		}
	}
	if out.Props.Get(goical.PropVersion) == nil {
		out.Props.SetText(goical.PropVersion, "2.0")
	}
	if method != "" {
		setPlainProp(out.Props, "METHOD", method)
	}
	for _, child := range cal.Children {
		out.Children = append(out.Children, cloneComponent(child))
	}
	return out
}

// cancelPayload builds the minimal CANCEL message body.
func cancelPayload(info *eventSummary) *goical.Calendar {
	out := goical.NewCalendar()
	out.Props.SetText(goical.PropVersion, "2.0")
	setPlainProp(out.Props, goical.PropProductID, "-//calserver//calserver//EN")
	setPlainProp(out.Props, "METHOD", "CANCEL")
	ev := goical.NewComponent(goical.CompEvent)
	setPlainProp(ev.Props, goical.PropUID, info.uid)
	seq, _ := strconv.Atoi(info.sequence)
	setPlainProp(ev.Props, "SEQUENCE", strconv.Itoa(seq+1))
	setPlainProp(ev.Props, "DTSTAMP", time.Now().UTC().Format("20060102T150405Z"))
	if dtstart := info.master.Props.Get(goical.PropDateTimeStart); dtstart != nil {
		cp := *dtstart
		ev.Props.Set(&cp)
	}
	org := goical.NewProp("ORGANIZER")
	org.Value = info.organizer
	ev.Props.Set(org)
	out.Children = append(out.Children, ev)
	return out
}

// ProcessMessage applies an incoming iTIP message to the recipient's
// existing copy (nil for none), returning the new object state or nil when
// the message is not understood. Port of Broker::processMessage.
func ProcessMessage(msg *Message, existing *goical.Calendar) (*goical.Calendar, error) {
	switch msg.Method {
	case "REQUEST":
		// The attendee copy is the invitation itself, minus METHOD.
		return itipPayload(msg.Calendar, ""), nil
	case "CANCEL":
		if existing == nil {
			return nil, nil
		}
		for _, comp := range existing.Children {
			if comp.Name == goical.CompEvent {
				setPlainProp(comp.Props, "STATUS", "CANCELLED")
			}
		}
		return existing, nil
	case "REPLY":
		if existing == nil {
			return nil, nil
		}
		replyInfo := summarize(msg.Calendar)
		if replyInfo == nil || len(replyInfo.attendees) == 0 {
			return nil, nil
		}
		replier := replyInfo.attendees[0]
		for _, comp := range existing.Children {
			if comp.Name != goical.CompEvent {
				continue
			}
			atts := comp.Props.Values("ATTENDEE")
			for i := range atts {
				if strings.EqualFold(atts[i].Value, replier.href) {
					atts[i].Params.Set("PARTSTAT", replier.partStat)
					atts[i].Params.Del("SCHEDULE-STATUS")
				}
			}
			comp.Props["ATTENDEE"] = atts
		}
		return existing, nil
	default:
		return nil, fmt.Errorf("unsupported iTIP method %q", msg.Method)
	}
}

// setPlainProp writes a property value without a VALUE parameter.
func setPlainProp(props goical.Props, name, value string) {
	prop := goical.NewProp(name)
	prop.Value = value
	props.Set(prop)
}
