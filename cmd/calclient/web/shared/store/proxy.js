// proxy.js — calendar delegation via the calendar-proxy-read / -write
// sub-principals. Two directions:
//   • manage who may act on my behalf — the group-member-set of my own
//     calendar-proxy-{read,write} principals (grant/revoke).
//   • discover whose calendars I may act on — my principal's
//     calendar-proxy-{read,write}-for properties; calendars.js turns those into
//     read-only / writable entries in the sidebar.

import * as dav from '../dav.js';
import { state, check } from './core.js';

const { NS, key, xml } = dav;

const proxyHref = access => state.principal + `calendar-proxy-${access}/`;

// principalInfo resolves a principal href to a friendly {href, name, email}.
// Reading another principal's properties needs -principals-readable on the
// server; when it's off (or the PROPFIND otherwise fails) we fall back to the
// last path segment so the roster still shows something recognisable.
export async function principalInfo(href) {
  const fallback = { href, name: decodeURIComponent(href.replace(/\/+$/, '').split('/').pop()), email: '' };
  try {
    const r = await dav.propfind(href, ['d:displayname', 'c:calendar-user-address-set'], '0');
    if (r.status !== 207) return fallback;
    const props = dav.msResponses(r.xml)[0]?.props ?? new Map();
    const email = (dav.hrefTexts(props.get(key(NS.c, 'calendar-user-address-set')))
      .find(a => /^mailto:/i.test(a)) || '').replace(/^mailto:/i, '');
    return { href, name: props.get(key(NS.d, 'displayname'))?.textContent || fallback.name, email };
  } catch { return fallback; }
}

// currentMembers reads the raw member hrefs of a proxy principal.
async function currentMembers(access) {
  try {
    const r = await dav.propfind(proxyHref(access), ['d:group-member-set'], '0');
    return dav.hrefTexts(dav.msResponses(r.xml)[0]?.props.get(key(NS.d, 'group-member-set')));
  } catch { return []; }
}

// setMembers replaces a proxy principal's group-member-set.
async function setMembers(access, hrefs) {
  const items = hrefs.map(h => xml`<d:href>${h}</d:href>`);
  check(await dav.proppatch(proxyHref(access), [{ name: 'd:group-member-set', xml: xml`${items}` }]), [207]);
}

// loadDelegates lists who I've granted proxy access to, resolved to names.
export async function loadDelegates() {
  const out = { read: [], write: [] };
  for (const access of ['read', 'write']) {
    out[access] = await Promise.all((await currentMembers(access)).map(principalInfo));
  }
  return out;
}

// grantDelegate gives a principal read or write proxy over my calendars, moving
// it off the other level first (a delegate is one or the other, not both).
export async function grantDelegate(access, principalHref) {
  const other = access === 'read' ? 'write' : 'read';
  const otherMembers = await currentMembers(other);
  if (otherMembers.includes(principalHref)) await setMembers(other, otherMembers.filter(h => h !== principalHref));
  const members = await currentMembers(access);
  if (!members.includes(principalHref)) await setMembers(access, [...members, principalHref]);
}

// revokeDelegate removes a principal from the given proxy level.
export async function revokeDelegate(access, principalHref) {
  const members = await currentMembers(access);
  if (members.includes(principalHref)) await setMembers(access, members.filter(h => h !== principalHref));
}

// resolvePrincipal maps an email to its principal via a principal-property-search
// on calendar-user-address-set (same directory search the attendee picker uses;
// the server scopes it to the requester's own domain). Returns {href, name,
// email} or null.
export async function resolvePrincipal(email) {
  const needle = email.trim().toLowerCase();
  // Match on sabre's {s}email-address — the searchable index the server exposes
  // (calendar-user-address-set is not searchable). The search returns the
  // principal href even when reading its props is forbidden; the server scopes
  // results to our own email domain.
  const body = dav.xmlDoc('d:principal-property-search', '', dav.xml`
    <d:property-search><d:prop><s:email-address/></d:prop><d:match>${needle}</d:match></d:property-search>
    <d:prop><d:displayname/></d:prop>`);
  const r = await dav.report('/principals/', body, '0');
  if (r.status !== 207) return null;
  const resp = dav.msResponses(r.xml).find(x => /\/principals\//.test(x.href || ''));
  if (!resp) return null;
  return { href: resp.href, name: resp.props.get(key(NS.d, 'displayname'))?.textContent || '', email };
}

// loadProxyFor lists the principals whose calendars I may act on, per access.
export async function loadProxyFor() {
  try {
    const r = await dav.propfind(state.principal, ['cs:calendar-proxy-read-for', 'cs:calendar-proxy-write-for'], '0');
    const props = dav.msResponses(r.xml)[0]?.props ?? new Map();
    return {
      read: dav.hrefTexts(props.get(key(NS.cs, 'calendar-proxy-read-for'))),
      write: dav.hrefTexts(props.get(key(NS.cs, 'calendar-proxy-write-for'))),
    };
  } catch { return { read: [], write: [] }; }
}

// calendarHomeOf maps a delegator's principal to their calendar home. Reading
// their calendar-home-set property needs -principals-readable (a 403 otherwise),
// but a proxy can reach the home collection directly, so derive it from the
// principal path by the /principals/ ⇄ /calendars/ convention and label it with
// the principal's own name.
export function calendarHomeOf(principalHref) {
  return {
    home: principalHref.replace('/principals/', '/calendars/'),
    owner: decodeURIComponent(principalHref.replace(/\/+$/, '').split('/').pop()),
  };
}
