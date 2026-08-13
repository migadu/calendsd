// router.js — the URL layer for calweb. The app is served under /cal/ with a
// server-side SPA fallback (see cmd/calclient/main.go), so real paths describe
// the whole screen and survive a reload:
//
//   /cal/calendar/<view>/<date>   view ∈ day|week|month|agenda, date = YYYY-MM-DD
//   /cal/calendar/agenda?q=term   the agenda's whole-calendar search filter
//   /cal/tasks | /cal/contacts | /cal/settings
//   /cal/event/<uid>[?occ=…]      read-only detail overlay over the calendar
//   /cal/event/<uid>/edit[?occ=…] the editor overlay
//
// This module owns nothing but URLs: it parses the location into a route,
// builds paths, and drives history. It imports no app modules so main.js and
// grid.js can both depend on it without a cycle; they register the render via
// onRoute().

import { localDate } from '/shared/time.js';

const BASE = '/cal/';
const SECTIONS = ['calendar', 'tasks', 'contacts', 'settings'];
const VIEWS = ['day', 'week', 'month', 'agenda'];

let handler = () => {};

// parseRoute turns the current location into a structured route. Unknown or
// malformed paths fall back to the calendar so a stray URL never dead-ends.
export function parseRoute() {
  const p = location.pathname;
  const rest = (p.startsWith(BASE) ? p.slice(BASE.length) : p.replace(/^\/+/, '')).replace(/\/+$/, '');
  const segs = rest ? rest.split('/').map(decodeURIComponent) : [];
  const query = new URLSearchParams(location.search);

  if (segs[0] === 'event') {
    return { name: 'event', uid: segs[1] || '', edit: segs[2] === 'edit', occ: query.get('occ') || '' };
  }
  if (SECTIONS.includes(segs[0]) && segs[0] !== 'calendar') {
    return { name: segs[0] };
  }
  // calendar (the default): optional /<view>/<date> and ?q= filter.
  const view = VIEWS.includes(segs[1]) ? segs[1] : '';
  return { name: 'calendar', view, date: view ? (segs[2] || '') : '', q: query.get('q') || '' };
}

// ---- path builders -------------------------------------------------------

// A Date argument is rendered in the local zone so the segment matches the day
// the user is looking at (localDate = YYYY-MM-DD).
function dateSeg(date) { return date instanceof Date ? localDate(date) : (date || ''); }

// occValue renders an occurrence's local start as YYYY-MM-DDTHH:MM:SS — readable
// in the URL and round-trips through `new Date(occ)` in the local zone (iCal
// times are second-precision, so no millisecond is lost).
export function occValue(date) {
  if (!(date instanceof Date)) return date || '';
  const p2 = n => String(n).padStart(2, '0');
  return `${localDate(date)}T${p2(date.getHours())}:${p2(date.getMinutes())}:${p2(date.getSeconds())}`;
}

export function calendarPath({ view, date, q } = {}) {
  let path = BASE + 'calendar';
  if (view) {
    path += '/' + view;
    const seg = dateSeg(date);
    if (seg) path += '/' + seg;
  }
  if (q) path += '?q=' + encodeURIComponent(q);
  return path;
}

export function sectionPath(name) { return BASE + name; }

export function eventPath(uid, { edit, occ } = {}) {
  let path = BASE + 'event/' + encodeURIComponent(uid);
  if (edit) path += '/edit';
  const v = occ instanceof Date ? occValue(occ) : (occ || '');
  if (v) path += '?occ=' + encodeURIComponent(v);
  return path;
}

// ---- navigation ----------------------------------------------------------

// navigate updates history and re-renders through the one registered handler,
// so the URL stays the single source of truth. pushState/replaceState do not
// fire popstate, so this never re-enters recursively.
export function navigate(path, { replace = false } = {}) {
  if (replace) history.replaceState({}, '', path);
  else history.pushState({}, '', path);
  handler(parseRoute());
}

// replaceUrl swaps the address bar without re-rendering — for state that mutates
// faster than a repaint should follow (the agenda search box on each keystroke)
// and for canonicalizing a bare path on load.
export function replaceUrl(path) { history.replaceState({}, '', path); }

export function onRoute(fn) {
  handler = fn;
  window.addEventListener('popstate', () => handler(parseRoute()));
}

// interceptLinks turns same-origin <a href="/cal/…"> clicks into in-app
// navigation (plain left click only — modified clicks and new-tab targets keep
// their native behavior), so the static nav anchors work without a full reload.
export function interceptLinks() {
  document.addEventListener('click', e => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const a = e.target.closest('a');
    if (!a) return;
    const href = a.getAttribute('href');
    if (!href || !href.startsWith(BASE)) return;
    if (a.target && a.target !== '_self') return;
    e.preventDefault();
    navigate(href);
  });
}
