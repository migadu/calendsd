// Unit tests for the pure iCalendar/vCard logic in web/shared/ical.js —
// the round-trip and text-surgery helpers whose edge cases (folding, params,
// date forms, master-vs-override) regress silently without coverage.

import test from 'node:test';
import assert from 'node:assert/strict';
import * as ical from '../web/shared/ical.js';

test('icalEscape / icalUnescape round-trip', () => {
  const raw = 'a,b;c\\d\nnext line';
  assert.equal(ical.icalEscape(raw), 'a\\,b\\;c\\\\d\\nnext line');
  assert.equal(ical.icalUnescape(ical.icalEscape(raw)), raw);
});

test('utc renders iCal basic UTC form', () => {
  assert.equal(ical.utc('2026-07-15T10:30:00Z'), '20260715T103000Z');
  assert.match(ical.utc('2026-07-15T10:30'), /^\d{8}T\d{6}Z$/);
});

test('dateOnly strips hyphens', () => {
  assert.equal(ical.dateOnly('2026-07-15'), '20260715');
});

test('parseICalDate handles DATE, floating and UTC DATE-TIME', () => {
  assert.deepEqual(ical.parseICalDate('20260715', {}),
    { allDay: true, date: new Date(2026, 6, 15) });
  assert.deepEqual(ical.parseICalDate('20260715T103000', { VALUE: 'DATE' }),
    { allDay: true, date: new Date(2026, 6, 15) });
  const floating = ical.parseICalDate('20260715T103000');
  assert.equal(floating.allDay, false);
  assert.equal(+floating.date, +new Date(2026, 6, 15, 10, 30));
  const utc = ical.parseICalDate('20260715T103000Z');
  assert.equal(+utc.date, Date.UTC(2026, 6, 15, 10, 30));
  assert.equal(ical.parseICalDate('garbage'), null);
});

test('icalToLocal converts DATE-TIME to a datetime-local value', () => {
  assert.equal(ical.icalToLocal('20260715T103000'), '2026-07-15T10:30');
  assert.match(ical.icalToLocal('20260715T103000Z'), /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/);
  assert.equal(ical.icalToLocal('20260715'), '');   // DATE-only has no time to offer
  assert.equal(ical.icalToLocal('garbage'), '');
  assert.equal(ical.icalToLocal(''), '');
});

test('parseDuration handles RFC 5545 forms', () => {
  assert.equal(ical.parseDuration('PT1H30M'), 90 * 60000);
  assert.equal(ical.parseDuration('P1D'), 864e5);
  assert.equal(ical.parseDuration('P1W'), 7 * 864e5);
  assert.equal(ical.parseDuration('-PT15M'), -15 * 60000);
  assert.equal(ical.parseDuration('bogus'), null);
});

test('foldLong wraps to 75 octets and unfold reassembles', () => {
  const line = 'DESCRIPTION:' + 'x'.repeat(200);
  const folded = ical.foldLong(line);
  for (const part of folded.split('\r\n')) assert.ok(part.length <= 75);
  assert.deepEqual(ical.unfold(folded), [line]);
});

const SAMPLE = [
  'BEGIN:VCALENDAR',
  'VERSION:2.0',
  'BEGIN:VEVENT',
  'UID:abc',
  'DTSTART:20260715T100000Z',
  'DTEND:20260715T110000Z',
  'SUMMARY:Team sync',
  'LOCATION:HQ',
  'ATTENDEE;PARTSTAT=NEEDS-ACTION;CN="Smith; Bob":mailto:bob@example.org',
  'BEGIN:VALARM',
  'ACTION:DISPLAY',
  'DESCRIPTION:reminder',
  'END:VALARM',
  'END:VEVENT',
  'END:VCALENDAR',
].join('\r\n') + '\r\n';

test('parse builds a component tree with params (quoted ; kept)', () => {
  const tree = ical.parse(SAMPLE);
  const ev = ical.components(tree, 'VEVENT')[0];
  assert.equal(ical.propValue(ev, 'SUMMARY'), 'Team sync');
  const att = ical.prop(ev, 'ATTENDEE');
  assert.equal(att.params.PARTSTAT, 'NEEDS-ACTION');
  assert.equal(att.params.CN, 'Smith; Bob');
  assert.equal(att.value, 'mailto:bob@example.org');
  assert.equal(ical.components(tree, 'VALARM').length, 1);
});

test('setPartstat rewrites the matching attendee and strips METHOD', () => {
  const withMethod = SAMPLE.replace('VERSION:2.0', 'VERSION:2.0\r\nMETHOD:REQUEST');
  const out = ical.setPartstat(withMethod, 'bob@example.org', 'ACCEPTED');
  assert.match(out, /ATTENDEE;PARTSTAT=ACCEPTED/);
  assert.doesNotMatch(out, /METHOD/);
});

test('addExdate inserts into the master VEVENT', () => {
  const out = ical.addExdate(SAMPLE, '20260716T100000Z', false);
  const lines = ical.unfold(out);
  const i = lines.indexOf('EXDATE:20260716T100000Z');
  assert.ok(i > 0, 'EXDATE present');
  assert.ok(i < lines.indexOf('END:VEVENT'), 'inside the VEVENT');
  const allDay = ical.addExdate(SAMPLE, '20260716', true);
  assert.match(allDay, /EXDATE;VALUE=DATE:20260716/);
});

test('addOverride appends and replaces an override with the same RECURRENCE-ID', () => {
  const over = ['UID:abc', 'RECURRENCE-ID:20260716T100000Z', 'SUMMARY:Moved once'];
  const once = ical.addOverride(SAMPLE, over);
  assert.match(once, /SUMMARY:Moved once/);
  const twice = ical.addOverride(once, ['UID:abc', 'RECURRENCE-ID:20260716T100000Z', 'SUMMARY:Moved again']);
  assert.doesNotMatch(twice, /Moved once/);
  assert.match(twice, /Moved again/);
  assert.equal(ical.unfold(twice).filter(l => l === 'BEGIN:VEVENT').length, 2);
});

test('setMasterProps replaces, inserts, removes — and spares the VALARM', () => {
  const out = ical.setMasterProps(SAMPLE, {
    SUMMARY: 'SUMMARY:Renamed',
    LOCATION: null,                                     // remove
    DESCRIPTION: 'DESCRIPTION:added later',             // insert
    ATTENDEE: ['ATTENDEE:mailto:a@x.org', 'ATTENDEE:mailto:b@x.org'],
  });
  assert.match(out, /SUMMARY:Renamed/);
  assert.doesNotMatch(out, /LOCATION/);
  assert.match(out, /DESCRIPTION:added later/);
  assert.equal(ical.unfold(out).filter(l => l.startsWith('ATTENDEE')).length, 2);
  // the VALARM's DESCRIPTION is a different property at depth 2 — untouched
  assert.match(out, /DESCRIPTION:reminder/);
});

test('eventLines emits a DISPLAY VALARM for a reminder trigger', () => {
  const lines = ical.eventLines({
    uid: 'x', summary: 'Call, Bob', start: '2026-07-15T10:00:00Z',
    end: '2026-07-15T11:00:00Z', alarm: '-PT15M',
  });
  const i = lines.indexOf('BEGIN:VALARM');
  assert.ok(i > 0, 'VALARM present');
  assert.deepEqual(lines.slice(i, i + 5), [
    'BEGIN:VALARM', 'ACTION:DISPLAY', 'DESCRIPTION:Call\\, Bob',
    'TRIGGER;RELATED=START:-PT15M', 'END:VALARM',
  ]);
  // no alarm field → no VALARM
  assert.ok(!ical.eventLines({
    uid: 'x', summary: 's', start: '2026-07-15T10:00:00Z', end: '2026-07-15T11:00:00Z',
  }).includes('BEGIN:VALARM'));
});

test('alarmPreset reads a simple reminder and flags the rest as CUSTOM', () => {
  const ev = t => ical.components(ical.parse(
    `BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\r\n${t}END:VEVENT\r\nEND:VCALENDAR\r\n`), 'VEVENT')[0];
  assert.equal(ical.alarmPreset(ev('')), '');                                   // no alarm
  assert.equal(ical.alarmPreset(ev(
    'BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER;RELATED=START:-PT15M\r\nEND:VALARM\r\n')), '-PT15M');
  assert.equal(ical.alarmPreset(ev(                                            // -PT60M ≡ -PT1H preset
    'BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT60M\r\nEND:VALARM\r\n')), '-PT1H');
  assert.equal(ical.alarmPreset(ev(                                            // at-start preset
    'BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:PT0S\r\nEND:VALARM\r\n')), 'PT0S');
  assert.equal(ical.alarmPreset(ev(                                            // EMAIL action → custom
    'BEGIN:VALARM\r\nACTION:EMAIL\r\nTRIGGER:-PT15M\r\nEND:VALARM\r\n')), 'CUSTOM');
  assert.equal(ical.alarmPreset(ev(                                            // odd offset → custom
    'BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT10M\r\nEND:VALARM\r\n')), 'CUSTOM');
  assert.equal(ical.alarmPreset(ev(                                            // relative to END → custom
    'BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER;RELATED=END:-PT15M\r\nEND:VALARM\r\n')), 'CUSTOM');
  assert.equal(ical.alarmPreset(ev(                                            // absolute trigger → custom
    'BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER;VALUE=DATE-TIME:20260715T094500Z\r\nEND:VALARM\r\n')), 'CUSTOM');
  assert.equal(ical.alarmPreset(ev(                                            // several alarms → custom
    'BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT15M\r\nEND:VALARM\r\n' +
    'BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT5M\r\nEND:VALARM\r\n')), 'CUSTOM');
});

test('setMasterAlarm replaces alarms on the master and can clear them', () => {
  const set = ical.setMasterAlarm(SAMPLE, '-PT30M', 'Team sync');
  assert.equal(ical.unfold(set).filter(l => l === 'BEGIN:VALARM').length, 1);
  assert.match(set, /TRIGGER;RELATED=START:-PT30M/);
  assert.doesNotMatch(set, /DESCRIPTION:reminder/);          // the old VALARM is gone
  assert.match(set, /SUMMARY:Team sync/);                    // top-level props untouched
  // empty trigger clears every alarm
  assert.ok(!ical.setMasterAlarm(SAMPLE, '').includes('BEGIN:VALARM'));
});

test('setMasterAlarm leaves an override VEVENT alone', () => {
  const withOverride = ical.addOverride(SAMPLE,
    ['UID:abc', 'RECURRENCE-ID:20260716T100000Z', 'SUMMARY:Override',
     'BEGIN:VALARM', 'ACTION:DISPLAY', 'DESCRIPTION:o', 'TRIGGER:-PT5M', 'END:VALARM']);
  const out = ical.setMasterAlarm(withOverride, '');
  assert.match(out, /DESCRIPTION:o/);                        // override's alarm survives
  assert.equal(ical.unfold(out).filter(l => l === 'BEGIN:VALARM').length, 1);
});

test('setMasterProps only edits the master, not an override VEVENT', () => {
  const withOverride = ical.addOverride(SAMPLE,
    ['UID:abc', 'RECURRENCE-ID:20260716T100000Z', 'SUMMARY:Override title']);
  const out = ical.setMasterProps(withOverride, { SUMMARY: 'SUMMARY:New master' });
  assert.match(out, /SUMMARY:New master/);
  assert.match(out, /SUMMARY:Override title/);
});

test('shiftDate shifts UTC and DATE forms, refuses TZID and floating', () => {
  assert.equal(
    ical.shiftDate('DTSTART', { value: '20260715T100000Z', params: {} }, 36e5),
    'DTSTART:20260715T110000Z');
  assert.equal(
    ical.shiftDate('DTSTART', { value: '20260715', params: { VALUE: 'DATE' } }, 2 * 864e5),
    'DTSTART;VALUE=DATE:20260717');
  assert.equal(ical.shiftDate('DTSTART', { value: '20260715T100000', params: { TZID: 'Europe/Zurich' } }, 36e5), null);
  assert.equal(ical.shiftDate('DTSTART', { value: '20260715T100000', params: {} }, 36e5), null);
});

test('buildEvent renders CRLF iCal with DATE values for all-day', () => {
  const out = ical.buildEvent({
    uid: 'u1', summary: 'Holiday', allDay: true,
    start: '2026-07-15', end: '2026-07-16',
  });
  assert.match(out, /\r\n/);
  assert.match(out, /DTSTART;VALUE=DATE:20260715/);
  assert.match(out, /DTEND;VALUE=DATE:20260716/);
  assert.match(out, /BEGIN:VEVENT[\s\S]*END:VEVENT/);
});

test('parseRRulePreset maps simple rules and rejects complex ones', () => {
  assert.deepEqual(ical.parseRRulePreset(''), { freq: '', interval: 1, byday: [], ends: 'never' });
  assert.deepEqual(ical.parseRRulePreset('FREQ=WEEKLY;COUNT=5'),
    { freq: 'WEEKLY', interval: 1, byday: [], ends: 'count', count: '5', until: '' });
  assert.deepEqual(ical.parseRRulePreset('FREQ=DAILY;UNTIL=20260801T235959Z'),
    { freq: 'DAILY', interval: 1, byday: [], ends: 'until', count: '10', until: '2026-08-01' });
  assert.equal(ical.parseRRulePreset('FREQ=MONTHLY;BYSETPOS=2'), null); // still a complex rule
  assert.equal(ical.parseRRulePreset('FREQ=WEEKLY;BYMONTH=3'), null);
});

test('buildRRule renders the preset back', () => {
  assert.equal(ical.buildRRule({ freq: '' }), '');
  assert.equal(ical.buildRRule({ freq: 'DAILY', ends: 'never' }), 'FREQ=DAILY');
  assert.equal(ical.buildRRule({ freq: 'WEEKLY', ends: 'count', count: '5' }), 'FREQ=WEEKLY;COUNT=5');
  assert.equal(ical.buildRRule({ freq: 'MONTHLY', ends: 'until', until: '2026-08-01' }),
    'FREQ=MONTHLY;UNTIL=20260801T235959Z');
});

// ---- vCard PHOTO pass-through -------------------------------------------
// The contact editor does not model PHOTO, so a contact's photo survives a
// load→save round-trip only by being carried in the store's `preserve` array:
// parseContact keeps every property not in MANAGED_VCARD as a vcardLine, and
// buildVCard re-emits those lines verbatim. These tests exercise that exact
// mechanism (parse → filter unmanaged → vcardLine → buildVCard → re-parse) so
// the round-trip that keeps a synced PHOTO can't regress silently.

// preserveOf mirrors parseContact's preserve step for the property `name`:
// reconstruct the content line for a property the editor doesn't own.
const preserveOf = (card, name) =>
  card.props.filter(p => p.name === name && !ical.MANAGED_VCARD.has(p.name)).map(ical.vcardLine);

test('MANAGED_VCARD leaves PHOTO unmanaged so it round-trips via preserve', () => {
  // The linchpin: if PHOTO were "managed" the editor would own (and drop) it.
  assert.equal(ical.MANAGED_VCARD.has('PHOTO'), false);
});

test('vCard base64 PHOTO survives a parse → preserve → buildVCard round-trip', () => {
  const b64 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg';
  const src = [
    'BEGIN:VCARD', 'VERSION:3.0', 'UID:photo-1', 'FN:Ada Byte', 'N:Byte;Ada;;;',
    `PHOTO;ENCODING=b;TYPE=PNG:${b64}`, 'END:VCARD',
  ].join('\r\n') + '\r\n';

  const card = ical.components(ical.parse(src), 'VCARD')[0];
  const photo = ical.prop(card, 'PHOTO');
  assert.equal(photo.value, b64);                      // parsed intact (folding rejoined)
  assert.equal(photo.params.ENCODING, 'b');
  assert.equal(photo.params.TYPE, 'PNG');

  const preserve = preserveOf(card, 'PHOTO');
  assert.equal(preserve.length, 1, 'PHOTO kept as a preserved line');

  const out = ical.buildVCard({ uid: 'photo-1', fn: 'Ada Byte', preserve });
  const round = ical.prop(ical.components(ical.parse(out), 'VCARD')[0], 'PHOTO');
  assert.equal(round.value, b64);                      // base64 payload byte-for-byte
  assert.equal(round.params.ENCODING, 'b');
  assert.equal(round.params.TYPE, 'PNG');
});

test('vCard PHOTO;VALUE=URI survives a parse → preserve → buildVCard round-trip', () => {
  const uri = 'https://photos.example.org/ada.jpg';
  const src = [
    'BEGIN:VCARD', 'VERSION:4.0', 'UID:photo-2', 'FN:Ada Byte',
    `PHOTO;VALUE=uri:${uri}`, 'END:VCARD',
  ].join('\r\n') + '\r\n';

  const card = ical.components(ical.parse(src), 'VCARD')[0];
  const preserve = preserveOf(card, 'PHOTO');
  assert.deepEqual(preserve, [`PHOTO;VALUE=uri:${uri}`]);

  const out = ical.buildVCard({ uid: 'photo-2', fn: 'Ada Byte', preserve });
  const round = ical.prop(ical.components(ical.parse(out), 'VCARD')[0], 'PHOTO');
  assert.equal(round.value, uri);
  assert.equal(round.params.VALUE, 'uri');
});

// vCard 2.1 encodes its parameters as valueless flags — PHOTO;BASE64: and
// PHOTO;JPEG;BASE64: rather than 3.0's PHOTO;ENCODING=b;TYPE=JPEG:. parse() keeps
// those bare flags (as '' values) and vcardLine re-emits them bare, so a
// 2.1-sourced photo round-trips losslessly through an edit→save instead of
// collapsing to an undecodable bare `PHOTO:<base64>`.
test('vCard 2.1 valueless flags (PHOTO;BASE64:, PHOTO;JPEG;BASE64:) round-trip losslessly', () => {
  const b64 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwC';
  const cases = [
    { line: `PHOTO;BASE64:${b64}`, params: { BASE64: '' } },
    { line: `PHOTO;JPEG;BASE64:${b64}`, params: { JPEG: '', BASE64: '' } },      // both bare, order kept
    { line: `PHOTO;JPEG;ENCODING=BASE64:${b64}`, params: { JPEG: '', ENCODING: 'BASE64' } }, // mixed bare + KEY=VALUE
  ];
  for (const { line, params } of cases) {
    const src = ['BEGIN:VCARD', 'VERSION:2.1', 'UID:photo-3', 'FN:Ada Byte', line, 'END:VCARD']
      .join('\r\n') + '\r\n';
    const card = ical.components(ical.parse(src), 'VCARD')[0];
    const photo = ical.prop(card, 'PHOTO');
    assert.equal(photo.value, b64);
    assert.deepEqual(photo.params, params);            // valueless flags retained

    // preserve reconstructs the exact original line, and buildVCard re-emits it.
    assert.deepEqual(preserveOf(card, 'PHOTO'), [line]);
    const out = ical.buildVCard({ uid: 'photo-3', fn: 'Ada Byte', preserve: preserveOf(card, 'PHOTO') });
    const round = ical.prop(ical.components(ical.parse(out), 'VCARD')[0], 'PHOTO');
    assert.equal(round.value, b64);
    assert.deepEqual(round.params, params);            // bare flags survive the round-trip
  }
});

// ---- photoSrc / buildPhotoLine (contacts view display + upload) ----------

test('photoSrc renders a base64 PHOTO as a data: URL and a URI PHOTO as-is', () => {
  const p = name => ical.prop(ical.components(ical.parse(
    `BEGIN:VCARD\r\nVERSION:3.0\r\nUID:a\r\nFN:A\r\n${name}\r\nEND:VCARD\r\n`), 'VCARD')[0], 'PHOTO');
  assert.equal(ical.photoSrc(p('PHOTO;ENCODING=b;TYPE=PNG:AAAB')), 'data:image/png;base64,AAAB');
  assert.equal(ical.photoSrc(p('PHOTO;ENCODING=b;TYPE=JPEG:AAAB')), 'data:image/jpeg;base64,AAAB');
  assert.equal(ical.photoSrc(p('PHOTO;BASE64:AAAB')), 'data:image/jpeg;base64,AAAB'); // 2.1, TYPE unknown
  assert.equal(ical.photoSrc(p('PHOTO;JPEG;BASE64:AAAB')), 'data:image/jpeg;base64,AAAB'); // 2.1 bare subtype
  assert.equal(ical.photoSrc(p('PHOTO;VALUE=uri:https://x/y.jpg')), 'https://x/y.jpg');
  assert.equal(ical.photoSrc(p('PHOTO:https://x/y.jpg')), 'https://x/y.jpg'); // bare http value
  assert.equal(ical.photoSrc(null), '');
  assert.equal(ical.photoSrc({ value: '', params: {} }), '');
});

test('buildPhotoLine turns an uploaded data: URL into a PHOTO preserve line', () => {
  const line = ical.buildPhotoLine('data:image/png;base64,AAAB');
  assert.equal(line, 'PHOTO;ENCODING=b;TYPE=PNG:AAAB');
  // it must round-trip back to the same displayable src.
  const card = ical.components(ical.parse(`BEGIN:VCARD\r\nVERSION:3.0\r\nUID:a\r\nFN:A\r\n${line}\r\nEND:VCARD\r\n`), 'VCARD')[0];
  assert.equal(ical.photoSrc(ical.prop(card, 'PHOTO')), 'data:image/png;base64,AAAB');
  assert.equal(ical.buildPhotoLine('not-a-data-url'), '');
});

test('isPhotoLine matches PHOTO lines (incl. Apple item groups), not others', () => {
  assert.ok(ical.isPhotoLine('PHOTO;ENCODING=b:AAAB'));
  assert.ok(ical.isPhotoLine('PHOTO:https://x/y.jpg'));
  assert.ok(ical.isPhotoLine('item1.PHOTO;ENCODING=b:AAAB'));
  assert.ok(!ical.isPhotoLine('LOGO;ENCODING=b:AAAB'));
  assert.ok(!ical.isPhotoLine('CATEGORIES:friends'));
});

// ---- recurrence split ("this and following") ------------------------------

test('rruleParts parses into an uppercased map', () => {
  assert.deepEqual(ical.rruleParts('FREQ=WEEKLY;interval=2;COUNT=10'),
    { FREQ: 'WEEKLY', INTERVAL: '2', COUNT: '10' });
  assert.deepEqual(ical.rruleParts(''), {});
});

test('canSplitSeries allows plain rules, rejects BY* and short-month days', () => {
  const d10 = new Date(2026, 0, 10), d31 = new Date(2026, 0, 31);
  assert.ok(ical.canSplitSeries(d10, 'FREQ=DAILY'));
  assert.ok(ical.canSplitSeries(d10, 'FREQ=WEEKLY;INTERVAL=2;COUNT=10'));
  assert.ok(ical.canSplitSeries(d10, 'FREQ=MONTHLY;COUNT=6'));       // day 10 is safe
  assert.ok(!ical.canSplitSeries(d31, 'FREQ=MONTHLY;COUNT=6'));      // day 31 skips short months
  assert.ok(!ical.canSplitSeries(d10, 'FREQ=MONTHLY;BYDAY=2TU'));    // BY* not steppable
  assert.ok(!ical.canSplitSeries(d10, 'FREQ=HOURLY'));
});

test('occurrencesBefore counts occurrences strictly before the split', () => {
  const start = new Date(2026, 0, 1, 9, 0);          // Thu Jan 1 09:00
  // weekly: Jan 1, 8, 15, 22, ... — split at Jan 22 keeps 3 (1,8,15).
  assert.equal(ical.occurrencesBefore(start, 'FREQ=WEEKLY;COUNT=10', new Date(2026, 0, 22, 9, 0)), 3);
  // daily every 2 days from Jan 1: 1,3,5,7 — split at Jan 7 keeps 3.
  assert.equal(ical.occurrencesBefore(start, 'FREQ=DAILY;INTERVAL=2', new Date(2026, 0, 7, 9, 0)), 3);
});

test('rruleWithUntil drops COUNT and sets UNTIL', () => {
  assert.equal(ical.rruleWithUntil('FREQ=WEEKLY;COUNT=10', '20260121T090000Z'),
    'FREQ=WEEKLY;COUNT=10'.replace(';COUNT=10', '') + ';UNTIL=20260121T090000Z');
  assert.equal(ical.rruleWithUntil('FREQ=DAILY', '20260121'), 'FREQ=DAILY;UNTIL=20260121');
});

test('splitSeriesRRules reduces the tail COUNT by the head occurrences', () => {
  const start = new Date(2026, 0, 1, 9, 0);
  const s = ical.splitSeriesRRules(start, 'FREQ=WEEKLY;COUNT=10', new Date(2026, 0, 22, 9, 0), '20260121T090000Z');
  assert.equal(s.past, 3);
  assert.equal(s.head, 'FREQ=WEEKLY;UNTIL=20260121T090000Z');
  assert.equal(s.tail, 'FREQ=WEEKLY;COUNT=7');       // 10 - 3
});

test('splitSeriesRRules keeps never/until tails intact', () => {
  const start = new Date(2026, 0, 1, 9, 0);
  const never = ical.splitSeriesRRules(start, 'FREQ=DAILY', new Date(2026, 0, 5, 9, 0), '20260104T090000Z');
  assert.equal(never.tail, 'FREQ=DAILY');
  const until = ical.splitSeriesRRules(start, 'FREQ=DAILY;UNTIL=20261231T000000Z', new Date(2026, 0, 5, 9, 0), '20260104T090000Z');
  assert.equal(until.tail, 'FREQ=DAILY;UNTIL=20261231T000000Z');
  assert.equal(ical.splitSeriesRRules(start, 'FREQ=MONTHLY;BYDAY=1MO', new Date(2026, 5, 1), 'x'), null);
});

test('dropForward strips overrides and EXDATEs at/after the cutoff, keeps earlier ones', () => {
  const text = [
    'BEGIN:VCALENDAR', 'VERSION:2.0',
    'BEGIN:VEVENT', 'UID:s', 'DTSTART:20260101T090000Z', 'RRULE:FREQ=WEEKLY',
    'EXDATE:20260108T090000Z', 'EXDATE:20260122T090000Z', 'END:VEVENT',
    'BEGIN:VEVENT', 'UID:s', 'RECURRENCE-ID:20260115T090000Z', 'DTSTART:20260115T100000Z', 'SUMMARY:moved-early', 'END:VEVENT',
    'BEGIN:VEVENT', 'UID:s', 'RECURRENCE-ID:20260129T090000Z', 'DTSTART:20260129T100000Z', 'SUMMARY:moved-late', 'END:VEVENT',
    'END:VCALENDAR', '',
  ].join('\r\n');
  const out = ical.dropForward(text, new Date(Date.UTC(2026, 0, 22, 9, 0)));
  assert.ok(out.includes('SUMMARY:moved-early'));      // Jan 15 override kept (before cutoff)
  assert.ok(!out.includes('SUMMARY:moved-late'));      // Jan 29 override dropped
  assert.ok(out.includes('EXDATE:20260108T090000Z'));  // earlier EXDATE kept
  assert.ok(!out.includes('20260122T090000Z'));        // cutoff-day EXDATE dropped
});

test('eventLines marks a room attendee with CUTYPE=ROOM, people without', () => {
  const lines = ical.eventLines({
    uid: 'u', summary: 'S', start: '2026-08-10T09:00:00Z', end: '2026-08-10T10:00:00Z',
    attendees: [{ email: 'alice@example.org' }, { email: 'room1@example.org', room: true }],
  });
  const att = lines.filter(l => l.startsWith('ATTENDEE'));
  assert.ok(att.some(l => l.includes('room1@example.org') && l.includes('CUTYPE=ROOM')));
  assert.ok(att.some(l => l.includes('alice@example.org') && !l.includes('CUTYPE')));
});

test('buildRRule emits INTERVAL (>1) and weekly BYDAY', () => {
  assert.equal(ical.buildRRule({ freq: 'DAILY', interval: 1, ends: 'never' }), 'FREQ=DAILY');
  assert.equal(ical.buildRRule({ freq: 'WEEKLY', interval: 2, byday: ['MO', 'WE'], ends: 'never' }),
    'FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE');
  assert.equal(ical.buildRRule({ freq: 'WEEKLY', interval: 1, byday: ['FR'], ends: 'count', count: '5' }),
    'FREQ=WEEKLY;BYDAY=FR;COUNT=5');
});

test('parseRRulePreset reads INTERVAL and weekly BYDAY, rejects positional', () => {
  assert.deepEqual(ical.parseRRulePreset('FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE'),
    { freq: 'WEEKLY', interval: 2, byday: ['MO', 'WE'], ends: 'never', count: '10', until: '' });
  assert.equal(ical.parseRRulePreset('FREQ=MONTHLY;BYDAY=2MO'), null); // positional → custom
  assert.equal(ical.parseRRulePreset('FREQ=DAILY;BYDAY=MO'), null);    // BYDAY only for weekly
  assert.equal(ical.parseRRulePreset('FREQ=DAILY;INTERVAL=3').interval, 3);
});
