// Unit tests for the pure contact-deduplication logic in
// web/shared/store/dedup.js — normalization, clustering (strong links vs
// name-only, no bridging, per-book), the merge preview, and the group-member
// rewrite. No DOM or network, so this runs under plain `node --test`.

import test from 'node:test';
import assert from 'node:assert/strict';
import * as dedup from '../web/shared/store/dedup.js';

// mk builds a minimal contact model in the shape parseContact produces.
let seq = 0;
function mk(o = {}) {
  seq++;
  return {
    href: o.href ?? `/book/c${seq}.vcf`, book: o.book ?? '/book/', etag: o.etag ?? `"e${seq}"`,
    kind: o.kind ?? 'individual', uid: o.uid ?? `u${seq}`,
    fn: o.fn ?? '', first: o.first ?? '', last: o.last ?? '',
    org: o.org ?? '', title: o.title ?? '', url: o.url ?? '', bday: o.bday ?? '', note: o.note ?? '',
    emails: o.emails ?? [], tels: o.tels ?? [],
    adr: o.adr ?? { type: 'HOME', street: '', city: '', region: '', postal: '', country: '' },
    preserve: o.preserve ?? [], members: o.members ?? [],
  };
}
const email = v => ({ value: v, type: '' });
const tel = v => ({ value: v, type: '' });

test('normEmail lowercases and trims', () => {
  assert.equal(dedup.normEmail('  Bob@Example.COM '), 'bob@example.com');
  assert.equal(dedup.normEmail(null), '');
});

test('normPhone keeps last 9 digits and rejects short numbers', () => {
  assert.equal(dedup.normPhone('+41 44 123 45 67'), '441234567');
  assert.equal(dedup.normPhone('044 123 45 67'), '441234567'); // country/trunk variant → same key
  assert.equal(dedup.normPhone('123-456'), '');                 // 6 digits: too short
  assert.equal(dedup.normPhone('0'), '');
});

test('normName strips accents, punctuation and case; falls back to first+last', () => {
  assert.equal(dedup.normName('Renée O’Brien', '', ''), 'renee o brien');
  assert.equal(dedup.normName('', 'John', 'Smith'), 'john smith');
  assert.equal(dedup.normName('  ', '', ''), '');
});

test('a shared email links two cards into one high-confidence cluster', () => {
  const a = mk({ fn: 'Bob R', emails: [email('bob@x.org')] });
  const b = mk({ fn: 'Robert', emails: [email('BOB@x.org')], tels: [tel('+1 555 000 1111')] });
  const c = mk({ fn: 'Zoe', emails: [email('zoe@x.org')] });
  const clusters = dedup.findDuplicateClusters([a, b, c]);
  assert.equal(clusters.length, 1);
  assert.equal(clusters[0].confidence, 'high');
  assert.deepEqual(clusters[0].contacts.map(x => x.fn).sort(), ['Bob R', 'Robert']);
});

test('a name-only match is a low pair and never bridges into a high cluster', () => {
  // a & b share a phone (strong). c shares only a NAME with a — it must not be
  // pulled into {a,b}, and since a is already strongly linked, no low pair forms.
  const a = mk({ fn: 'John Smith', tels: [tel('+1 555 111 2222')] });
  const b = mk({ fn: 'J. Smith', tels: [tel('555 111 2222')] });
  const c = mk({ fn: 'John Smith' });
  const clusters = dedup.findDuplicateClusters([a, b, c]);
  assert.equal(clusters.length, 1);
  assert.equal(clusters[0].confidence, 'high');
  assert.deepEqual(clusters[0].contacts.map(x => x.href).sort(), [a.href, b.href].sort());
});

test('two same-name cards with no stronger signal form a low pair', () => {
  const a = mk({ fn: 'Jane Doe' });
  const b = mk({ fn: 'jane  doe' });
  const clusters = dedup.findDuplicateClusters([a, b]);
  assert.equal(clusters.length, 1);
  assert.equal(clusters[0].confidence, 'low');
  assert.equal(clusters[0].contacts.length, 2);
});

test('cards in different address books never match', () => {
  const a = mk({ fn: 'Bob', book: '/personal/', emails: [email('bob@x.org')] });
  const b = mk({ fn: 'Bob', book: '/work/', emails: [email('bob@x.org')] });
  assert.equal(dedup.findDuplicateClusters([a, b]).length, 0);
});

test('high clusters sort before low, and group cards are ignored', () => {
  const h1 = mk({ fn: 'A', emails: [email('a@x.org')] });
  const h2 = mk({ fn: 'A2', emails: [email('a@x.org')] });
  const l1 = mk({ fn: 'Same Name' });
  const l2 = mk({ fn: 'same name' });
  const grp = mk({ fn: 'A', kind: 'group', emails: [email('a@x.org')] });
  const clusters = dedup.findDuplicateClusters([l1, h1, l2, h2, grp]);
  assert.equal(clusters.length, 2);
  assert.equal(clusters[0].confidence, 'high'); // high first regardless of input order
  assert.equal(clusters[1].confidence, 'low');
});

test('findDuplicatesOf returns a high match on a shared phone, null when alone', () => {
  const target = mk({ fn: 'Al', tels: [tel('+41 44 123 45 67')] });
  const match = mk({ fn: 'Alan', tels: [tel('044 123 45 67')] });
  const other = mk({ fn: 'Nobody', emails: [email('n@x.org')] });
  const hit = dedup.findDuplicatesOf(target, [target, match, other]);
  assert.equal(hit.confidence, 'high');
  assert.deepEqual(hit.contacts.map(c => c.fn), ['Al', 'Alan']);
  assert.equal(dedup.findDuplicatesOf(other, [target, match, other]), null);
});

test('mergePreview unions emails/tels and keeps the survivor identity', () => {
  const survivor = mk({ href: '/b/keep.vcf', uid: 'keep', etag: '"k"', fn: 'Bob',
    emails: [email('bob@x.org')], tels: [tel('+1 555 111 2222')] });
  const loser = mk({ href: '/b/drop.vcf', uid: 'drop', etag: '"d"', fn: 'Robert',
    emails: [email('BOB@x.org'), email('rob@work.org')], tels: [tel('555 111 2222')] });
  const cluster = { confidence: 'high', contacts: [survivor, loser] };
  const p = dedup.mergePreview(cluster, '/b/keep.vcf');
  assert.equal(p.uid, 'keep');
  assert.equal(p.href, '/b/keep.vcf');
  assert.equal(p.etag, '"k"');
  assert.deepEqual(p.emails.map(e => e.value), ['bob@x.org', 'rob@work.org']); // deduped
  assert.equal(p.tels.length, 1);                                             // country-code dup collapsed
  assert.deepEqual(p.losers, [{ href: '/b/drop.vcf', etag: '"d"', uid: 'drop' }]);
});

test('mergePreview auto-picks the most complete card as survivor', () => {
  const sparse = mk({ href: '/b/sparse.vcf', uid: 'sparse', fn: 'Bob', emails: [email('bob@x.org')] });
  const rich = mk({ href: '/b/rich.vcf', uid: 'rich', fn: 'Bob',
    emails: [email('bob@x.org')], tels: [tel('+1 555 111 2222')], org: 'Acme' });
  const p = dedup.mergePreview({ contacts: [sparse, rich] }); // no survivorHref → auto
  assert.equal(p.uid, 'rich');
  assert.equal(p.href, '/b/rich.vcf');
  assert.deepEqual(p.losers.map(l => l.uid), ['sparse']);
});

test('mergePreview surfaces the display name as a conflict, carrying each N', () => {
  const a = mk({ href: '/b/a.vcf', fn: 'Bob Smith', first: 'Bob', last: 'Smith', emails: [email('b@x.org')] });
  const b = mk({ href: '/b/b.vcf', fn: 'Robert Smith', first: 'Robert', last: 'Smith', emails: [email('b@x.org')] });
  const p = dedup.mergePreview({ contacts: [a, b] }, '/b/a.vcf');
  const nameConflict = p.conflicts.find(c => c.field === 'fn');
  assert.deepEqual(nameConflict.values.sort(), ['Bob Smith', 'Robert Smith']);
  assert.equal(p.nameOptions.find(o => o.fn === 'Robert Smith').first, 'Robert');
});

test('mergePreview reports no name conflict when the display names agree', () => {
  const a = mk({ href: '/b/a.vcf', fn: 'Bob', emails: [email('b@x.org')] });
  const b = mk({ href: '/b/b.vcf', fn: 'Bob', emails: [email('b@x.org')] });
  const p = dedup.mergePreview({ contacts: [a, b] }, '/b/a.vcf');
  assert.ok(!p.conflicts.some(c => c.field === 'fn'));
});

test('mergePreview reports scalar conflicts and defaults to the survivor value', () => {
  const survivor = mk({ href: '/b/a.vcf', fn: 'Bob', org: 'Acme', title: '' });
  const loser = mk({ href: '/b/b.vcf', fn: 'Bob', org: 'Globex', title: 'CTO' });
  const p = dedup.mergePreview({ contacts: [survivor, loser] }, '/b/a.vcf');
  const orgConflict = p.conflicts.find(c => c.field === 'org');
  assert.deepEqual(orgConflict.values.sort(), ['Acme', 'Globex']);
  assert.equal(p.org, 'Acme');            // survivor wins by default
  assert.equal(p.title, 'CTO');           // survivor empty → loser fills, no conflict
  assert.ok(!p.conflicts.some(c => c.field === 'title'));
});

test('mergePreview dedups preserve lines: no doubled REV or PHOTO', () => {
  const survivor = mk({ href: '/b/a.vcf', fn: 'Bob',
    preserve: ['REV:20260101T000000Z', 'PHOTO;VALUE=URI:https://a/keep.jpg', 'CATEGORIES:friends'] });
  const loser = mk({ href: '/b/b.vcf', fn: 'Bob',
    preserve: ['REV:20260202T000000Z', 'PHOTO;VALUE=URI:https://a/drop.jpg', 'CATEGORIES:work'] });
  const p = dedup.mergePreview({ contacts: [survivor, loser] }, '/b/a.vcf');
  assert.equal(p.preserve.filter(l => l.startsWith('REV')).length, 0);       // REV dropped entirely
  const photos = p.preserve.filter(l => l.startsWith('PHOTO'));
  assert.deepEqual(photos, ['PHOTO;VALUE=URI:https://a/keep.jpg']);          // survivor's single-valued kept
  assert.deepEqual(p.preserve.filter(l => l.startsWith('CATEGORIES')),       // multi-valued: both kept
    ['CATEGORIES:friends', 'CATEGORIES:work']);
});

test('mergePreview keeps the survivor address and carries a loser one as ADR', () => {
  const survivor = mk({ href: '/b/a.vcf', fn: 'Bob',
    adr: { type: 'HOME', street: '1 Main', city: 'Bern', region: '', postal: '3000', country: 'CH' } });
  const loser = mk({ href: '/b/b.vcf', fn: 'Bob',
    adr: { type: 'WORK', street: '9 Loop', city: 'Zug', region: '', postal: '6300', country: 'CH' } });
  const p = dedup.mergePreview({ contacts: [survivor, loser] }, '/b/a.vcf');
  assert.equal(p.adr.city, 'Bern');                                          // survivor's kept structured
  const adrLines = p.preserve.filter(l => l.startsWith('ADR'));
  assert.equal(adrLines.length, 1);
  assert.match(adrLines[0], /9 Loop/);                                       // loser's carried, not dropped
});

test('rewriteMemberLines repoints losers to the survivor, keeps urn:uuid:, collapses dups', () => {
  const lines = [
    'BEGIN:VCARD',
    'X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:loser1',
    'X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:survivor',
    'X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:keepme',
    'FN:Team',
    'END:VCARD',
  ];
  const out = dedup.rewriteMemberLines(lines, ['loser1'], 'survivor');
  const members = out.filter(l => l.startsWith('X-ADDRESSBOOKSERVER-MEMBER'));
  // loser1 → survivor, which was already a member: collapsed to one survivor line.
  assert.deepEqual(members, [
    'X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:survivor',
    'X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:keepme',
  ]);
  assert.ok(out.includes('FN:Team'));       // unrelated lines untouched
  assert.ok(out.includes('BEGIN:VCARD'));
});

test('rewriteMemberLines handles a plain UID member without adding a prefix', () => {
  const out = dedup.rewriteMemberLines(['MEMBER:loser1'], ['loser1'], 'survivor');
  assert.deepEqual(out, ['MEMBER:survivor']);
});
