// dedup.js — pure contact-deduplication logic: normalize the fields that
// identify a person, cluster likely duplicates, and preview a merge. Operates
// only on the parsed contact models `parseContact` produces (see
// store/contacts.js) — no DOM, no network, and deliberately no import from the
// rest of store/ (core.js touches localStorage at load, which would break
// importing this under node). That keeps it unit-testable in isolation.
//
// Matching philosophy: a shared email or phone is a *strong* signal and links
// transitively (union-find); a shared name alone is *weak* and is only ever
// reported as its own low-confidence group — never allowed to bridge two strong
// clusters, the classic dedup blow-up (A—email—B—name—C dragging an unrelated C
// into a "high confidence" merge).

import { icalEscape, foldLong } from '../ical.js';

// ---- normalization --------------------------------------------------------

export function normEmail(v) {
  return String(v || '').trim().toLowerCase();
}

// normPhone reduces a dialled number to a comparable key: digits only, compared
// on the last 9 so +41.../0... country-code and trunk-prefix variants of the
// same line still match. Values under 7 digits (extensions, or junk like "0" or
// "-") return '' so they never link unrelated cards.
export function normPhone(v) {
  const digits = String(v || '').replace(/\D/g, '');
  return digits.length >= 7 ? digits.slice(-9) : '';
}

// normName folds a display name to a comparison key: lower-cased, accents
// stripped, punctuation and runs of whitespace collapsed to single spaces.
export function normName(fn, first, last) {
  const base = (fn && fn.trim()) || `${first || ''} ${last || ''}`.trim();
  return base.toLowerCase()
    .normalize('NFD').replace(/[̀-ͯ]/g, '') // strip combining accents
    .replace(/[^a-z0-9]+/g, ' ').trim();
}

const emailKeys = c => (c.emails || []).map(e => normEmail(e.value)).filter(Boolean);
const phoneKeys = c => (c.tels || []).map(t => normPhone(t.value)).filter(Boolean);
// strong keys are namespaced so an email and a phone that happen to share text
// can't collide.
const strongKeys = c => [...emailKeys(c).map(k => 'e:' + k), ...phoneKeys(c).map(k => 'p:' + k)];

// ---- clustering -----------------------------------------------------------

// highClusters unions cards that share any strong key and returns the groups of
// size ≥2 (each an array of the original contact objects).
function highClusters(list) {
  const parent = list.map((_, i) => i);
  const find = i => { while (parent[i] !== i) { parent[i] = parent[parent[i]]; i = parent[i]; } return i; };
  const byKey = new Map();
  list.forEach((c, i) => {
    for (const k of strongKeys(c)) {
      if (byKey.has(k)) parent[find(i)] = find(byKey.get(k));
      else byKey.set(k, i);
    }
  });
  const groups = new Map();
  list.forEach((_, i) => {
    const r = find(i);
    (groups.get(r) || groups.set(r, []).get(r)).push(list[i]);
  });
  return [...groups.values()].filter(g => g.length > 1);
}

// findDuplicatesOf returns the cluster of cards that look like duplicates of one
// contact — the single-card check behind the per-contact affordance — or null.
// A strong (email/phone) overlap wins and reports 'high'; otherwise an exact
// name match reports 'low'. Scoped to the contact's own address book.
export function findDuplicatesOf(contact, contacts) {
  const peers = contacts.filter(c => c !== contact && c.book === contact.book && c.kind !== 'group');
  const keys = new Set(strongKeys(contact));
  const strong = keys.size ? peers.filter(c => strongKeys(c).some(k => keys.has(k))) : [];
  if (strong.length) return { confidence: 'high', contacts: [contact, ...strong] };
  const nm = normName(contact.fn, contact.first, contact.last);
  if (!nm) return null;
  const named = peers.filter(c => normName(c.fn, c.first, c.last) === nm);
  return named.length ? { confidence: 'low', contacts: [contact, ...named] } : null;
}

// findDuplicateClusters is the whole-book sweep behind the Settings action.
// Cards are partitioned by address book first (cross-book merges are a separate,
// opt-in mode), then clustered: strong links transitively, name-only cards that
// have no strong duplicate grouped separately as low-confidence. High clusters
// come first, larger groups before smaller.
export function findDuplicateClusters(contacts) {
  const byBook = new Map();
  for (const c of contacts) {
    if (c.kind === 'group') continue;
    (byBook.get(c.book) || byBook.set(c.book, []).get(c.book)).push(c);
  }
  const clusters = [];
  for (const list of byBook.values()) {
    const high = highClusters(list);
    const stronglyLinked = new Set(high.flat());
    for (const g of high) clusters.push({ confidence: 'high', contacts: g });
    // Name-only grouping runs over the cards with no strong duplicate, so a
    // shared name can never pull a card into (or bridge) a strong cluster.
    const byName = new Map();
    for (const c of list) {
      if (stronglyLinked.has(c)) continue;
      const nm = normName(c.fn, c.first, c.last);
      if (nm) (byName.get(nm) || byName.set(nm, []).get(nm)).push(c);
    }
    for (const g of byName.values()) if (g.length > 1) clusters.push({ confidence: 'low', contacts: g });
  }
  return clusters.sort((a, b) =>
    (a.confidence === b.confidence ? 0 : a.confidence === 'high' ? -1 : 1)
    || b.contacts.length - a.contacts.length);
}

// ---- merge preview --------------------------------------------------------

const telKey = v => { const d = String(v).replace(/\D/g, ''); return d.length >= 7 ? d.slice(-9) : d.toLowerCase(); };

// unionMulti concatenates a repeated field (emails/tels) across cards, dropping
// blanks and later entries whose normalized value was already seen (first-seen
// type wins).
function unionMulti(cards, field, keyer) {
  const seen = new Set(), out = [];
  for (const c of cards) for (const item of (c[field] || [])) {
    const value = (item.value || '').trim();
    if (!value) continue;
    const k = keyer(value) || value.toLowerCase();
    if (seen.has(k)) continue;
    seen.add(k);
    out.push({ value, type: item.type || '' });
  }
  return out;
}

const adrFilled = a => !!(a && (a.street || a.city || a.region || a.postal || a.country));
const adrLine = a => foldLong(`ADR;TYPE=${a.type || 'HOME'}:;;${icalEscape(a.street || '')};` +
  `${icalEscape(a.city || '')};${icalEscape(a.region || '')};${icalEscape(a.postal || '')};${icalEscape(a.country || '')}`);
const sameAdr = (a, b) => adrLine(a) === adrLine(b);

// Properties that may appear only once on a card: on merge the survivor's wins
// and losers' copies are dropped (two PHOTOs, GEOs, ... make an invalid card).
const SINGLE_VALUED = new Set(['PHOTO', 'GEO', 'TZ', 'ANNIVERSARY', 'GENDER', 'REV']);

// mergePreserve unions the pass-through property lines. REV is dropped entirely
// (it rides along in every card's `preserve` since it isn't a MANAGED_VCARD
// property; keeping two would be invalid, and the next write regenerates it).
// Single-valued properties keep the survivor's; everything else dedups by exact
// line so distinct CATEGORIES/X-* all survive without doubling.
function mergePreserve(survivor, losers, extraLines) {
  const seen = new Set(), out = [];
  const add = line => {
    if (!line) return;
    const name = (/^([^;:]+)/.exec(line)?.[1] || '').toUpperCase();
    if (name === 'REV') return;
    const k = SINGLE_VALUED.has(name) ? name : line;
    if (seen.has(k)) return;
    seen.add(k);
    out.push(line);
  };
  for (const line of survivor.preserve || []) add(line);
  for (const l of losers) for (const line of l.preserve || []) add(line);
  for (const line of extraLines) add(line);
  return out;
}

// completeness scores how much a card carries, to auto-pick the survivor: the
// merged card inherits one member's identity (uid/href/etag) and its structured
// address, so keeping the richest card loses the least.
function completeness(c) {
  const scalars = ['org', 'title', 'url', 'bday', 'note'].filter(f => (c[f] || '').trim()).length;
  return (c.emails?.length || 0) + (c.tels?.length || 0) + scalars + (adrFilled(c.adr) ? 1 : 0);
}

// mergePreview computes the single card a cluster would collapse to. Because
// every multi-valued field (emails/phones/addresses) is unioned, the choice of
// survivor is not the user's to make — it only fixes the kept identity, which is
// invisible — so it's picked automatically (the most complete card). The only
// real decisions are fields the cards genuinely disagree on (the name, org,
// …); those are returned in `conflicts` for the UI to offer, defaulting to the
// survivor's value. Nothing is silently dropped: a loser's distinct postal
// address rides along as a preserved ADR line. `survivorHref` is honored when
// given (tests, or a future manual override) but is normally omitted.
export function mergePreview(cluster, survivorHref) {
  const members = cluster.contacts;
  const survivor = (survivorHref && members.find(c => c.href === survivorHref))
    || members.slice().sort((a, b) => completeness(b) - completeness(a))[0];
  const losers = members.filter(c => c !== survivor);
  const ordered = [survivor, ...losers];

  const conflicts = [];
  const scalar = field => {
    const values = ordered.map(c => (c[field] || '').trim()).filter(Boolean);
    const distinct = [...new Set(values)];
    if (distinct.length > 1) conflicts.push({ field, values: distinct });
    // survivor's own value wins by default, else the first non-empty loser's.
    return (survivor[field] || '').trim() || distinct[0] || '';
  };
  // Name is a conflict too when the cards disagree; each distinct display name
  // carries its own N (first/last) so picking one keeps them consistent.
  const nameOptions = [];
  for (const c of ordered) {
    const fn = (c.fn || '').trim();
    if (fn && !nameOptions.some(n => n.fn === fn)) nameOptions.push({ fn, first: c.first || '', last: c.last || '' });
  }
  if (nameOptions.length > 1) conflicts.push({ field: 'fn', values: nameOptions.map(n => n.fn) });

  const org = scalar('org'), title = scalar('title');
  const url = scalar('url'), bday = scalar('bday'), note = scalar('note');

  const adr = adrFilled(survivor.adr) ? survivor.adr
    : (losers.map(l => l.adr).find(adrFilled) || survivor.adr);
  const extraAdr = losers.map(l => l.adr)
    .filter(a => adrFilled(a) && !sameAdr(a, adr)).map(adrLine);

  return {
    href: survivor.href, etag: survivor.etag, uid: survivor.uid,
    kind: 'individual',
    fn: survivor.fn, first: survivor.first, last: survivor.last, nameOptions,
    org, title, url, bday, note, adr,
    emails: unionMulti(ordered, 'emails', normEmail),
    tels: unionMulti(ordered, 'tels', telKey),
    preserve: mergePreserve(survivor, losers, extraAdr),
    conflicts,
    losers: losers.map(l => ({ href: l.href, etag: l.etag, uid: l.uid })),
  };
}

// ---- group membership -----------------------------------------------------

// normalizeMemberUid strips the urn:uuid: prefix Apple puts on group member
// references, matching store/contacts.js's normalizeUID.
export function normalizeMemberUid(uid) {
  return String(uid || '').replace(/^urn:uuid:/i, '');
}

// rewriteMemberLines repoints a group card's member references (unfolded lines)
// from any merged-away UID to the survivor's, keeping each line's urn:uuid:
// prefix style and collapsing a member that becomes a duplicate. Non-member
// lines pass through untouched. Pure text surgery — the store wraps it with the
// fetch + ETag-guarded PUT.
export function rewriteMemberLines(lines, loserUids, survivorUid) {
  const gone = new Set(loserUids.map(normalizeMemberUid));
  const survivorBare = normalizeMemberUid(survivorUid);
  const seen = new Set();
  const out = [];
  for (const line of lines) {
    if (!/^(X-ADDRESSBOOKSERVER-MEMBER|MEMBER)[;:]/i.test(line)) { out.push(line); continue; }
    const colon = line.indexOf(':');
    const raw = line.slice(colon + 1);
    const bare = gone.has(normalizeMemberUid(raw)) ? survivorBare : normalizeMemberUid(raw);
    if (seen.has(bare)) continue; // a member that just became a duplicate
    seen.add(bare);
    const prefix = /^urn:uuid:/i.test(raw) ? 'urn:uuid:' : '';
    out.push(line.slice(0, colon + 1) + prefix + bare);
  }
  return out;
}
