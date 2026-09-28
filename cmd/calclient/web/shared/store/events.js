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
    room: /^(ROOM|RESOURCE)$/i.test(p.params['CUTYPE'] || ''),
  }));
  // ATTACH: a URI link, or inline BASE64 binary surfaced as a data: URL. Names
  // come from a FILENAME / Apple filename param, else the URL's basename.
  const attachments = ev.props.filter(p => p.name === 'ATTACH').map(p => {
    const name = p.params['FILENAME'] || p.params['X-APPLE-FILENAME'] || '';
    if ((p.params['ENCODING'] || '').toUpperCase() === 'BASE64' || (p.params['VALUE'] || '').toUpperCase() === 'BINARY') {
      return { url: `data:${p.params['FMTTYPE'] || 'application/octet-stream'};base64,${p.value}`, name: name || 'attachment', inline: true };
    }
    return { url: p.value, name: name || p.value.replace(/[?#].*$/, '').replace(/^.*\//, '') || 'link', inline: false };
  });
  return {
    href, calHref, etag,
    uid: ical.propValue(ev, 'UID'),
    rid: ical.propValue(ev, 'RECURRENCE-ID') || '',
    recurring: !!ical.prop(ev, 'RRULE') || !!ical.propValue(ev, 'RECURRENCE-ID'),
    rrule: ical.propValue(ev, 'RRULE'),
    // The zone the stored series recurs in (a TZID, 'UTC', or undefined for
    // floating/DATE) — meaningful on a stored master, not on an expanded
    // instance (the server returns those in UTC).
    zone: ical.seriesZone(dtstart),
    allDay: s.allDay,
    start: s.date, end,
    summary: ical.icalUnescape(ical.propValue(ev, 'SUMMARY')) || '(no title)',
    location: ical.icalUnescape(ical.propValue(ev, 'LOCATION')),
    description: ical.icalUnescape(ical.propValue(ev, 'DESCRIPTION')),
    // Meeting link: the URL property, or a CONFERENCE (RFC 7986) URI.
    url: ical.propValue(ev, 'URL') || ical.propValue(ev, 'CONFERENCE'),
    organizer: ical.propValue(ev, 'ORGANIZER').replace(/^mailto:/i, ''),
    attendees, attachments,
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

// searchEvents runs a whole-calendar title search that ignores the loaded
// agenda window: a calendar-query REPORT per visible calendar with a SUMMARY
// text-match and no time-range, so it spans every event past and future. With
// no time-range there is no server-side <expand>, so a recurring series comes
// back as its stored object; we surface it once, anchored to its next
// occurrence (see atNextOccurrence) rather than its original master start.
// Results are flattened and date-sorted like visibleInstances (declined events
// and hidden calendars dropped). This does not touch state.instances — it is a
// transient query, not the render cache. Being unbounded (no time-range), this
// is the heaviest request in the app, so it only runs on the debounced,
// non-empty agenda search path (see grid.js).
export async function searchEvents(query) {
  const q = query.trim();
  if (!q) return [];
  const now = Date.now();
  const cals = state.calendars.filter(c => !state.hidden.has(c.href));
  const lists = await Promise.all(cals.map(cal => searchCalendar(cal, q)));
  const out = [];
  for (const list of lists) for (const i of list) {
    if (i.myPartstat === 'DECLINED') continue;
    out.push(atNextOccurrence(i, now));
  }
  return out.sort((a, b) => a.start - b.start);
}

// atNextOccurrence anchors a recurring series (returned as its stored master,
// since the search runs no <expand>) to its next occurrence at or after `now`,
// so an old weekly meeting sorts and groups by when it next happens instead of
// its original start years ago. Only plainly steppable rules shift (see
// ical.nextOccurrence); complex rules and non-recurring events pass through
// unchanged. The shifted start keeps the master's time-of-day and duration.
function atNextOccurrence(inst, now) {
  if (!inst.rrule || +inst.start >= now) return inst;
  const next = ical.nextOccurrence(inst.start, inst.rrule, new Date(now), inst.zone);
  if (!next || +next === +inst.start) return inst;
  const dur = +inst.end - +inst.start;
  return { ...inst, start: next, end: new Date(+next + dur) };
}

// searchCalendar: whole-calendar SUMMARY search over one calendar.
function searchCalendar(cal, q) {
  return reportEvents(cal, dav.xml`
    <c:prop-filter name="SUMMARY"><c:text-match collation="i;unicode-casemap">${q}</c:text-match></c:prop-filter>`);
}

// resolveEvent locates a single instance for a URL like /cal/event/<uid>?occ=…,
// so an event overlay can be opened from a cold load or the Back button. It
// tries the render cache first — the common case, since the calendar URL also
// carries the view+date, so a reload reloads the same window and the occurrence
// is already expanded there. Only on a miss does it fetch the stored object by
// UID. Returns null when nothing matches.
export async function resolveEvent(uid, occ) {
  if (!uid) return null;
  const at = occ ? +new Date(occ) : null;
  for (const list of state.instances.values()) {
    for (const i of list) {
      if (i.uid !== uid) continue;
      if (at != null) { if (+i.start === at) return i; }
      else if (!i.rid) return i;   // no occurrence asked for → the master
    }
  }
  // Not loaded: fetch the stored object by UID (no <expand>, so a recurring
  // series comes back as its master). The per-calendar lookups are independent,
  // so fire them in parallel and keep the first hit in calendar order. If an
  // occurrence was requested, shift the master onto that start so the overlay
  // shows the right time. A far-off occurrence therefore opens from the master
  // rather than a server-expanded instance — acceptable given the cache covers
  // the on-screen window.
  const cals = state.calendars.filter(c => !state.hidden.has(c.href));
  const found = (await Promise.all(cals.map(cal => fetchByUid(cal, uid)))).find(Boolean);
  if (!found) return null;
  if (at != null && found.recurring && +found.start !== at) {
    const dur = +found.end - +found.start;
    const start = new Date(at);
    return { ...found, start, end: new Date(at + dur) };
  }
  return found;
}

async function fetchByUid(cal, uid) {
  const rows = await reportEvents(cal, dav.xml`
    <c:prop-filter name="UID"><c:text-match collation="i;octet">${uid}</c:text-match></c:prop-filter>`);
  return rows.find(i => i.uid === uid) || null;   // text-match is substring; confirm exact UID
}

// reportEvents runs a calendar-query REPORT over one calendar with the given
// VEVENT prop-filter (a dav.xml fragment) and no <time-range>, so recurring
// series come back as their stored object. It returns one instance per matching
// object, preferring the master component (no RECURRENCE-ID) so a series shows
// once at its start; a lone override still surfaces itself. Shared by the title
// search and the UID lookup.
async function reportEvents(cal, propFilter) {
  const body = dav.xmlDoc('c:calendar-query', '', dav.xml`
    <d:prop><d:getetag/><c:calendar-data/></d:prop>
    <c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT">
      ${propFilter}
    </c:comp-filter></c:comp-filter></c:filter>`);
  const r = await dav.report(cal.href, body, '1');
  if (r.status !== 207) return [];
  const out = [];
  for (const resp of dav.msResponses(r.xml)) {
    const data = resp.props.get(key(NS.c, 'calendar-data'))?.textContent;
    if (!data) continue;
    const etag = resp.props.get(key(NS.d, 'getetag'))?.textContent ?? '';
    const evs = ical.components(ical.parse(data), 'VEVENT');
    const ev = evs.find(v => !ical.prop(v, 'RECURRENCE-ID')) || evs[0];
    const inst = ev && instanceFrom(ev, resp.href, cal.href, etag);
    if (inst) out.push(inst);
  }
  return out;
}

// moveEvent moves a (non-recurring) event so it starts at `to` instead of
// inst.start — the drag & drop path. DTSTART/DTEND are rewritten in place so
// alarms, overrides, and unmodeled properties survive; each property moves by
// the delta of its own form (ical.shiftDate: wall-clock in a TZID's zone,
// elapsed for UTC, days for DATE), and DTEND by the same move as DTSTART.
// Callers guard against recurring events (moving an occurrence needs the
// editor's scope choice).
export async function moveEvent(inst, to) {
  const { text, etag } = await getObject(inst.href);
  const master = ical.components(ical.parse(text), 'VEVENT')
    .find(v => !ical.prop(v, 'RECURRENCE-ID'));
  const ds = master && ical.prop(master, 'DTSTART');
  const cantShift = new Error('This event stores its dates in a form that cannot be dragged — use Edit instead.');
  const move = { from: inst.start, to };
  const start = ds && ical.shiftDate('DTSTART', ds, move);
  if (!start) throw cantShift;
  const props = { DTSTART: start };
  const de = ical.prop(master, 'DTEND');
  if (de) {
    const end = ical.shiftDate('DTEND', de, move);
    if (!end) throw cantShift;
    props.DTEND = end;
  }
  await putObject(inst.href, ical.setMasterProps(text, props), etag);
  await refreshEvents();
}

// resendInvitations re-sends the invitation to attendees whose delivery failed
// or is stuck, by marking them SCHEDULE-FORCE-SEND=REQUEST and re-PUTting the
// event. A plain re-save wouldn't re-send (no significant change), so this is
// the organizer's recovery path (JOB-QUEUE-PLAN.md D5b). The request carries the
// organizer's own credentials, so the resend stays metered.
export async function resendInvitations(inst, emails) {
  if (!emails || !emails.length) return;
  const { text, etag } = await getObject(inst.href);
  await putObject(inst.href, ical.forceSendAttendees(text, emails), etag);
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
    await putObject(inst.href, ical.addExdateLine(text, ridLine('EXDATE', inst, masterDtstart(text))), etag);
  } else if (scope === 'future') {
    const { text, etag } = await getObject(inst.href);
    const m0 = ical.components(ical.parse(text), 'VEVENT').find(v => !ical.prop(v, 'RECURRENCE-ID'));
    const ds = m0 && ical.prop(m0, 'DTSTART');
    const rrule = m0 ? ical.propValue(m0, 'RRULE') : '';
    const masterStart = ds ? ical.parseICalDate(ds.value, ds.params)?.date : null;
    // Splitting at (or before) the first occurrence just removes the series.
    if (!rrule || !masterStart || +inst.start <= +masterStart) { await deleteObject(inst.href); return; }
    const until = inst.allDay
      ? ical.dateOnly(localDate(new Date(+inst.start - DAY)))
      : ical.utc(new Date(+inst.start - 1000).toISOString());
    let out = ical.setMasterProps(text, { RRULE: `RRULE:${ical.rruleWithUntil(rrule, until)}` });
    out = ical.dropForward(out, inst.start);
    await putObject(inst.href, out, etag);
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

// ridLine renders a RECURRENCE-ID or EXDATE line naming the instance's
// original occurrence in the same form as the master's DTSTART (ds): a zoned
// series gets its zone's wall time. The server returns an expanded instance's
// RECURRENCE-ID in UTC, which names the same instant.
export function ridLine(name, inst, ds) {
  const at = inst.rid ? ical.parseICalDate(inst.rid, {})?.date : inst.start;
  return ical.occurrenceLine(name, at || inst.start, inst.allDay, ds);
}

// masterDtstart is the DTSTART property of a stored object's master VEVENT.
export function masterDtstart(text) {
  const m0 = ical.components(ical.parse(text), 'VEVENT').find(v => !ical.prop(v, 'RECURRENCE-ID'));
  return (m0 && ical.prop(m0, 'DTSTART')) || null;
}
