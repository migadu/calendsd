// events.js — the per-calendar instance cache, fed by server-side expanded
// calendar-queries over the loaded range, plus event-level edits (drag
// moves) and iCal date parsing shared with freebusy/invites.

import * as dav from '../dav.js';
import * as ical from '../ical.js';
import { state, emit, myEmail } from './core.js';
import { getObject, putObject } from './objects.js';

const { NS, key } = dav;

export async function setRange(start, end) {
  state.range = { start, end };
  await Promise.all(state.calendars.map(fetchCalendarEvents));
  emit('events');
}

export async function fetchCalendarEvents(cal) {
  if (!state.range) return;
  const s = ical.utc(state.range.start.toISOString());
  const e = ical.utc(state.range.end.toISOString());
  const body = dav.xmlDoc('c:calendar-query', '', `
    <d:prop><d:getetag/><c:calendar-data><c:expand start="${s}" end="${e}"/></c:calendar-data></d:prop>
    <c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT">
      <c:time-range start="${s}" end="${e}"/>
    </c:comp-filter></c:comp-filter></c:filter>`);
  const r = await dav.report(cal.href, body, '1');
  if (r.status !== 207) return; // keep old data on transient errors
  const out = [];
  for (const resp of dav.msResponses(r.xml)) {
    const data = resp.props.get(key(NS.c, 'calendar-data'))?.textContent;
    if (!data) continue;
    const etag = resp.props.get(key(NS.d, 'getetag'))?.textContent ?? '';
    for (const ev of ical.components(ical.parse(data), 'VEVENT')) {
      const inst = instanceFrom(ev, resp.href, cal.href, etag);
      if (inst) out.push(inst);
    }
  }
  out.sort((a, b) => a.start - b.start);
  state.instances.set(cal.href, out);
}

export function parseICalDate(v, params = {}) {
  if (params.VALUE === 'DATE' || /^\d{8}$/.test(v)) {
    return { allDay: true, date: new Date(+v.slice(0, 4), +v.slice(4, 6) - 1, +v.slice(6, 8)) };
  }
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})(Z?)$/.exec(v);
  if (!m) return null;
  return { allDay: false, date: new Date(`${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}${m[7]}`) };
}

export function instanceFrom(ev, href, calHref, etag) {
  const dtstart = ical.prop(ev, 'DTSTART');
  if (!dtstart) return null;
  const s = parseICalDate(dtstart.value, dtstart.params);
  if (!s) return null;
  const dtend = ical.prop(ev, 'DTEND');
  const e = dtend ? parseICalDate(dtend.value, dtend.params) : null;
  const durMs = !e ? ical.parseDuration(ical.propValue(ev, 'DURATION')) : null;
  const end = e ? e.date
    : durMs != null ? new Date(+s.date + durMs)
    : new Date(+s.date + (s.allDay ? 864e5 : 36e5));
  const me = myEmail().toLowerCase();
  const attendees = ev.props.filter(p => p.name === 'ATTENDEE').map(p => ({
    email: p.value.replace(/^mailto:/i, ''),
    partstat: p.params['PARTSTAT'] || 'NEEDS-ACTION',
    scheduleStatus: p.params['SCHEDULE-STATUS'] || '',
  }));
  return {
    href, calHref, etag,
    uid: ical.propValue(ev, 'UID'),
    rid: ical.propValue(ev, 'RECURRENCE-ID') || '',
    recurring: !!ical.prop(ev, 'RRULE') || !!ical.propValue(ev, 'RECURRENCE-ID'),
    rrule: ical.propValue(ev, 'RRULE'),
    allDay: s.allDay,
    start: s.date, end,
    summary: ical.icalUnescape(ical.propValue(ev, 'SUMMARY')) || '(no title)',
    location: ical.icalUnescape(ical.propValue(ev, 'LOCATION')),
    description: ical.icalUnescape(ical.propValue(ev, 'DESCRIPTION')),
    // Meeting link: the URL property, or a CONFERENCE (RFC 7986) URI.
    url: ical.propValue(ev, 'URL') || ical.propValue(ev, 'CONFERENCE'),
    organizer: ical.propValue(ev, 'ORGANIZER').replace(/^mailto:/i, ''),
    attendees,
    myPartstat: attendees.find(a => a.email.toLowerCase() === me)?.partstat ?? '',
    // CLASS:CONFIDENTIAL — read-only sharees see only a busy block for this event.
    confidential: (ical.propValue(ev, 'CLASS') || '').toUpperCase() === 'CONFIDENTIAL',
  };
}

// visibleInstances flattens the cache for enabled calendars, hiding events
// this user has declined.
export function visibleInstances() {
  const out = [];
  for (const [calHref, list] of state.instances) {
    if (state.hidden.has(calHref)) continue;
    if (!state.calendars.some(c => c.href === calHref)) continue;
    for (const i of list) if (i.myPartstat !== 'DECLINED') out.push(i);
  }
  return out.sort((a, b) => a.start - b.start);
}

export async function refreshEvents() {
  if (!state.range) return;
  await Promise.all(state.calendars.map(fetchCalendarEvents));
  emit('events');
}

// moveEvent shifts a (non-recurring) event by deltaMs — the drag & drop path.
// DTSTART/DTEND are rewritten in place so alarms, overrides, and unmodeled
// properties survive; callers guard against recurring events (moving an
// occurrence needs the editor's scope choice).
export async function moveEvent(inst, deltaMs) {
  const { text, etag } = await getObject(inst.href);
  const master = ical.components(ical.parse(text), 'VEVENT')
    .find(v => !ical.prop(v, 'RECURRENCE-ID'));
  const ds = master && ical.prop(master, 'DTSTART');
  const cantShift = new Error('This event stores its dates in a form that cannot be dragged — use Edit instead.');
  const start = ds && ical.shiftDate('DTSTART', ds, deltaMs);
  if (!start) throw cantShift;
  const props = { DTSTART: start };
  const de = ical.prop(master, 'DTEND');
  if (de) {
    const end = ical.shiftDate('DTEND', de, deltaMs);
    if (!end) throw cantShift;
    props.DTEND = end;
  }
  await putObject(inst.href, ical.setMasterProps(text, props), etag);
  await refreshEvents();
}

export function eventHref(calHref, uid) {
  return calHref + encodeURIComponent(uid) + '.ics';
}

// ridValue renders an instance's RECURRENCE-ID value for EXDATE/overrides.
export function ridValue(inst) {
  if (inst.rid) return inst.rid;
  if (inst.allDay) {
    const p = n => String(n).padStart(2, '0');
    return `${inst.start.getFullYear()}${p(inst.start.getMonth() + 1)}${p(inst.start.getDate())}`;
  }
  return ical.utc(inst.start.toISOString());
}
