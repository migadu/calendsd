// store.js — calweb's data layer: session, discovery, a per-calendar
// instance cache fed by server-side expanded calendar-queries, ctag polling,
// and ETag-checked writes. Views subscribe via `bus` and re-render.

import * as dav from './dav.js';
import * as ical from './ical.js';

const { NS, key } = dav;

export const state = {
  user: '', principal: '', displayName: '',
  calHome: '', abHome: '', inbox: '', outbox: '', addresses: [],
  calendars: [],           // {href,name,displayName,color,ctag,description}
  hidden: new Set(),       // calendar hrefs toggled off in the sidebar
  range: null,             // {start: Date, end: Date} currently loaded
  instances: new Map(),    // calHref -> [instance]
  invites: [],             // parsed inbox items
  books: [], contacts: [], // carddav
  prefs: loadPrefs(),
};

export const bus = new EventTarget();
const emit = type => bus.dispatchEvent(new Event(type));

// ---- preferences ---------------------------------------------------------

function loadPrefs() {
  // tz: IANA name for display, '' = the device's own time zone.
  const defaults = { weekStart: 1, hour12: false, tz: '', defaultCal: '', staySignedIn: false };
  try {
    return { ...defaults, ...JSON.parse(localStorage.getItem('calweb.prefs') || '{}') };
  } catch { return { ...defaults }; }
}
export function savePrefs(patch) {
  Object.assign(state.prefs, patch);
  localStorage.setItem('calweb.prefs', JSON.stringify(state.prefs));
  emit('prefs');
}

// ---- errors --------------------------------------------------------------

export class DavError extends Error {
  constructor(res, fallback) {
    super(friendly(res.status, fallback));
    this.status = res.status;
    this.conflict = res.status === 412 || res.status === 409;
    this.detail = res.text?.slice(0, 500) ?? '';
  }
}
export function friendly(status, fallback) {
  switch (status) {
    case 401: return 'Wrong username or password.';
    case 403: return "You don't have permission to do that.";
    case 404: return "That item doesn't exist any more.";
    case 409: case 412: return 'This item was changed elsewhere — your view has been refreshed. Please re-apply your edit.';
    case 415: return "That file doesn't look like a calendar or contact file.";
    case 502: case 503: return "Can't reach the calendar server.";
    default: return fallback || 'Something went wrong (HTTP ' + status + ').';
  }
}
function check(res, okStatuses = [200, 201, 204, 207]) {
  if (!okStatuses.includes(res.status)) throw new DavError(res);
  return res;
}

// ---- session -------------------------------------------------------------

export function haveSession() { return !!dav.session.auth; }

export async function login(user, pass, stay) {
  dav.session.login(user, pass);
  try {
    await discover();
  } catch (e) {
    dav.session.logout();
    throw e;
  }
  if (stay) {
    localStorage.setItem('calweb.user', sessionStorage.getItem('calclient.user'));
    localStorage.setItem('calweb.auth', sessionStorage.getItem('calclient.auth'));
  }
  savePrefs({ staySignedIn: !!stay });
  await bootstrap();
}

export function logout() {
  dav.session.logout();
  localStorage.removeItem('calweb.user');
  localStorage.removeItem('calweb.auth');
  stopPolling();
  emit('session');
}

// restore restores a saved session (sessionStorage first, then "stay signed
// in" localStorage) and bootstraps. Returns false if there is none / stale.
export async function restore() {
  if (!dav.session.auth && localStorage.getItem('calweb.auth')) {
    sessionStorage.setItem('calclient.user', localStorage.getItem('calweb.user'));
    sessionStorage.setItem('calclient.auth', localStorage.getItem('calweb.auth'));
  }
  if (!dav.session.auth) return false;
  try {
    await discover();
    await bootstrap();
    return true;
  } catch {
    dav.session.logout();
    return false;
  }
}

async function discover() {
  const r1 = check(await dav.propfind('/', ['d:current-user-principal']), [207]);
  state.principal = dav.hrefText(dav.msResponses(r1.xml)[0]?.props.get(key(NS.d, 'current-user-principal')));
  if (!state.principal) throw new DavError({ status: 401 });
  const r2 = check(await dav.propfind(state.principal, [
    'd:displayname', 'c:calendar-home-set', 'card:addressbook-home-set',
    'c:schedule-inbox-URL', 'c:schedule-outbox-URL', 'c:calendar-user-address-set',
  ]), [207]);
  const p = dav.msResponses(r2.xml)[0]?.props ?? new Map();
  state.user = dav.session.user;
  state.displayName = p.get(key(NS.d, 'displayname'))?.textContent ?? dav.session.user;
  state.calHome = dav.hrefText(p.get(key(NS.c, 'calendar-home-set')));
  state.abHome = dav.hrefText(p.get(key(NS.card, 'addressbook-home-set')));
  state.inbox = dav.hrefText(p.get(key(NS.c, 'schedule-inbox-URL')));
  state.outbox = dav.hrefText(p.get(key(NS.c, 'schedule-outbox-URL')));
  state.addresses = dav.hrefTexts(p.get(key(NS.c, 'calendar-user-address-set')));
  emit('session');
}

export function myEmail() {
  const m = state.addresses.find(a => a.startsWith('mailto:'));
  return m ? m.slice(7) : '';
}

async function bootstrap() {
  await loadCalendars();
  loadInvites().catch(() => {});
  loadContacts().catch(() => {});
  startPolling();
}

// ---- calendars -----------------------------------------------------------

export async function loadCalendars() {
  const r = check(await dav.propfind(state.calHome, [
    'd:resourcetype', 'd:displayname', 'a:calendar-color', 'c:calendar-description', 'cs:getctag',
  ], '1'), [207]);
  const before = new Map(state.calendars.map(c => [c.href, c.ctag]));
  state.calendars = dav.msResponses(r.xml)
    .filter(x => x.href !== state.calHome)
    .map(x => ({
      href: x.href,
      name: decodeURIComponent(x.href.replace(/\/+$/, '').split('/').pop()),
      displayName: x.props.get(key(NS.d, 'displayname'))?.textContent || '',
      color: normColor(x.props.get(key(NS.a, 'calendar-color'))?.textContent),
      description: x.props.get(key(NS.c, 'calendar-description'))?.textContent ?? '',
      ctag: x.props.get(key(NS.cs, 'getctag'))?.textContent ?? '',
      isCalendar: dav.resourceTypes(x.props.get(key(NS.d, 'resourcetype'))).includes(key(NS.c, 'calendar')),
    }))
    .filter(c => c.isCalendar);
  if (!state.prefs.defaultCal || !state.calendars.some(c => c.href === state.prefs.defaultCal)) {
    state.prefs.defaultCal = state.calendars[0]?.href ?? '';
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
  const body = dav.xmlDoc('c:mkcalendar', '', `<d:set><d:prop>
    <d:displayname>${dav.esc(displayName)}</d:displayname>
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

// ---- events (expanded instances) ----------------------------------------

export async function setRange(start, end) {
  state.range = { start, end };
  await Promise.all(state.calendars.map(fetchCalendarEvents));
  emit('events');
}

async function fetchCalendarEvents(cal) {
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

function parseICalDate(v, params = {}) {
  if (params.VALUE === 'DATE' || /^\d{8}$/.test(v)) {
    return { allDay: true, date: new Date(+v.slice(0, 4), +v.slice(4, 6) - 1, +v.slice(6, 8)) };
  }
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})(Z?)$/.exec(v);
  if (!m) return null;
  return { allDay: false, date: new Date(`${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}${m[7]}`) };
}

function instanceFrom(ev, href, calHref, etag) {
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

// ---- object-level operations --------------------------------------------

export async function getObject(href) {
  const r = check(await dav.req('GET', href), [200]);
  return { text: r.text, etag: r.headers.get('ETag') ?? '' };
}

export async function putObject(href, text, etag, contentType = 'text/calendar; charset=utf-8') {
  const headers = { 'Content-Type': contentType };
  if (etag) headers['If-Match'] = etag;
  else if (etag === null) headers['If-None-Match'] = '*';
  return check(await dav.req('PUT', href, { headers, body: text }), [201, 204]);
}

export async function deleteObject(href) {
  check(await dav.req('DELETE', href), [204]);
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

// ---- free-busy -----------------------------------------------------------

// freeBusy probes availability via the outbox; returns Map(email -> [{start,end}]).
export async function freeBusy(emails, start, end) {
  const body = ical.buildFreeBusyRequest({
    organizer: myEmail(), attendees: emails,
    start: start.toISOString(), end: end.toISOString(),
  });
  const r = check(await dav.req('POST', state.outbox, {
    headers: { 'Content-Type': 'text/calendar; charset=utf-8' }, body,
  }), [200]);
  const out = new Map(emails.map(e => [e, []]));
  if (!r.xml) return out;
  for (const resp of [...r.xml.getElementsByTagNameNS(NS.c, 'response')]) {
    const who = resp.getElementsByTagNameNS(NS.d, 'href')[0]?.textContent
      ?.replace(/^mailto:/i, '').toLowerCase();
    const data = resp.getElementsByTagNameNS(NS.c, 'calendar-data')[0]?.textContent ?? '';
    const busy = [];
    for (const line of ical.unfold(data)) {
      const m = /^FREEBUSY[^:]*:(.*)$/i.exec(line);
      if (!m) continue;
      for (const period of m[1].split(',')) {
        const [ps, pe] = period.split('/');
        const s = parseICalDate(ps, {});
        if (!s) continue;
        // A period end is either an explicit datetime or a DURATION
        // (RFC 5545 §3.3.9); a dropped duration would falsely read as "free".
        let end = null;
        if (/^[+-]?P/i.test(pe || '')) {
          const dur = ical.parseDuration(pe);
          if (dur != null) end = new Date(+s.date + dur);
        } else {
          const e = parseICalDate(pe, {});
          if (e) end = e.date;
        }
        if (end) busy.push({ start: s.date, end });
      }
    }
    const match = emails.find(e => e.toLowerCase() === who);
    if (match) out.set(match, busy);
  }
  return out;
}

// ---- invitations (schedule inbox) ---------------------------------------

export async function loadInvites() {
  const r = await dav.propfind(state.inbox, ['d:getetag'], '1');
  if (r.status !== 207) return;
  const hrefs = dav.msResponses(r.xml).map(x => x.href)
    .filter(h => h !== state.inbox && !h.endsWith('/'));
  const items = [];
  for (const href of hrefs) {
    try {
      const { text, etag } = await getObject(href);
      const tree = ical.parse(text);
      const ev = ical.components(tree, 'VEVENT')[0];
      const method = (tree.children[0] ? ical.propValue(tree.children[0], 'METHOD') : '') || 'REQUEST';
      const inst = ev ? instanceFrom(ev, href, '', etag) : null;
      items.push({ href, etag, text, method, ...(inst ?? { summary: '(scheduling message)' }) });
    } catch { /* skip unreadable items */ }
  }
  state.invites = items;
  emit('invites');
}

// answerInvite: ACCEPTED | TENTATIVE | DECLINED. Storing our copy with the
// new PARTSTAT makes the server deliver the iTIP REPLY to the organizer;
// then the inbox item is removed.
export async function answerInvite(invite, partstat) {
  const text = ical.setPartstat(invite.text, myEmail(), partstat);
  // Unconditional PUT (no If-Match / If-None-Match) is an intentional upsert:
  // the href is derived from the invite UID and lives in the user's own default
  // calendar, so the only object it can hit is this same event — and a
  // re-sent REQUEST (a reschedule) must be allowed to update the stored copy,
  // which If-None-Match:* would reject.
  await putObject(eventHref(state.prefs.defaultCal, invite.uid), text, undefined);
  await deleteObject(invite.href).catch(() => {});
  await Promise.all([loadInvites(), refreshEvents()]);
}

export async function dismissInvite(invite) {
  await deleteObject(invite.href).catch(() => {});
  await loadInvites();
}

// ---- contacts ------------------------------------------------------------

export async function loadContacts() {
  const r = await dav.propfind(state.abHome, ['d:resourcetype', 'd:displayname'], '1');
  if (r.status !== 207) return;
  state.books = dav.msResponses(r.xml)
    .filter(x => x.href !== state.abHome &&
      dav.resourceTypes(x.props.get(key(NS.d, 'resourcetype'))).includes(key(NS.card, 'addressbook')))
    .map(x => ({ href: x.href, displayName: x.props.get(key(NS.d, 'displayname'))?.textContent || 'Contacts' }));
  const contacts = [];
  for (const book of state.books) {
    const list = await dav.propfind(book.href, ['d:getetag'], '1');
    if (list.status !== 207) continue;
    const hrefs = dav.msResponses(list.xml).map(x => x.href)
      .filter(h => h !== book.href && !h.endsWith('/'));
    if (!hrefs.length) continue;
    const body = dav.xmlDoc('card:addressbook-multiget', '',
      `<d:prop><d:getetag/><card:address-data/></d:prop>` +
      hrefs.map(h => `<d:href>${dav.esc(h)}</d:href>`).join(''));
    const mg = await dav.report(book.href, body, '1');
    for (const resp of dav.msResponses(mg.xml)) {
      const data = resp.props.get(key(NS.card, 'address-data'))?.textContent;
      if (!data) continue;
      const card = ical.components(ical.parse(data), 'VCARD')[0];
      if (!card) continue;
      contacts.push(parseContact(card, resp.href, book.href,
        resp.props.get(key(NS.d, 'getetag'))?.textContent ?? ''));
    }
  }
  contacts.sort((a, b) => a.fn.localeCompare(b.fn));
  state.contacts = contacts;
  emit('contacts');
}

// human type params on EMAIL/TEL, minus the boilerplate ones.
function contactType(prop) {
  const raw = (prop.params.TYPE || '').split(',').map(s => s.toUpperCase())
    .filter(t => t && !['INTERNET', 'VOICE', 'PREF'].includes(t));
  return raw[0] || '';
}

// parseContact turns a parsed VCARD into the editor model, keeping the emails/
// phones/org/title/address/etc. and preserving any properties we don't model.
function parseContact(card, href, book, etag) {
  const n = ical.vcardValue(ical.propValue(card, 'N')); // Family;Given;...
  const adrProp = ical.prop(card, 'ADR');
  const adr = adrProp ? ical.vcardValue(adrProp.value) : [];
  return {
    href, book, etag,
    uid: ical.propValue(card, 'UID'),
    fn: ical.icalUnescape(ical.propValue(card, 'FN')),
    last: ical.icalUnescape(n[0] || ''),
    first: ical.icalUnescape(n[1] || ''),
    org: ical.icalUnescape(ical.propValue(card, 'ORG')),
    title: ical.icalUnescape(ical.propValue(card, 'TITLE')),
    emails: card.props.filter(p => p.name === 'EMAIL')
      .map(p => ({ value: p.value, type: contactType(p) })),
    tels: card.props.filter(p => p.name === 'TEL')
      .map(p => ({ value: p.value, type: contactType(p) })),
    adr: {
      type: contactType(adrProp || { params: {} }) || 'HOME',
      street: ical.icalUnescape(adr[2] || ''), city: ical.icalUnescape(adr[3] || ''),
      region: ical.icalUnescape(adr[4] || ''), postal: ical.icalUnescape(adr[5] || ''),
      country: ical.icalUnescape(adr[6] || ''),
    },
    url: ical.propValue(card, 'URL'),
    bday: ical.propValue(card, 'BDAY'),
    note: ical.icalUnescape(ical.propValue(card, 'NOTE')),
    // reconstruct lines for properties the editor doesn't own, so a round-trip
    // through Save keeps PHOTO, CATEGORIES, X-*, etc.
    preserve: card.props.filter(p => !ical.MANAGED_VCARD.has(p.name)).map(ical.vcardLine),
  };
}

async function ensureBook() {
  if (state.books.length) return state.books[0].href;
  const href = state.abHome + 'contacts/';
  const body = dav.xmlDoc('d:mkcol', '', `<d:set><d:prop>
    <d:resourcetype><d:collection/><card:addressbook/></d:resourcetype>
    <d:displayname>Contacts</d:displayname></d:prop></d:set>`);
  check(await dav.req('MKCOL', href, {
    headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
  }), [201]);
  state.books.push({ href, displayName: 'Contacts' });
  return href;
}

// saveContact accepts the editor model (see parseContact). New contacts get a
// UID and land in the first addressbook; edits overwrite in place with ETag.
export async function saveContact(model) {
  const uid = model.uid || crypto.randomUUID();
  const text = ical.buildVCard({ ...model, uid });
  const target = model.href ?? (await ensureBook()) + encodeURIComponent(uid) + '.vcf';
  await putObject(target, text, model.href ? model.etag : null, 'text/vcard; charset=utf-8');
  await loadContacts();
}

export async function deleteContact(href) {
  await deleteObject(href);
  await loadContacts();
}

// searchDirectory merges cached contacts and a principal search for the
// guest autocomplete. Returns [{name, email}].
export async function searchDirectory(q) {
  q = q.trim().toLowerCase();
  if (!q) return [];
  const seen = new Set();
  const out = [];
  for (const c of state.contacts) {
    const primary = c.emails[0]?.value;
    if (!primary) continue;
    if (c.fn.toLowerCase().includes(q) || c.emails.some(e => e.value.toLowerCase().includes(q))) {
      if (!seen.has(primary)) { seen.add(primary); out.push({ name: c.fn, email: primary }); }
    }
  }
  try {
    const body = dav.xmlDoc('d:principal-property-search', '', `
      <d:property-search><d:prop><d:displayname/></d:prop><d:match>${dav.esc(q)}</d:match></d:property-search>
      <d:prop><d:displayname/><c:calendar-user-address-set/></d:prop>`);
    const r = await dav.report('/principals/', body, '0');
    for (const resp of dav.msResponses(r.xml)) {
      const name = resp.props.get(key(NS.d, 'displayname'))?.textContent ?? '';
      const email = dav.hrefTexts(resp.props.get(key(NS.c, 'calendar-user-address-set')))
        .find(a => a.startsWith('mailto:'))?.slice(7);
      if (email && !seen.has(email)) { seen.add(email); out.push({ name, email }); }
    }
  } catch { /* directory search is best-effort */ }
  return out.slice(0, 8);
}

// ---- polling -------------------------------------------------------------

let pollTimer = null;
export function startPolling() {
  stopPolling();
  pollTimer = setInterval(() => {
    loadCalendars().catch(() => {});
    loadInvites().catch(() => {});
  }, 30_000);
}
export function stopPolling() {
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = null;
}
