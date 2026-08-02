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
  assert.deepEqual(ical.parseRRulePreset(''), { freq: '', ends: 'never' });
  assert.deepEqual(ical.parseRRulePreset('FREQ=WEEKLY;COUNT=5'),
    { freq: 'WEEKLY', ends: 'count', count: '5', until: '' });
  assert.deepEqual(ical.parseRRulePreset('FREQ=DAILY;UNTIL=20260801T235959Z'),
    { freq: 'DAILY', ends: 'until', count: '10', until: '2026-08-01' });
  assert.equal(ical.parseRRulePreset('FREQ=WEEKLY;BYDAY=MO,WE'), null);
  assert.equal(ical.parseRRulePreset('FREQ=WEEKLY;INTERVAL=2'), null);
});

test('buildRRule renders the preset back', () => {
  assert.equal(ical.buildRRule({ freq: '' }), '');
  assert.equal(ical.buildRRule({ freq: 'DAILY', ends: 'never' }), 'FREQ=DAILY');
  assert.equal(ical.buildRRule({ freq: 'WEEKLY', ends: 'count', count: '5' }), 'FREQ=WEEKLY;COUNT=5');
  assert.equal(ical.buildRRule({ freq: 'MONTHLY', ends: 'until', until: '2026-08-01' }),
    'FREQ=MONTHLY;UNTIL=20260801T235959Z');
});
