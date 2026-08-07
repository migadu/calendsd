// contacts.js — CardDAV: address book + vCard loading (including
// X-ADDRESSBOOKSERVER-KIND:group cards), the contact editor model, and the
// guest-autocomplete directory search.

import * as dav from '../dav.js';
import * as ical from '../ical.js';
import { state, emit, check } from './core.js';
import { getObject, putObject, deleteObject } from './objects.js';
import { rewriteMemberLines } from './dedup.js';

const { NS, key } = dav;

// LIST_PROPS is the minimum a contact/group *row* needs — name, the primary
// email/phone/org shown as the subtitle, and the KIND/MEMBER properties that
// separate groups from people and drive the sidebar counts. Fetching only these
// (via addressbook-query's property whitelist) skips the bulky PHOTO blobs, so
// the list paints fast; the full cards are pulled in a second pass. UID/VERSION/
// FN are always returned by the server, so they're omitted here.
const LIST_PROPS = ['N', 'EMAIL', 'TEL', 'ORG', 'TITLE',
  'X-ADDRESSBOOKSERVER-KIND', 'KIND', 'X-ADDRESSBOOKSERVER-MEMBER', 'MEMBER'];

// discoverBooks refreshes state.books from the address-book home. Returns false
// when the home itself couldn't be read (nothing to load).
async function discoverBooks() {
  const r = await dav.propfind(state.abHome, ['d:resourcetype', 'd:displayname'], '1');
  if (r.status !== 207) return false;
  state.books = dav.msResponses(r.xml)
    .filter(x => x.href !== state.abHome &&
      dav.resourceTypes(x.props.get(key(NS.d, 'resourcetype'))).includes(key(NS.card, 'addressbook')))
    .map(x => ({ href: x.href, displayName: x.props.get(key(NS.d, 'displayname'))?.textContent || 'Contacts' }));
  return true;
}

// cardModelsFromReport parses the vCards in a multistatus REPORT body into
// {contacts, groups}. When `partial` is set (the light list pass) each model is
// flagged so the editor knows to refetch the whole card before saving — the
// filtered cards omit PHOTO and every other property `preserve` round-trips.
function cardModelsFromReport(xml, book, partial) {
  const contacts = [], groups = [];
  for (const resp of dav.msResponses(xml)) {
    if (resp.href === book.href || resp.href.endsWith('/')) continue;
    const data = resp.props.get(key(NS.card, 'address-data'))?.textContent;
    if (!data) continue;
    const card = ical.components(ical.parse(data), 'VCARD')[0];
    if (!card) continue;
    const model = parseContact(card, resp.href, book.href,
      resp.props.get(key(NS.d, 'getetag'))?.textContent ?? '');
    if (partial) model.partial = true;
    // A group/list vCard (X-ADDRESSBOOKSERVER-KIND:group) is not a contact —
    // Apple Contacts files it in the sidebar, so we keep them apart.
    (model.kind === 'group' ? groups : contacts).push(model);
  }
  return { contacts, groups };
}

// fetchBook pulls one address book's cards in a single addressbook-query REPORT
// (an empty filter matches every card), avoiding the extra PROPFIND the old
// multiget path needed to list hrefs first. `props` restricts the returned
// address-data to a property whitelist (the fast list pass); omit it for whole
// cards.
async function fetchBook(book, props) {
  const dataEl = props
    ? dav.xml`<card:address-data>${props.map(p => dav.xml`<card:prop name="${p}"/>`)}</card:address-data>`
    : dav.xml`<card:address-data/>`;
  const body = dav.xmlDoc('card:addressbook-query', '', dav.xml`
    <d:prop><d:getetag/>${dataEl}</d:prop>
    <card:filter/>`);
  const r = await dav.report(book.href, body, '1');
  if (r.status !== 207) return { contacts: [], groups: [] };
  return cardModelsFromReport(r.xml, book, !!props);
}

// gatherBooks fetches every book in parallel and returns the merged, sorted
// {contacts, groups}. A book that fails contributes nothing rather than aborting
// the whole load.
async function gatherBooks(props) {
  const results = await Promise.all(
    state.books.map(b => fetchBook(b, props).catch(() => ({ contacts: [], groups: [] }))));
  const contacts = results.flatMap(r => r.contacts).sort((a, b) => a.fn.localeCompare(b.fn));
  const groups = results.flatMap(r => r.groups).sort((a, b) => a.fn.localeCompare(b.fn));
  return { contacts, groups };
}

function applyContacts({ contacts, groups }) {
  state.contacts = contacts;
  state.groups = groups;
  emit('contacts');
}

// loadContacts does a full refresh: rediscover the books and pull whole cards.
// It's what every mutation below calls, so state always ends up holding complete
// models (the editor and the dedup sweep rely on that). The two-pass fast path
// used at sign-in is initContacts.
export async function loadContacts() {
  if (!await discoverBooks()) return;
  applyContacts(await gatherBooks(null));
}

// initContacts is the first load after sign-in. It paints a lightweight list as
// soon as the property-filtered pass returns, then fills in the full cards in
// the background. contactsLoaded gates the view's loading spinner; it flips true
// with the first (light) paint so the spinner clears the moment names appear.
export async function initContacts() {
  state.contactsLoaded = false;
  try {
    if (!await discoverBooks()) return;
    const light = await gatherBooks(LIST_PROPS);
    state.contactsLoaded = true;   // clear the spinner as soon as names are up
    applyContacts(light);          // first paint: names + primary email/phone
    applyContacts(await gatherBooks(null)); // background: full cards replace stubs
  } finally {
    // Whatever happened (success, empty home, or a failed fetch), stop showing
    // the loading spinner — a stuck spinner is worse than an empty list.
    if (!state.contactsLoaded) { state.contactsLoaded = true; emit('contacts'); }
  }
}

// ensureFullContact upgrades a partial (list-pass) model to a complete one by
// refetching its card, so opening or editing a contact during the brief window
// before the full pass finishes still sees every field (and doesn't drop
// PHOTO/CATEGORIES/etc. on save). A no-op for already-complete models.
export async function ensureFullContact(c) {
  if (!c?.partial) return c;
  const { text, etag } = await getObject(c.href);
  const card = ical.components(ical.parse(text), 'VCARD')[0];
  if (!card) return c;
  const full = parseContact(card, c.href, c.book, etag);
  const i = state.contacts.indexOf(c);
  if (i >= 0) state.contacts[i] = full;
  return full;
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

// Apple's Contacts.app doesn't use TYPE params — it groups a value with a custom
// label under an item prefix, e.g. `item1.EMAIL:…` paired with
// `item1.X-ABLABEL:_$!<Home>!$_`. propGroup splits "ITEM1.EMAIL" (names are
// upper-cased by the parser) into its group ("ITEM1") and base ("EMAIL"); an
// ungrouped name has an empty group.
function propGroup(name) {
  const m = /^(item\d+)\.(.+)$/i.exec(name);
  return m ? { group: m[1].toUpperCase(), base: m[2].toUpperCase() } : { group: '', base: name };
}

// unwrapAbLabel strips Apple's _$!<Label>!$_ wrapper from an X-ABLABEL value.
function unwrapAbLabel(v) {
  const m = /^_\$!<(.*)>!\$_$/.exec(String(v || ''));
  return m ? m[1] : String(v || '');
}

// abLabelType maps an (unwrapped) Apple X-ABLABEL onto the editor's fixed
// EMAIL/TEL type vocabulary, so a grouped value's custom label still lands as
// Home/Work/Mobile/… Types the field doesn't model (CELL/FAX on an email)
// collapse to OTHER.
function abLabelType(label, isTel) {
  const l = String(label || '').toLowerCase();
  if (!l) return '';
  if (/fax/.test(l)) return isTel ? 'FAX' : 'OTHER';
  if (/mobile|cell|iphone/.test(l)) return isTel ? 'CELL' : 'OTHER';
  if (/home/.test(l)) return 'HOME';
  if (/work|office/.test(l)) return 'WORK';
  return 'OTHER';
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

  // Apple item-groups: index each group's X-ABLABEL so grouped EMAIL/TEL values
  // can be read into the editable fields with a sensible type, and record which
  // groups we consume so their now-modeled value + label lines drop out of
  // `preserve` (leaving them would double-write the address on save).
  const abLabels = new Map();
  const consumed = new Set();
  for (const p of card.props) {
    const { group, base } = propGroup(p.name);
    if (!group) continue;
    if (base === 'X-ABLABEL') abLabels.set(group, unwrapAbLabel(p.value));
    else if (base === 'EMAIL' || base === 'TEL') consumed.add(group);
  }
  // Both plain EMAIL/TEL and Apple's item1.EMAIL/item1.TEL, in document order.
  // A TYPE param wins; otherwise the group's X-ABLABEL supplies the type.
  const multi = base => card.props
    .filter(p => propGroup(p.name).base === base)
    .map(p => ({
      value: p.value,
      type: contactType(p) || abLabelType(abLabels.get(propGroup(p.name).group), base === 'TEL'),
    }));

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
    emails: multi('EMAIL'),
    tels: multi('TEL'),
    adr: {
      type: contactType(adrProp || { params: {} }) || 'HOME',
      street: ical.icalUnescape(adr[2] || ''), city: ical.icalUnescape(adr[3] || ''),
      region: ical.icalUnescape(adr[4] || ''), postal: ical.icalUnescape(adr[5] || ''),
      country: ical.icalUnescape(adr[6] || ''),
    },
    url: ical.propValue(card, 'URL'),
    bday: ical.propValue(card, 'BDAY'),
    note: ical.icalUnescape(ical.propValue(card, 'NOTE')),
    // A displayable <img> src for the contact's photo, derived from PHOTO for
    // the list/detail avatars. PHOTO itself stays in `preserve` below (so save
    // and dedup are unchanged); the editor rewrites that line when it changes.
    photo: ical.photoSrc(ical.prop(card, 'PHOTO')),
    // reconstruct lines for properties the editor doesn't own, so a round-trip
    // through Save keeps PHOTO, CATEGORIES, X-*, etc. Grouped EMAIL/TEL and the
    // X-ABLABEL that names them are now modeled above, so they're excluded here —
    // Save re-emits them as plain EMAIL;TYPE=…/TEL;TYPE=… lines.
    preserve: card.props.filter(p => {
      if (ical.MANAGED_VCARD.has(p.name)) return false;
      const { group, base } = propGroup(p.name);
      if (base === 'EMAIL' || base === 'TEL') return false;
      if (base === 'X-ABLABEL' && consumed.has(group)) return false;
      return true;
    }).map(ical.vcardLine),
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

// putContact writes the editor model (see parseContact) without reloading the
// list. New contacts get a UID and land in targetBook (the selected address
// book) when given, otherwise the first/auto-created book; edits overwrite in
// place with ETag. The bulk importer calls this in a loop and reloads once.
export async function putContact(model, targetBook) {
  const uid = model.uid || crypto.randomUUID();
  const text = ical.buildVCard({ ...model, uid });
  const dir = targetBook || (await ensureBook());
  const target = model.href ?? dir + encodeURIComponent(uid) + '.vcf';
  await putObject(target, text, model.href ? model.etag : null, 'text/vcard; charset=utf-8');
}

// saveContact writes one contact and refreshes the list — the interactive
// editor path.
export async function saveContact(model, targetBook) {
  await putContact(model, targetBook);
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

// renameBook changes an address book's display name (CardDAV PROPPATCH).
export async function renameBook(href, displayName) {
  check(await dav.proppatch(href, [{ name: 'd:displayname', value: displayName.trim() }]), [207]);
  await loadContacts();
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

// renameGroup changes a group's name. A group is a vCard, not a collection, so
// this rewrites its FN in place (text surgery keeps KIND and the member list
// byte-for-byte) rather than a PROPPATCH.
export async function renameGroup(href, displayName) {
  const { text, etag } = await getObject(href);
  const fnLine = ical.foldLong(`FN:${ical.icalEscape(displayName.trim())}`);
  let replaced = false;
  const lines = ical.unfold(text).map(l => {
    if (!/^FN[;:]/i.test(l)) return l;
    replaced = true;
    return fnLine;
  });
  if (!replaced) lines.splice(Math.max(1, lines.length - 1), 0, fnLine); // no FN? insert before END
  await putObject(href, ical.fold(lines), etag, 'text/vcard; charset=utf-8');
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
