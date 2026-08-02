// contacts.js — CardDAV: address book + vCard loading (including
// X-ADDRESSBOOKSERVER-KIND:group cards), the contact editor model, and the
// guest-autocomplete directory search.

import * as dav from '../dav.js';
import * as ical from '../ical.js';
import { state, emit, check } from './core.js';
import { getObject, putObject, deleteObject } from './objects.js';
import { rewriteMemberLines } from './dedup.js';

const { NS, key } = dav;

export async function loadContacts() {
  const r = await dav.propfind(state.abHome, ['d:resourcetype', 'd:displayname'], '1');
  if (r.status !== 207) return;
  state.books = dav.msResponses(r.xml)
    .filter(x => x.href !== state.abHome &&
      dav.resourceTypes(x.props.get(key(NS.d, 'resourcetype'))).includes(key(NS.card, 'addressbook')))
    .map(x => ({ href: x.href, displayName: x.props.get(key(NS.d, 'displayname'))?.textContent || 'Contacts' }));
  const contacts = [], groups = [];
  for (const book of state.books) {
    const list = await dav.propfind(book.href, ['d:getetag'], '1');
    if (list.status !== 207) continue;
    const hrefs = dav.msResponses(list.xml).map(x => x.href)
      .filter(h => h !== book.href && !h.endsWith('/'));
    if (!hrefs.length) continue;
    const body = dav.xmlDoc('card:addressbook-multiget', '', dav.xml`
      <d:prop><d:getetag/><card:address-data/></d:prop>
      ${hrefs.map(h => dav.xml`<d:href>${h}</d:href>`)}`);
    const mg = await dav.report(book.href, body, '1');
    for (const resp of dav.msResponses(mg.xml)) {
      const data = resp.props.get(key(NS.card, 'address-data'))?.textContent;
      if (!data) continue;
      const card = ical.components(ical.parse(data), 'VCARD')[0];
      if (!card) continue;
      const model = parseContact(card, resp.href, book.href,
        resp.props.get(key(NS.d, 'getetag'))?.textContent ?? '');
      // A group/list vCard (X-ADDRESSBOOKSERVER-KIND:group) is not a contact —
      // Apple Contacts files it in the sidebar, so we keep them apart.
      (model.kind === 'group' ? groups : contacts).push(model);
    }
  }
  contacts.sort((a, b) => a.fn.localeCompare(b.fn));
  groups.sort((a, b) => a.fn.localeCompare(b.fn));
  state.contacts = contacts;
  state.groups = groups;
  emit('contacts');
}

// normalizeUID strips the urn:uuid: prefix Apple uses on group member refs so a
// group's X-ADDRESSBOOKSERVER-MEMBER values match member cards' UIDs.
export function normalizeUID(uid) {
  return String(uid || '').replace(/^urn:uuid:/i, '');
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
  // Group/list card: X-ADDRESSBOOKSERVER-KIND:group (vCard 3.0) or KIND:group
  // (4.0); its members are UID refs on X-ADDRESSBOOKSERVER-MEMBER / MEMBER.
  const kindVal = (ical.propValue(card, 'X-ADDRESSBOOKSERVER-KIND') ||
    ical.propValue(card, 'KIND')).toLowerCase();
  const isGroup = kindVal === 'group';
  const members = isGroup
    ? card.props.filter(p => p.name === 'X-ADDRESSBOOKSERVER-MEMBER' || p.name === 'MEMBER')
      .map(p => normalizeUID(p.value))
    : [];
  return {
    href, book, etag,
    kind: isGroup ? 'group' : 'individual',
    members,
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

// saveContact writes the editor model (see parseContact). New contacts get a
// UID and land in targetBook (the selected address book) when given, otherwise
// the first/auto-created book; edits overwrite in place with ETag.
export async function saveContact(model, targetBook) {
  const uid = model.uid || crypto.randomUUID();
  const text = ical.buildVCard({ ...model, uid });
  const dir = targetBook || (await ensureBook());
  const target = model.href ?? dir + encodeURIComponent(uid) + '.vcf';
  await putObject(target, text, model.href ? model.etag : null, 'text/vcard; charset=utf-8');
  await loadContacts();
}

export async function deleteContact(href) {
  await deleteObject(href);
  await loadContacts();
}

// createBook makes a new address book (CardDAV MKCOL), deriving a URL-safe,
// collision-free collection name from the display name.
export async function createBook(displayName) {
  const name = displayName.trim() || 'Contacts';
  const base = name.toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/^-+|-+$/g, '') || 'contacts';
  let slug = base;
  for (let n = 2; state.books.some(b => b.href === state.abHome + slug + '/'); n++) slug = `${base}-${n}`;
  const href = state.abHome + slug + '/';
  const body = dav.xmlDoc('d:mkcol', '', dav.xml`<d:set><d:prop>
    <d:resourcetype><d:collection/><card:addressbook/></d:resourcetype>
    <d:displayname>${name}</d:displayname></d:prop></d:set>`);
  check(await dav.req('MKCOL', href, {
    headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
  }), [201]);
  await loadContacts();
  return href;
}

// deleteBook removes an address book and everything in it.
export async function deleteBook(href) {
  await deleteObject(href);
  await loadContacts();
}

// createGroup writes an empty group card (X-ADDRESSBOOKSERVER-KIND:group) into
// targetBook, or the first/auto-created book. Members are added elsewhere.
export async function createGroup(name, targetBook) {
  const uid = crypto.randomUUID();
  const fn = ical.icalEscape((name || '').trim() || 'New Group');
  const dir = targetBook || (await ensureBook());
  const text = ['BEGIN:VCARD', 'VERSION:3.0', `UID:${uid}`, `FN:${fn}`, `N:${fn};;;;`,
    'X-ADDRESSBOOKSERVER-KIND:group', 'END:VCARD'].join('\r\n') + '\r\n';
  await putObject(dir + encodeURIComponent(uid) + '.vcf', text, null, 'text/vcard; charset=utf-8');
  await loadContacts();
}

// deleteGroup removes a group card; its member contacts are unaffected.
export async function deleteGroup(href) {
  await deleteObject(href);
  await loadContacts();
}

function mergeConflict() {
  const e = new Error('A contact changed since you opened this — the list has been reloaded. Please review the duplicates again.');
  e.conflict = true;
  return e;
}

// mergeContacts collapses a duplicate cluster into `survivor` (a merged model
// carrying the survivor card's href/etag/uid) and removes the `losers`
// ([{href, etag, uid}]). Ordering makes a mid-merge failure safe: the survivor
// is written first, then group references are repointed, then losers deleted —
// so at any interruption the book holds either the merged survivor plus
// not-yet-deleted losers (re-running dedup finishes the job) or the untouched
// originals. This idempotency, not an undo, is the safety story.
export async function mergeContacts(survivor, losers) {
  // The preview was built from possibly-stale in-memory models; re-fetch and
  // confirm nothing moved under us before writing (each ETag-guarded step would
  // 412 anyway, but failing early avoids a half-applied merge).
  await loadContacts();
  const byHref = new Map(state.contacts.map(c => [c.href, c]));
  const cur = byHref.get(survivor.href);
  if (!cur || cur.etag !== survivor.etag) throw mergeConflict();
  for (const l of losers) {
    const c = byHref.get(l.href);
    if (!c || c.etag !== l.etag) throw mergeConflict();
  }

  await saveContact(survivor); // in-place overwrite (If-Match survivor.etag)
  await rewriteGroupMembers(losers.map(l => l.uid), survivor.uid);
  for (const l of losers) await deleteObject(l.href, l.etag);
  await loadContacts();
}

// rewriteGroupMembers repoints every group card that lists a merged-away UID at
// the survivor's UID, so a merge never leaves a group with a dangling member.
// saveContact/buildVCard are shaped for person cards (they emit FN/N and treat
// member lines as opaque preserve text), so this is text surgery on the raw
// card followed by an ETag-guarded PUT. The urn:uuid: prefix normalizeUID
// strips is kept on the rewritten line, and any resulting duplicate member
// (the group already listed the survivor too) is collapsed.
async function rewriteGroupMembers(loserUids, survivorUid) {
  const gone = new Set(loserUids.map(normalizeUID));
  for (const g of state.groups) {
    if (!g.members.some(m => gone.has(normalizeUID(m)))) continue;
    const { text, etag } = await getObject(g.href);
    const out = rewriteMemberLines(ical.unfold(text), loserUids, survivorUid);
    await putObject(g.href, ical.fold(out), etag, 'text/vcard; charset=utf-8');
  }
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
    const body = dav.xmlDoc('d:principal-property-search', '', dav.xml`
      <d:property-search><d:prop><d:displayname/></d:prop><d:match>${q}</d:match></d:property-search>
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
