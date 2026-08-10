// iTIP scheduling broker: generates scheduling messages from calendar-object
// changes and applies incoming messages to recipient copies. Port of the
// core flows of sabre/vobject's ITip\Broker (new invites, updates,
// cancellations, attendee replies), including the per-instance
// (RECURRENCE-ID-scoped) attendee matrix: each attendee receives only the
// occurrences they belong to, with EXDATE synthesized on the master for
// excluded instances, and replies scoped to the instances whose PARTSTAT
// changed.
package ical

import (
	"fmt"
	"sort"
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

// eventSummary captures the scheduling-relevant view of an object, including
// the per-instance (RECURRENCE-ID) attendee matrix sabre's broker needs.
type eventSummary struct {
	uid       string
	organizer string
	orgName   string
	sequence  string
	status    string
	exdate    []string // sorted EXDATE values across all components
	master    *goical.Component
	// instances maps a recurrence id ("master" or the RECURRENCE-ID value)
	// to its VEVENT; instOrder preserves calendar order.
	instances map[string]*goical.Component
	instOrder []string
	// attendees is the flat, first-seen list (the reply applier reads it);
	// perAtt/attOrder carry the per-instance participation matrix.
	attendees []attendeeInfo
	perAtt    map[string]*attendeeState
	attOrder  []string
	sigHash   string // significant-change fingerprint
	cal       *goical.Calendar
}

// attendeeState is one attendee's participation across instances.
type attendeeState struct {
	href      string
	name      string
	instances map[string]string // recurrence id -> PARTSTAT
	order     []string          // recurrence ids in first-seen order
	// forceSend is set when the organizer marked this ATTENDEE with
	// SCHEDULE-FORCE-SEND=REQUEST (RFC 6638 §7.1): send a REQUEST even without a
	// significant change — how a client resends a lost invitation.
	forceSend bool
}

// sigProps are the properties whose change makes an update significant (sabre
// Broker::$significantChangeProperties).
var sigProps = []string{"DTSTART", "DTEND", "DURATION", "DUE", "RRULE", "RDATE", "EXDATE", "STATUS"}

func summarize(cal *goical.Calendar) *eventSummary {
	if cal == nil {
		return nil
	}
	out := &eventSummary{
		cal: cal, sequence: "0",
		instances: map[string]*goical.Component{},
		perAtt:    map[string]*attendeeState{},
	}
	flatSeen := map[string]bool{}
	var exset, eventHashes []string
	for _, comp := range cal.Children {
		if comp.Name != goical.CompEvent {
			continue
		}
		rid := recurID(comp)
		out.instances[rid] = comp
		out.instOrder = append(out.instOrder, rid)
		if rid == "master" || out.master == nil {
			out.master = comp
		}
		if p := comp.Props.Get(goical.PropUID); p != nil && out.uid == "" {
			out.uid = p.Value
		}
		if p := comp.Props.Get("ORGANIZER"); p != nil && out.organizer == "" {
			out.organizer = strings.ToLower(p.Value)
			out.orgName = p.Params.Get("CN")
		}
		if p := comp.Props.Get("SEQUENCE"); p != nil && out.sequence == "0" {
			out.sequence = p.Value
		}
		if p := comp.Props.Get("STATUS"); p != nil {
			out.status = strings.ToUpper(p.Value)
		}
		for _, ex := range comp.Props.Values("EXDATE") {
			exset = append(exset, splitList(ex.Value)...)
		}
		// Per-instance attendee participation.
		for _, att := range comp.Props.Values("ATTENDEE") {
			// Expanded Calendar-Server group placeholders are display-only:
			// no iTIP messages, and their SCHEDULE-STATUS=2.7 stays put.
			if att.Params.Get("CUTYPE") == "X-SERVER-GROUP" {
				continue
			}
			href := strings.ToLower(att.Value)
			partStat := att.Params.Get("PARTSTAT")
			if partStat == "" {
				partStat = "NEEDS-ACTION"
			}
			st := out.perAtt[href]
			if st == nil {
				st = &attendeeState{href: href, name: att.Params.Get("CN"), instances: map[string]string{}}
				out.perAtt[href] = st
				out.attOrder = append(out.attOrder, href)
			}
			if _, dup := st.instances[rid]; !dup {
				st.order = append(st.order, rid)
			}
			st.instances[rid] = partStat
			if strings.EqualFold(att.Params.Get("SCHEDULE-FORCE-SEND"), "REQUEST") {
				st.forceSend = true
			}
			if !flatSeen[href] {
				flatSeen[href] = true
				out.attendees = append(out.attendees, attendeeInfo{
					href: href, name: att.Params.Get("CN"), partStat: partStat, prop: att,
				})
			}
		}
		// Per-event significant-change hash.
		var h strings.Builder
		for _, prop := range sigProps {
			vals := comp.Props.Values(prop)
			if len(vals) == 0 {
				continue
			}
			h.WriteString(prop + ":")
			for _, v := range vals {
				h.WriteString(v.Value + ";")
			}
		}
		eventHashes = append(eventHashes, h.String())
	}
	if out.master == nil {
		return nil
	}
	sort.Strings(exset)
	out.exdate = exset
	sort.Strings(eventHashes)
	out.sigHash = strings.Join(eventHashes, "")
	return out
}

// recurID returns a component's recurrence id: the RECURRENCE-ID value, or
// "master" for the base instance.
func recurID(comp *goical.Component) string {
	if p := comp.Props.Get("RECURRENCE-ID"); p != nil {
		return p.Value
	}
	return "master"
}

// splitList splits a comma-separated property value (EXDATE can carry many).
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
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

// parseAsOrganizer generates the per-attendee REQUEST/CANCEL messages for an
// organizer-side change. Port of Broker::parseEventForOrganizer: each attendee
// receives only the instances they belong to, with EXDATE synthesized for the
// master when they are excluded from some occurrences.
func parseAsOrganizer(newInfo, oldInfo *eventSummary) ([]*Message, error) {
	base := newInfo
	if base == nil {
		base = oldInfo
	}
	type merged struct {
		href, name string
		old, new   *attendeeState
	}
	m := map[string]*merged{}
	var order []string
	touch := func(href string) *merged {
		if x := m[href]; x != nil {
			return x
		}
		x := &merged{href: href}
		m[href] = x
		order = append(order, href)
		return x
	}
	if oldInfo != nil {
		for _, href := range oldInfo.attOrder {
			st := oldInfo.perAtt[href]
			x := touch(href)
			x.old, x.name = st, st.name
		}
	}
	if newInfo != nil {
		for _, href := range newInfo.attOrder {
			st := newInfo.perAtt[href]
			x := touch(href)
			x.new = st
			if st.name != "" {
				x.name = st.name
			}
		}
	}

	sigChanged := oldInfo == nil || newInfo == nil || oldInfo.sigHash != newInfo.sigHash

	var out []*Message
	for _, href := range order {
		x := m[href]
		if href == base.organizer {
			continue // an organizer who is also an attendee gets no message
		}
		msg := &Message{
			UID: base.uid, Component: "VEVENT",
			Sender: base.organizer, SenderName: base.orgName,
			Recipient: href, RecipientName: x.name,
		}
		if x.new == nil || len(x.new.instances) == 0 || base.status == "CANCELLED" {
			msg.Method = "CANCEL"
			msg.SignificantChange = true
			msg.Calendar = cancelMessage(base, href, x.name)
		} else {
			msg.Method = "REQUEST"
			// SCHEDULE-FORCE-SEND=REQUEST (RFC 6638) forces a send even when nothing
			// significant changed — the organizer resending a lost invitation.
			msg.SignificantChange = sigChanged || instancesDiffer(x.old, x.new) || x.new.forceSend
			msg.Calendar = requestMessage(newInfo, x.new)
		}
		out = append(out, msg)
	}
	return out, nil
}

// instancesDiffer reports whether the set of instances an attendee belongs to
// changed (a significant change even when no property did).
func instancesDiffer(oldSt, newSt *attendeeState) bool {
	var o, n int
	seen := map[string]bool{}
	if oldSt != nil {
		for id := range oldSt.instances {
			seen[id] = true
			o++
		}
	}
	if newSt != nil {
		for id := range newSt.instances {
			if !seen[id] {
				return true
			}
			n++
		}
	}
	return o != n
}

// requestMessage builds the REQUEST body an attendee receives: only the
// instances they belong to, VTIMEZONEs, and (on the master) EXDATE for the
// occurrences they are excluded from, with server-only scheduling parameters
// stripped. Port of the REQUEST branch of Broker::parseEventForOrganizer.
func requestMessage(info *eventSummary, att *attendeeState) *goical.Calendar {
	out := goical.NewCalendar()
	out.Props.SetText(goical.PropVersion, "2.0")
	setPlainProp(out.Props, goical.PropProductID, "-//calserver//calserver//EN")
	setPlainProp(out.Props, "METHOD", "REQUEST")
	for _, comp := range info.cal.Children {
		if comp.Name == goical.CompTimezone {
			out.Children = append(out.Children, cloneComponent(comp))
		}
	}
	for _, rid := range info.instOrder {
		if _, ok := att.instances[rid]; !ok {
			continue
		}
		ev := cloneComponent(info.instances[rid])
		if rid == "master" {
			var exclude []string
			for _, other := range info.instOrder {
				if other == "master" {
					continue
				}
				if _, ok := att.instances[other]; !ok {
					exclude = append(exclude, other)
				}
			}
			if len(exclude) > 0 {
				addExdate(ev, info.instances["master"], exclude)
			}
			stripScheduleParams(ev)
		}
		setPlainProp(ev.Props, "DTSTAMP", nowStamp())
		out.Children = append(out.Children, ev)
	}
	return out
}

// cancelMessage builds a minimal CANCEL for an attendee who was removed (or
// whose event was deleted/cancelled).
func cancelMessage(info *eventSummary, href, name string) *goical.Calendar {
	out := goical.NewCalendar()
	out.Props.SetText(goical.PropVersion, "2.0")
	setPlainProp(out.Props, goical.PropProductID, "-//calserver//calserver//EN")
	setPlainProp(out.Props, "METHOD", "CANCEL")
	ev := goical.NewComponent(goical.CompEvent)
	setPlainProp(ev.Props, goical.PropUID, info.uid)
	setPlainProp(ev.Props, "SEQUENCE", incSequence(info.sequence))
	setPlainProp(ev.Props, "DTSTAMP", nowStamp())
	m := info.master
	if s := m.Props.Get("SUMMARY"); s != nil {
		setPlainProp(ev.Props, "SUMMARY", s.Value)
	}
	if ds := m.Props.Get("DTSTART"); ds != nil {
		ev.Props["DTSTART"] = []goical.Prop{cloneProp(*ds)}
	}
	if de := m.Props.Get("DTEND"); de != nil {
		ev.Props["DTEND"] = []goical.Prop{cloneProp(*de)}
	} else if du := m.Props.Get("DURATION"); du != nil {
		ev.Props["DURATION"] = []goical.Prop{cloneProp(*du)}
	}
	org := goical.NewProp("ORGANIZER")
	org.Value = info.organizer
	if info.orgName != "" {
		org.Params.Set("CN", info.orgName)
	}
	ev.Props.Set(org)
	at := goical.NewProp("ATTENDEE")
	at.Value = href
	if name != "" {
		at.Params.Set("CN", name)
	}
	ev.Props.Set(at)
	out.Children = append(out.Children, ev)
	return out
}

// parseAsAttendee generates the attendee's REPLY, per instance. Port of
// Broker::parseEventForAttendee: a REPLY carries only the instances whose
// PARTSTAT changed (plus DECLINEs for occurrences the attendee dropped via a
// new EXDATE).
func parseAsAttendee(newInfo, oldInfo *eventSummary, addresses map[string]bool) ([]*Message, error) {
	base := newInfo
	if base == nil {
		base = oldInfo
	}
	if base == nil || base.status == "CANCELLED" {
		return nil, nil
	}
	href := actingAttendee(newInfo, addresses)
	if href == "" {
		href = actingAttendee(oldInfo, addresses)
	}
	if href == "" {
		return nil, nil
	}
	newSt, oldSt := attState(newInfo, href), attState(oldInfo, href)

	type inst struct{ oldStatus, newStatus string }
	byID := map[string]*inst{}
	var order []string
	get := func(id string) *inst {
		if x := byID[id]; x != nil {
			return x
		}
		x := &inst{}
		byID[id] = x
		order = append(order, id)
		return x
	}
	if oldSt != nil {
		for _, id := range oldSt.order {
			get(id).oldStatus = oldSt.instances[id]
		}
	}
	if newSt != nil {
		for _, id := range newSt.order {
			get(id).newStatus = newSt.instances[id]
		}
	} else if newInfo == nil && oldSt != nil {
		// The attendee deleted their whole copy of the event: reply DECLINED
		// for every instance they belonged to.
		for _, id := range oldSt.order {
			get(id).newStatus = "DECLINED"
		}
	}
	// A newly-added EXDATE means the attendee dropped that occurrence: reply
	// DECLINED for it, unless the master itself is declined.
	if mi := byID["master"]; mi == nil || mi.newStatus != "DECLINED" {
		var oldEx []string
		if oldInfo != nil {
			oldEx = oldInfo.exdate
		}
		if newInfo != nil {
			for _, ex := range newInfo.exdate {
				if !containsStr(oldEx, ex) {
					get(ex).newStatus = "DECLINED"
				}
			}
		}
	}

	senderName := ""
	if newSt != nil {
		senderName = newSt.name
	} else if oldSt != nil {
		senderName = oldSt.name
	}

	reply := goical.NewCalendar()
	reply.Props.SetText(goical.PropVersion, "2.0")
	setPlainProp(reply.Props, goical.PropProductID, "-//calserver//calserver//EN")
	setPlainProp(reply.Props, "METHOD", "REPLY")
	if newInfo != nil {
		for _, comp := range newInfo.cal.Children {
			if comp.Name == goical.CompTimezone {
				reply.Children = append(reply.Children, cloneComponent(comp))
			}
		}
	}
	summary := ""
	if s := base.master.Props.Get("SUMMARY"); s != nil {
		summary = s.Value
	}

	hasReply := false
	for _, id := range order {
		in := byID[id]
		if in.oldStatus == in.newStatus {
			continue // no participation change for this instance
		}
		ev := goical.NewComponent(goical.CompEvent)
		setPlainProp(ev.Props, goical.PropUID, base.uid)
		setPlainProp(ev.Props, "SEQUENCE", base.sequence)
		setPlainProp(ev.Props, "DTSTAMP", nowStamp())
		src := instanceOf(newInfo, id)
		if src == nil {
			src = instanceOf(oldInfo, id)
		}
		if src != nil {
			if ds := src.Props.Get("DTSTART"); ds != nil {
				ev.Props["DTSTART"] = []goical.Prop{cloneProp(*ds)}
			}
			if de := src.Props.Get("DTEND"); de != nil {
				ev.Props["DTEND"] = []goical.Prop{cloneProp(*de)}
			} else if du := src.Props.Get("DURATION"); du != nil {
				ev.Props["DURATION"] = []goical.Prop{cloneProp(*du)}
			}
			if s := src.Props.Get("SUMMARY"); s != nil {
				setPlainProp(ev.Props, "SUMMARY", s.Value)
			} else if summary != "" {
				setPlainProp(ev.Props, "SUMMARY", summary)
			}
		} else {
			// Instance known only from EXDATE: synthesize DTSTART from the id.
			dt := goical.NewProp("DTSTART")
			dt.Value = id
			if isDateOnly(id) {
				dt.Params.Set("VALUE", "DATE")
			}
			ev.Props.Set(dt)
			if summary != "" {
				setPlainProp(ev.Props, "SUMMARY", summary)
			}
		}
		if id != "master" {
			rec := goical.NewProp("RECURRENCE-ID")
			rec.Value = id
			if isDateOnly(id) {
				rec.Params.Set("VALUE", "DATE")
			}
			ev.Props.Set(rec)
		}
		org := goical.NewProp("ORGANIZER")
		org.Value = base.organizer
		if base.orgName != "" {
			org.Params.Set("CN", base.orgName)
		}
		ev.Props.Set(org)
		at := goical.NewProp("ATTENDEE")
		at.Value = href
		at.Params.Set("PARTSTAT", in.newStatus)
		if senderName != "" {
			at.Params.Set("CN", senderName)
		}
		ev.Props.Set(at)
		reply.Children = append(reply.Children, ev)
		hasReply = true
	}
	if !hasReply {
		return nil, nil
	}
	return []*Message{{
		UID: base.uid, Method: "REPLY", Component: "VEVENT",
		Sender: href, SenderName: senderName,
		Recipient: base.organizer, RecipientName: base.orgName,
		SignificantChange: true, Calendar: reply,
	}}, nil
}

func actingAttendee(info *eventSummary, addresses map[string]bool) string {
	if info == nil {
		return ""
	}
	for _, href := range info.attOrder {
		if addresses[href] {
			return href
		}
	}
	return ""
}

func attState(info *eventSummary, href string) *attendeeState {
	if info == nil {
		return nil
	}
	return info.perAtt[href]
}

func instanceOf(info *eventSummary, id string) *goical.Component {
	if info == nil {
		return nil
	}
	return info.instances[id]
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// isDateOnly reports whether a recurrence id looks like a DATE (no time part).
func isDateOnly(id string) bool { return len(id) <= 8 && !strings.Contains(id, "T") }

func nowStamp() string { return time.Now().UTC().Format("20060102T150405Z") }

// cloneProp deep-copies a property including its parameters.
func cloneProp(p goical.Prop) goical.Prop {
	cp := goical.Prop{Name: p.Name, Value: p.Value, Params: goical.Params{}}
	for k, vs := range p.Params {
		cp.Params[k] = append([]string(nil), vs...)
	}
	return cp
}

// addExdate adds EXDATE entries (the ids of excluded occurrences) to a master
// event, matching its DTSTART TZID and merging with any existing EXDATE.
func addExdate(ev, master *goical.Component, exclude []string) {
	tzid := ""
	if ds := master.Props.Get("DTSTART"); ds != nil {
		tzid = ds.Params.Get("TZID")
	}
	joined := strings.Join(exclude, ",")
	if existing := ev.Props.Get("EXDATE"); existing != nil {
		merged := cloneProp(*existing)
		merged.Value = merged.Value + "," + joined
		ev.Props["EXDATE"] = []goical.Prop{merged}
		return
	}
	ex := goical.NewProp("EXDATE")
	ex.Value = joined
	if tzid != "" {
		ex.Params.Set("TZID", tzid)
	}
	ev.Props["EXDATE"] = []goical.Prop{*ex}
}

// StripForceSend removes SCHEDULE-FORCE-SEND from every ATTENDEE and ORGANIZER
// in the calendar. The server honors the parameter while generating messages
// (a forced resend), then removes it before storing so it doesn't persist and
// re-fire on the next write (RFC 6638 §7.1).
func StripForceSend(cal *goical.Calendar) {
	if cal == nil {
		return
	}
	for _, comp := range cal.Children {
		for _, p := range comp.Props["ORGANIZER"] {
			p.Params.Del("SCHEDULE-FORCE-SEND")
		}
		for _, p := range comp.Props["ATTENDEE"] {
			p.Params.Del("SCHEDULE-FORCE-SEND")
		}
	}
}

// stripScheduleParams removes server-only scheduling parameters from a message
// event and defaults each attendee's PARTSTAT (so iOS shows an inbox item).
func stripScheduleParams(ev *goical.Component) {
	if org := ev.Props.Get("ORGANIZER"); org != nil {
		org.Params.Del("SCHEDULE-FORCE-SEND")
		org.Params.Del("SCHEDULE-STATUS")
	}
	for _, att := range ev.Props["ATTENDEE"] {
		att.Params.Del("SCHEDULE-FORCE-SEND")
		att.Params.Del("SCHEDULE-STATUS")
		if att.Params.Get("PARTSTAT") == "" {
			att.Params.Set("PARTSTAT", "NEEDS-ACTION")
		}
	}
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

// incSequence returns s+1 — the SEQUENCE a CANCEL carries so it reliably
// supersedes the last REQUEST an attendee received. RFC 5546 treats a
// cancellation as a significant change: bumping SEQUENCE (not just DTSTAMP)
// keeps the CANCEL "newer" even for recipients that collate on SEQUENCE alone.
// This is safe to bump precisely because a CANCEL has no ongoing stored event
// to stay in sync with — unlike a REQUEST, whose SEQUENCE the organizer's own
// client owns and the server only echoes (bumping it here would desync the
// stored event from what attendees hold and break REPLY matching).
func incSequence(s string) string {
	n, _ := strconv.Atoi(s)
	return strconv.Itoa(n + 1)
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
		// SCHEDULE-STATUS comes from the reply's REQUEST-STATUS, defaulting
		// to 2.0, and a reply clears RSVP (Broker::processMessageReply).
		requestStatus := "2.0"
		if p := replyInfo.master.Props.Get("REQUEST-STATUS"); p != nil {
			requestStatus, _, _ = strings.Cut(p.Value, ";")
		}
		// Apply each reply instance's PARTSTAT to the existing VEVENT matched
		// by RECURRENCE-ID (master reply -> master component, override reply ->
		// matching override); a reply for an instance without its own existing
		// component falls back to the master.
		for _, comp := range existing.Children {
			if comp.Name != goical.CompEvent {
				continue
			}
			rid := recurID(comp)
			partStat := ""
			if st := replyInfo.perAtt[replier.href]; st != nil {
				if ps, ok := st.instances[rid]; ok {
					partStat = ps
				} else if ps, ok := st.instances["master"]; ok {
					partStat = ps
				}
			}
			if partStat == "" {
				continue
			}
			atts := comp.Props.Values("ATTENDEE")
			for i := range atts {
				if strings.EqualFold(atts[i].Value, replier.href) {
					atts[i].Params.Set("PARTSTAT", partStat)
					atts[i].Params.Set("SCHEDULE-STATUS", requestStatus)
					atts[i].Params.Del("RSVP")
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
