// calendars.js — the calendar list: loading it (with ctag change detection
// feeding the event cache), colors, and calendar-level CRUD.

import * as dav from '../dav.js';
import { state, emit, check } from './core.js';
import { fetchCalendarEvents } from './events.js';

const { NS, key } = dav;

export async function loadCalendars() {
  const r = check(await dav.propfind(state.calHome, [
    'd:resourcetype', 'd:displayname', 'a:calendar-color', 'c:calendar-description', 'cs:getctag',
    's:read-only', 'c:supported-calendar-component-set',
  ], '1'), [207]);
  const before = new Map(state.calendars.map(c => [c.href, c.ctag]));
  state.calendars = dav.msResponses(r.xml)
    .filter(x => x.href !== state.calHome)
    .map(x => {
      const comps = compNames(x.props.get(key(NS.c, 'supported-calendar-component-set')));
      return {
        href: x.href,
        name: decodeURIComponent(x.href.replace(/\/+$/, '').split('/').pop()),
        displayName: x.props.get(key(NS.d, 'displayname'))?.textContent || '',
        color: normColor(x.props.get(key(NS.a, 'calendar-color'))?.textContent),
        description: x.props.get(key(NS.c, 'calendar-description'))?.textContent ?? '',
        ctag: x.props.get(key(NS.cs, 'getctag'))?.textContent ?? '',
        // Sabre marks a calendar shared to us read-only ({s}read-only = "1"); the
        // server sends no such property on our own calendars, so absent = writable.
        readOnly: truthyProp(x.props.get(key(NS.s, 'read-only'))),
        // A task/journal-only collection can't hold VEVENTs; an absent set means
        // "all component types" per RFC 4791, so treat empty as event-capable
        // (and likewise task-capable).
        supportsEvents: comps.length === 0 || comps.includes('VEVENT'),
        supportsTasks: comps.length === 0 || comps.includes('VTODO'),
        isCalendar: dav.resourceTypes(x.props.get(key(NS.d, 'resourcetype'))).includes(key(NS.c, 'calendar')),
      };
    })
    .filter(c => c.isCalendar);
  // Default to a calendar that can actually hold new events: writable and
  // VEVENT-capable, so defaultCal never points at a read-only share or a
  // task/journal collection.
  if (!state.prefs.defaultCal || !state.calendars.some(c => c.href === state.prefs.defaultCal && !c.readOnly && c.supportsEvents)) {
    state.prefs.defaultCal = (state.calendars.find(c => !c.readOnly && c.supportsEvents)
      ?? state.calendars.find(c => !c.readOnly) ?? state.calendars[0])?.href ?? '';
  }
  emit('calendars');
  // When a range is loaded, fetch events for calendars not yet cached (first
  // load, or a calendar just created) and for those whose ctag moved (poll).
  if (state.range) {
    const stale = state.calendars.filter(c =>
      !state.instances.has(c.href) || before.get(c.href) !== c.ctag);
    await Promise.all(stale.map(fetchCalendarEvents));
    if (stale.length) emit('events');
  }
}

// compNames lists the VEVENT/VTODO/VJOURNAL names from a
// supported-calendar-component-set property element.
function compNames(el) {
  if (!el) return [];
  return [...el.getElementsByTagNameNS(NS.c, 'comp')]
    .map(c => c.getAttribute('name')).filter(Boolean);
}

// truthyProp reads sabre's "1"/"0" boolean-ish marker properties.
function truthyProp(el) {
  if (!el) return false;
  const v = el.textContent.trim();
  return v !== '' && v !== '0';
}

// eventCalendars returns the calendars a new event may be created on: writable
// and able to hold VEVENTs (excludes read-only shares and task/journal-only
// collections). Used by the event editor and the default-calendar setting.
export function eventCalendars() {
  return state.calendars.filter(c => !c.readOnly && c.supportsEvents);
}

// isReadOnly reports whether the given calendar href is a read-only share, so
// the UI can suppress writes the server would reject with 403.
export function isReadOnly(href) {
  return !!state.calendars.find(c => c.href === href)?.readOnly;
}

// palette used when a calendar has no stored color
const PALETTE = ['#3174ad', '#b3543a', '#3a8a5f', '#7d5ba6', '#b08a2e', '#468189'];
function normColor(v) {
  if (!v) return '';
  const m = /^#([0-9a-f]{6})([0-9a-f]{2})?$/i.exec(v.trim());
  return m ? '#' + m[1] : '';
}
export function calColor(href) {
  const cal = state.calendars.find(c => c.href === href);
  if (cal?.color) return cal.color;
  const i = Math.max(0, state.calendars.findIndex(c => c.href === href));
  return PALETTE[i % PALETTE.length];
}

export async function createCalendar(name, displayName, color) {
  const body = dav.xmlDoc('c:mkcalendar', '', dav.xml`<d:set><d:prop>
    <d:displayname>${displayName}</d:displayname>
    <c:supported-calendar-component-set><c:comp name="VEVENT"/></c:supported-calendar-component-set>
  </d:prop></d:set>`);
  check(await dav.req('MKCALENDAR', state.calHome + encodeURIComponent(name) + '/', {
    headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
  }), [201]);
  if (color) await updateCalendar(state.calHome + encodeURIComponent(name) + '/', { color });
  await loadCalendars();
}

export async function updateCalendar(href, { displayName, color }) {
  const set = [];
  if (displayName !== undefined) set.push({ name: 'd:displayname', value: displayName });
  if (color !== undefined) set.push({ name: 'a:calendar-color', value: color + 'ff' });
  check(await dav.proppatch(href, set), [207]);
  await loadCalendars();
}

export async function deleteCalendar(href) {
  check(await dav.req('DELETE', href), [204]);
  state.instances.delete(href);
  await loadCalendars();
  emit('events');
}
