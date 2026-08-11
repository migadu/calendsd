// calendars.js — the calendar list: loading it (with ctag change detection
// feeding the event cache), colors, and calendar-level CRUD.

import * as dav from '../dav.js';
import { state, emit, check } from './core.js';
import { fetchCalendarEvents } from './events.js';
import { loadProxyFor, calendarHomeOf } from './proxy.js';

const { NS, key } = dav;

// The property set every calendar listing (mine or a delegated home) asks for.
const CAL_PROPS = [
  'd:resourcetype', 'd:displayname', 'a:calendar-color', 'a:calendar-order', 'c:calendar-description',
  'cs:getctag', 's:read-only', 'c:supported-calendar-component-set', 'c:schedule-calendar-transp',
];

// mapCalendar turns one PROPFIND response into a calendar model; `extra` layers
// on delegation fields (delegatedFrom, a forced readOnly).
function mapCalendar(x, extra = {}) {
  const comps = compNames(x.props.get(key(NS.c, 'supported-calendar-component-set')));
  return {
    href: x.href,
    name: decodeURIComponent(x.href.replace(/\/+$/, '').split('/').pop()),
    displayName: x.props.get(key(NS.d, 'displayname'))?.textContent || '',
    color: normColor(x.props.get(key(NS.a, 'calendar-color'))?.textContent),
    // calendar-order (Apple ns) sets the sidebar position; absent sorts last.
    order: parseInt(x.props.get(key(NS.a, 'calendar-order'))?.textContent ?? '', 10),
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
    // A private calendar is marked schedule-calendar-transp:transparent, so
    // its busy times are excluded from free-busy. Absent = opaque (shared).
    private: isTransparent(x.props.get(key(NS.c, 'schedule-calendar-transp'))),
    isCalendar: dav.resourceTypes(x.props.get(key(NS.d, 'resourcetype'))).includes(key(NS.c, 'calendar')),
    ...extra,
  };
}

// Calendars delegated to me (calendar-proxy-*-for), discovered once by
// loadDelegatedCalendars and re-appended on every poll so a ctag refresh of my
// own home doesn't drop them.
let delegated = [];

export async function loadCalendars() {
  const r = check(await dav.propfind(state.calHome, CAL_PROPS, '1'), [207]);
  const before = new Map(state.calendars.map(c => [c.href, c.ctag]));
  const mine = dav.msResponses(r.xml)
    .filter(x => x.href !== state.calHome)
    .map(x => mapCalendar(x))
    .filter(c => c.isCalendar)
    // calendar-order first (unset sorts last), then name — a stable sidebar order.
    .sort((a, b) => (Number.isNaN(a.order) ? Infinity : a.order) - (Number.isNaN(b.order) ? Infinity : b.order)
      || (a.displayName || a.name).localeCompare(b.displayName || b.name));
  state.calendars = [...mine, ...delegated];
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

// loadDelegatedCalendars discovers the calendars others have delegated to me
// (calendar-proxy-write-for → writable, -read-for → read-only) and folds them
// into the sidebar. Best-effort and run once at bootstrap: a delegator we can't
// read is skipped rather than failing the whole list. Their events load on the
// next range fetch; the ctag poll covers only my own home, so a delegated
// calendar refreshes on reload rather than live.
export async function loadDelegatedCalendars() {
  const pf = await loadProxyFor();
  // Dedupe homes across both levels (write wins) and cap the fan-out so a
  // principal proxying many resources doesn't fire an unbounded number of
  // PROPFINDs at login.
  const CAP = 50;
  const seen = new Set();
  const targets = [];
  for (const [access, principals] of [['write', pf.write], ['read', pf.read]]) {
    for (const ph of principals) {
      const { home, owner } = calendarHomeOf(ph);
      if (!home || seen.has(home)) continue;
      seen.add(home);
      targets.push({ home, owner, access });
      if (targets.length >= CAP) break;
    }
    if (targets.length >= CAP) break;
  }
  const lists = await Promise.all(targets.map(async ({ home, owner, access }) => {
    try {
      const r = check(await dav.propfind(home, CAL_PROPS, '1'), [207]);
      return dav.msResponses(r.xml)
        .filter(x => x.href !== home)
        .map(x => mapCalendar(x, { delegatedFrom: owner, readOnly: access === 'read' }))
        .filter(c => c.isCalendar);
    } catch { return []; } // a delegator whose home we can't read — skip
  }));
  delegated = lists.flat();
  state.calendars = [...state.calendars.filter(c => !c.delegatedFrom), ...delegated];
  emit('calendars');
  if (state.range) {
    await Promise.all(delegated.map(fetchCalendarEvents));
    emit('events');
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

// isTransparent reads schedule-calendar-transp: the value is a child element
// (<c:transparent/> or <c:opaque/>), so detect transparency by that child.
function isTransparent(el) {
  return !!el && el.getElementsByTagNameNS(NS.c, 'transparent').length > 0;
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

export async function updateCalendar(href, { displayName, color, private: priv, order, description }) {
  const set = [];
  if (displayName !== undefined) set.push({ name: 'd:displayname', value: displayName });
  if (color !== undefined) set.push({ name: 'a:calendar-color', value: color + 'ff' });
  if (order !== undefined) set.push({ name: 'a:calendar-order', value: String(order) });
  if (description !== undefined) set.push({ name: 'c:calendar-description', value: description });
  // schedule-calendar-transp carries an element value, not text: transparent
  // (private, hidden from free-busy) vs opaque (shared).
  if (priv !== undefined) {
    set.push({ name: 'c:schedule-calendar-transp', xml: priv ? '<c:transparent/>' : '<c:opaque/>' });
  }
  check(await dav.proppatch(href, set), [207]);
  await loadCalendars();
}

// reorderCalendars persists a new sidebar order by writing calendar-order = the
// new index on each of my calendars. Delegated calendars aren't reordered (I
// can't set properties on someone else's calendar); they keep trailing.
export async function reorderCalendars(orderedHrefs) {
  const mine = new Set(state.calendars.filter(c => !c.delegatedFrom).map(c => c.href));
  const list = orderedHrefs.filter(h => mine.has(h));
  // 1-based: the storage backend treats calendar-order 0 as "unset" and omits
  // it, which would drop the first calendar back to name-order.
  await Promise.all(list.map(async (href, i) =>
    check(await dav.proppatch(href, [{ name: 'a:calendar-order', value: String(i + 1) }]), [207])));
  await loadCalendars();
}

export async function deleteCalendar(href) {
  check(await dav.req('DELETE', href), [204]);
  state.instances.delete(href);
  await loadCalendars();
  emit('events');
}
