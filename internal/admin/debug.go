package admin

// Phase B data handlers (ADMIN-PLAN.md §3.6, §3.7): scheduling inbox,
// sync-collection preview, and the doctor.

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	goical "github.com/emersion/go-ical"

	"calserver/internal/caldav"
	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
	"calserver/internal/storage"
)

// --- Scheduling debugger (§3.6) ---

// AttendeeView is one parsed ATTENDEE line of an inbox message.
type AttendeeView struct {
	Address        string `json:"address"`
	Name           string `json:"name"`
	PartStat       string `json:"partStat"`
	ScheduleStatus string `json:"scheduleStatus"`
}

// InboxItem is one schedulingobjects row with its iTIP essentials parsed.
type InboxItem struct {
	URI        string         `json:"uri"`
	Method     string         `json:"method"`
	UID        string         `json:"uid"`
	Summary    string         `json:"summary"`
	Organizer  string         `json:"organizer"`
	Attendees  []AttendeeView `json:"attendees"`
	ParseError string         `json:"parseError"`
	Data       string         `json:"data"`
}

type SchedulingData struct {
	Principal   string `json:"principal"`
	IMipEnabled bool   `json:"imipEnabled"`
	IMipSender  string `json:"imipSender"`
	// InboxUnavailable is set when no scheduling backend is wired (mock
	// deployments); the page says so instead of showing an empty inbox.
	InboxUnavailable bool        `json:"inboxUnavailable"`
	Inbox            []InboxItem `json:"inbox"`
	// PendingInvites lists this principal's unanswered share invitations
	// (share_invitestatus = 1); the shared schema stores no notification
	// objects, so calendarinstances is the source of truth.
	PendingInvites []storage.AdminCalendarInstance `json:"pendingInvites"`
}

func (a *App) schedulingData(r *http.Request) (any, error) {
	principal := r.URL.Query().Get("principal")
	if principal == "" {
		return nil, fmt.Errorf("%w: missing ?principal=principals/<name>", errBadRequest)
	}
	d := &SchedulingData{
		Principal:   principal,
		IMipEnabled: a.Config.IMipSMTP != "",
		IMipSender:  a.Config.IMipSender,
	}
	if a.Scheduling == nil {
		d.InboxUnavailable = true
	} else {
		objects, err := a.Scheduling.SchedulingObjects(r.Context(), principal)
		if err != nil {
			return nil, err
		}
		for _, o := range objects {
			d.Inbox = append(d.Inbox, parseInboxItem(o))
		}
	}
	invites, err := a.Store.PendingShareInvites(r.Context(), principal)
	if err != nil {
		return nil, err
	}
	d.PendingInvites = invites
	return d, nil
}

func parseInboxItem(o caldav.SchedulingObjectInfo) InboxItem {
	item := InboxItem{URI: o.URI, Data: string(o.CalendarData)}
	cal, err := ical.Decode(o.CalendarData)
	if err != nil {
		item.ParseError = err.Error()
		return item
	}
	if p := cal.Props.Get("METHOD"); p != nil {
		item.Method = p.Value
	}
	seen := map[string]bool{}
	for _, comp := range cal.Children {
		if comp.Name == goical.CompTimezone {
			continue
		}
		if p := comp.Props.Get(goical.PropUID); p != nil && item.UID == "" {
			item.UID = p.Value
		}
		if p := comp.Props.Get("SUMMARY"); p != nil && item.Summary == "" {
			item.Summary = p.Value
		}
		if p := comp.Props.Get("ORGANIZER"); p != nil && item.Organizer == "" {
			item.Organizer = p.Value
		}
		for _, att := range comp.Props.Values("ATTENDEE") {
			if seen[att.Value] {
				continue
			}
			seen[att.Value] = true
			partStat := att.Params.Get("PARTSTAT")
			if partStat == "" {
				partStat = "NEEDS-ACTION"
			}
			item.Attendees = append(item.Attendees, AttendeeView{
				Address:        att.Value,
				Name:           att.Params.Get("CN"),
				PartStat:       partStat,
				ScheduleStatus: att.Params.Get("SCHEDULE-STATUS"),
			})
		}
	}
	return item
}

// --- Sync debugger (§3.6) ---

type SyncData struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Token string `json:"token"`
	// Log is the collection's change-log timeline, newest first.
	Log []storage.AdminChange `json:"log"`
	// Preview is what sync-collection would return for Token; nil with
	// PreviewNote set when the token is unknown (client gets a full
	// resync) or the collection is gone.
	Preview     *dav.ChangeSet `json:"preview"`
	PreviewNote string         `json:"previewNote"`
}

const syncTokenPrefix = "http://sabre.io/ns/sync/"

func (a *App) syncData(r *http.Request) (any, error) {
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind == "" {
		kind = "calendar"
	}
	if kind != "calendar" && kind != "addressbook" {
		return nil, fmt.Errorf("%w: kind must be calendar or addressbook", errBadRequest)
	}
	id := q.Get("id")
	if id == "" {
		return nil, fmt.Errorf("%w: missing ?id=", errBadRequest)
	}
	limit := 100
	if lim := q.Get("limit"); lim != "" {
		var err error
		if limit, err = strconv.Atoi(lim); err != nil {
			return nil, fmt.Errorf("%w: limit: %v", errBadRequest, err)
		}
	}
	d := &SyncData{Kind: kind, ID: id, Token: q.Get("token")}
	var err error
	if d.Log, err = a.Store.ChangeLog(r.Context(), kind, id, limit); err != nil {
		return nil, err
	}
	// The sync REPORT carries the prefixed token; the backends take the
	// bare counter. Accept either here.
	token := strings.TrimPrefix(d.Token, syncTokenPrefix)
	if kind == "calendar" {
		d.Preview, err = a.Calendars.ChangesForCalendar(r.Context(),
			caldav.CalendarID{Calendar: id}, token, 1, 500)
	} else {
		d.Preview, err = a.AddressBooks.ChangesForAddressBook(r.Context(), id, token, 1, 500)
	}
	if err != nil {
		return nil, err
	}
	if d.Preview == nil {
		d.PreviewNote = "no sync data for this token — a client presenting it would be told to do a full resync (invalid sync token / 403 valid-sync-token)"
	}
	return d, nil
}

// --- Doctor (§3.7) ---

type DoctorData struct {
	Report *storage.AdminDoctorReport `json:"report"`
	// DeletableChecks lists the present findings the delete-orphans action
	// can clean up (§3.8).
	DeletableChecks []string `json:"deletableChecks"`
}

func (a *App) doctorData(r *http.Request) (any, error) {
	scanCap := 0
	if s := r.URL.Query().Get("scancap"); s != "" {
		var err error
		if scanCap, err = strconv.Atoi(s); err != nil {
			return nil, fmt.Errorf("%w: scancap: %v", errBadRequest, err)
		}
	}
	report, err := a.Store.Doctor(r.Context(), scanCap)
	if err != nil {
		return nil, err
	}
	d := &DoctorData{Report: report}
	present := map[string]bool{}
	for _, f := range report.Findings {
		present[f.Check] = true
	}
	for _, check := range storage.DeletableOrphanChecks() {
		if present[check] {
			d.DeletableChecks = append(d.DeletableChecks, check)
		}
	}
	return d, nil
}
