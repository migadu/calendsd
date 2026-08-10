// events.js — the per-calendar instance cache, fed by server-side expanded
// calendar-queries over the loaded range, plus event-level edits (drag
// moves) and iCal date parsing shared with freebusy/invites.

import * as dav from '../dav.js';
import * as ical from '../ical.js';
import { DAY, HOUR, localDate } from '../time.js';
import { state, emit, myEmail } from './core.js';
import { getObject, putObject, deleteObject } from './objects.js';

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
  const body = dav.xmlDoc('c:calendar-query', '', dav.xml`
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

export function instanceFrom(ev, href, calHref, etag) {
  const dtstart = ical.prop(ev, 'DTSTART');
  if (!dtstart) return null;
  const s = ical.parseICalDate(dtstart.value, dtstart.params);
  if (!s) return null;
  const dtend = ical.prop(ev, 'DTEND');
  const e = dtend ? ical.parseICalDate(dtend.value, dtend.params) : null;
  const durMs = !e ? ical.parseDuration(ical.propValue(ev, 'DURATION')) : null;
  const end = e ? e.date
    : durMs != null ? new Date(+s.date + durMs)
    : new Date(+s.date + (s.allDay ? DAY : HOUR));
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
    // STATUS:CANCELLED — the organizer cancelled; the copy lingers (struck
    // through in the views) until the invitation panel removes it.
    cancelled: (ical.propValue(ev, 'STATUS') || '').toUpperCase() === 'CANCELLED',
  };
}

// setCalendarHidden toggles a calendar off/on in the views. Views go through
// this (not state.hidden directly) so every 'events' subscriber re-renders,
// keeping the one-way state → bus → render flow honest.
export function setCalendarHidden(href, hidden) {
  if (hidden) state.hidden.add(href);
  else state.hidden.delete(href);
  emit('events');
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

// deleteEvent removes an event honoring the recurrence scope: scope 'one'
// excludes just the clicked occurrence with an EXDATE (leaving the rest of the
// series in place), while 'all' (the default, and the only choice for a plain
// event) deletes the whole stored object. Both the editor's Delete and the
// detail popup's Delete share this one path.
export async function deleteEvent(inst, scope = 'all') {
  if (scope === 'one') {
    const { text, etag } = await getObject(inst.href);
    await putObject(inst.href, ical.addExdate(text, ridValue(inst), inst.allDay), etag);
  } else {
    await deleteObject(inst.href);
  }
}

// instanceKey is a stable id for one rendered occurrence, used to correlate a
// grid element with a cache entry (e.g. to flash an event that just changed).
// href identifies the stored object; the start disambiguates the occurrences an
// expanded recurring series produces from the same object.
export function instanceKey(inst) {
  return `${inst.href}#${+inst.start}`;
}

// ridValue renders an instance's RECURRENCE-ID value for EXDATE/overrides.
export function ridValue(inst) {
  if (inst.rid) return inst.rid;
  if (inst.allDay) return ical.dateOnly(localDate(inst.start));
  return ical.utc(inst.start.toISOString());
}
