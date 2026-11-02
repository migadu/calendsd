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

test('eventLines emits one DISPLAY VALARM per reminder trigger', () => {
  const lines = ical.eventLines({
    uid: 'x', summary: 'Call, Bob', start: '2026-07-15T10:00:00Z',
    end: '2026-07-15T11:00:00Z', alarms: ['-PT15M', 'PT0S'],
  });
  const i = lines.indexOf('BEGIN:VALARM');
  assert.ok(i > 0, 'VALARM present');
  assert.deepEqual(lines.slice(i, i + 10), [
    'BEGIN:VALARM', 'ACTION:DISPLAY', 'DESCRIPTION:Call\\, Bob',
    'TRIGGER;RELATED=START:-PT15M', 'END:VALARM',
    'BEGIN:VALARM', 'ACTION:DISPLAY', 'DESCRIPTION:Call\\, Bob',
    'TRIGGER;RELATED=START:PT0S', 'END:VALARM',
  ]);
  // no alarms field → no VALARM
  assert.ok(!ical.eventLines({
    uid: 'x', summary: 's', start: '2026-07-15T10:00:00Z', end: '2026-07-15T11:00:00Z',
  }).includes('BEGIN:VALARM'));
});

test('readReminders lists plain relative reminders and flags the rest as custom', () => {
  const ev = t => ical.components(ical.parse(
    `BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\r\n${t}END:VEVENT\r\nEND:VCALENDAR\r\n`), 'VEVENT')[0];
  const plain = (...triggers) => ({ triggers, custom: false });
  const custom = { triggers: [], custom: true };
  assert.deepEqual(ical.readReminders(ev('')), plain());                     // no alarm
  assert.deepEqual(ical.readReminders(ev(
    'BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER;RELATED=START:-PT15M\r\nEND:VALARM\r\n')), plain('-PT15M'));
  assert.deepEqual(ical.readReminders(ev(                                    // any offset, several alarms
    'BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT10M\r\nEND:VALARM\r\n' +
    'BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:PT0S\r\nEND:VALARM\r\n')), plain('-PT10M', 'PT0S'));
  assert.deepEqual(ical.readReminders(ev(                                    // EMAIL action
    'BEGIN:VALARM\r\nACTION:EMAIL\r\nTRIGGER:-PT15M\r\nEND:VALARM\r\n')), custom);
  assert.deepEqual(ical.readReminders(ev(                                    // relative to END on an event
    'BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER;RELATED=END:-PT15M\r\nEND:VALARM\r\n')), custom);
  assert.deepEqual(ical.readReminders(ev(                                    // absolute trigger
    'BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER;VALUE=DATE-TIME:20260715T094500Z\r\nEND:VALARM\r\n')), custom);
  assert.deepEqual(ical.readReminders(ev(                                    // a task's DUE-relative reminder
    'BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER;RELATED=END:-PT1H\r\nEND:VALARM\r\n'), 'END'), plain('-PT1H'));
});

test('offsetToTrigger and triggerToOffset round-trip the editor offsets', () => {
  assert.equal(ical.offsetToTrigger({ when: 'at' }), 'PT0S');
  assert.equal(ical.offsetToTrigger({ when: 'before', amount: 15, unit: 'minutes' }), '-PT15M');
  assert.equal(ical.offsetToTrigger({ when: 'after', amount: 2, unit: 'days' }), 'P2D');
  assert.equal(ical.offsetToTrigger({ when: 'before', amount: 1, unit: 'weeks' }), '-P1W');
  assert.deepEqual(ical.triggerToOffset('-PT60M'), { when: 'before', amount: 1, unit: 'hours' });
  assert.deepEqual(ical.triggerToOffset('-P7D'), { when: 'before', amount: 1, unit: 'weeks' });
  assert.deepEqual(ical.triggerToOffset('PT0S'), { when: 'at', amount: 0, unit: 'minutes' });
  assert.equal(ical.triggerToOffset('20260715T094500Z'), null);
});

test('setMasterAlarms replaces alarms on the master and can clear them', () => {
  const set = ical.setMasterAlarms(SAMPLE, ['-PT30M', '-P1D'], 'Team sync');
  assert.equal(ical.unfold(set).filter(l => l === 'BEGIN:VALARM').length, 2);
  assert.match(set, /TRIGGER;RELATED=START:-PT30M/);
  assert.match(set, /TRIGGER;RELATED=START:-P1D/);
  assert.doesNotMatch(set, /DESCRIPTION:reminder/);          // the old VALARM is gone
  assert.match(set, /SUMMARY:Team sync/);                    // top-level props untouched
  // an empty list clears every alarm
  assert.ok(!ical.setMasterAlarms(SAMPLE, []).includes('BEGIN:VALARM'));
});

test('setMasterAlarms leaves an override VEVENT alone', () => {
  const withOverride = ical.addOverride(SAMPLE,
    ['UID:abc', 'RECURRENCE-ID:20260716T100000Z', 'SUMMARY:Override',
     'BEGIN:VALARM', 'ACTION:DISPLAY', 'DESCRIPTION:o', 'TRIGGER:-PT5M', 'END:VALARM']);
  const out = ical.setMasterAlarms(withOverride, []);
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

const Z = 'Europe/Zurich';
const at = iso => new Date(iso);

test('shiftDate moves each form by its own delta; refuses floating and unknown zones', () => {
  const hour = { from: at('2026-07-15T08:00:00Z'), to: at('2026-07-15T09:00:00Z') };
  assert.equal(ical.shiftDate('DTSTART', { value: '20260715T100000Z', params: {} }, hour),
    'DTSTART:20260715T110000Z');
  // DATE values move by calendar days of from/to (device-local dates).
  const twoDays = { from: new Date(2026, 6, 15), to: new Date(2026, 6, 17) };
  assert.equal(ical.shiftDate('DTSTART', { value: '20260715', params: { VALUE: 'DATE' } }, twoDays),
    'DTSTART;VALUE=DATE:20260717');
  assert.equal(ical.shiftDate('DTSTART', { value: '20260715T100000', params: { TZID: Z } }, hour),
    'DTSTART;TZID=Europe/Zurich:20260715T110000');
  assert.equal(ical.shiftDate('DTSTART', { value: '20260715T100000', params: {} }, hour), null);
  assert.equal(ical.shiftDate('DTSTART',
    { value: '20260715T100000', params: { TZID: 'W. Europe Standard Time' } }, hour), null);
});

test('shiftDate: a 23- or 25-hour move across DST is one wall-clock day in the value’s zone', () => {
  // Zurich 09:00 on 28 March → 09:00 on 29 March is 23 elapsed hours.
  const spring = { from: at('2026-03-28T08:00:00Z'), to: at('2026-03-29T07:00:00Z') };
  // ... and 24 → 25 October is 25 hours.
  const autumn = { from: at('2026-10-24T07:00:00Z'), to: at('2026-10-25T08:00:00Z') };
  // Applied to a master whose first occurrence is in another season, both
  // stay exactly one day at the same wall time.
  const master = { value: '20260105T090000', params: { TZID: Z } };
  assert.equal(ical.shiftDate('DTSTART', master, spring), 'DTSTART;TZID=Europe/Zurich:20260106T090000');
  assert.equal(ical.shiftDate('DTSTART', master, autumn), 'DTSTART;TZID=Europe/Zurich:20260106T090000');
  // The delta is taken in the VALUE's zone: a New York series moved from
  // 09:00 EST on 7 March to 09:00 EDT on 8 March (23 elapsed hours across New
  // York's own switch) moves one day, even though in Zurich — still on
  // winter time — those instants are 15:00 and 14:00.
  const ny = { value: '20260105T090000', params: { TZID: 'America/New_York' } };
  const nySpring = { from: at('2026-03-07T14:00:00Z'), to: at('2026-03-08T13:00:00Z') };
  assert.equal(ical.shiftDate('DTSTART', ny, nySpring), 'DTSTART;TZID=America/New_York:20260106T090000');
  // ... and the Zurich move above is 23 hours of New York wall time.
  assert.equal(ical.shiftDate('DTSTART', ny, spring), 'DTSTART;TZID=America/New_York:20260106T080000');
});

test('parseICalDate resolves TZID values, with RFC 5545 gap and fold rules', () => {
  const p = ical.parseICalDate('20260313T090000', { TZID: Z });
  assert.equal(p.date.toISOString(), '2026-03-13T08:00:00.000Z');
  assert.equal(p.tzid, Z);
  // Repeated hour → the earlier instant; skipped hour → offset before the gap.
  assert.equal(ical.parseICalDate('20261025T023000', { TZID: Z }).date.toISOString(), '2026-10-25T00:30:00.000Z');
  assert.equal(ical.parseICalDate('20260329T023000', { TZID: Z }).date.toISOString(), '2026-03-29T01:30:00.000Z');
});

test('eventLines writes timed events in the given zone, all-day as DATE, legacy UTC without a zone', () => {
  const base = { uid: 'u', summary: 's', start: '2026-03-13T08:00:00Z', end: '2026-03-13T09:00:00Z' };
  const zoned = ical.eventLines({ ...base, tz: Z });
  assert.ok(zoned.includes('DTSTART;TZID=Europe/Zurich:20260313T090000'));
  assert.ok(zoned.includes('DTEND;TZID=Europe/Zurich:20260313T100000'));
  assert.ok(ical.eventLines(base).includes('DTSTART:20260313T080000Z'));
  assert.ok(ical.eventLines({ ...base, tz: 'UTC' }).includes('DTSTART:20260313T080000Z'));
  const allDay = ical.eventLines({ ...base, allDay: true, start: '2026-03-13', end: '2026-03-14', tz: Z });
  assert.ok(allDay.includes('DTSTART;VALUE=DATE:20260313'));
});

test('dtLine keeps UTC for the second occurrence of a repeated hour', () => {
  assert.equal(ical.dtLine('DTSTART', at('2026-10-25T00:30:00Z'), Z), 'DTSTART;TZID=Europe/Zurich:20261025T023000');
  assert.equal(ical.dtLine('DTSTART', at('2026-10-25T01:30:00Z'), Z), 'DTSTART:20261025T013000Z');
});

test('occurrenceLine follows the master DTSTART form', () => {
  const when = at('2026-04-03T07:00:00Z');
  assert.equal(ical.occurrenceLine('EXDATE', when, false, { value: '20260313T090000', params: { TZID: Z } }),
    'EXDATE;TZID=Europe/Zurich:20260403T090000');
  assert.equal(ical.occurrenceLine('EXDATE', when, false, { value: '20260313T080000Z', params: {} }),
    'EXDATE:20260403T070000Z');
  assert.equal(ical.occurrenceLine('RECURRENCE-ID', new Date(2026, 3, 3), true, { value: '20260313', params: { VALUE: 'DATE' } }),
    'RECURRENCE-ID;VALUE=DATE:20260403');
});

test('addOverride replaces an override named in another form (UTC vs TZID)', () => {
  const stored = ['BEGIN:VCALENDAR', 'BEGIN:VEVENT', 'UID:u', 'DTSTART;TZID=Europe/Zurich:20260313T090000',
    'RRULE:FREQ=WEEKLY', 'END:VEVENT', 'BEGIN:VEVENT', 'UID:u', 'RECURRENCE-ID:20260403T070000Z',
    'SUMMARY:old', 'END:VEVENT', 'END:VCALENDAR', ''].join('\r\n');
  const out = ical.addOverride(stored, ['UID:u', 'RECURRENCE-ID;TZID=Europe/Zurich:20260403T090000', 'SUMMARY:new']);
  assert.doesNotMatch(out, /SUMMARY:old/);
  assert.equal(out.match(/RECURRENCE-ID/g).length, 1);
});

test('series stepping follows the series zone across DST', () => {
  // A weekly Zurich 09:00 series: the occurrence on 3 April (after the
  // switch) is at 07:00Z, so exactly three occurrences precede it.
  const start = at('2026-03-13T08:00:00Z');
  assert.equal(ical.occurrencesBefore(start, 'FREQ=WEEKLY', at('2026-04-03T07:00:00Z'), Z), 3);
  assert.equal(ical.nextOccurrence(start, 'FREQ=WEEKLY', at('2026-04-01T00:00:00Z'), Z).toISOString(),
    '2026-04-03T07:00:00.000Z');
  // A UTC series steps in UTC: its 3 April occurrence is still at 08:00Z.
  assert.equal(ical.nextOccurrence(start, 'FREQ=WEEKLY', at('2026-04-01T00:00:00Z'), 'UTC').toISOString(),
    '2026-04-03T08:00:00.000Z');
});

// A calweb-written weekly series stored in UTC, with an EXDATE and an override
// after the spring change, and a head-split UNTIL.
const utcSeries = (prodid = '-//calclient//EN') => ['BEGIN:VCALENDAR', 'VERSION:2.0', `PRODID:${prodid}`,
  'BEGIN:VEVENT', 'UID:a', 'DTSTAMP:20260101T000000Z', 'DTSTART:20260313T080000Z', 'DTEND:20260313T090000Z',
  'RRULE:FREQ=WEEKLY;UNTIL=20260424T075959Z', 'EXDATE:20260403T080000Z', 'SUMMARY:x',
  'BEGIN:VALARM', 'ACTION:DISPLAY', 'TRIGGER:-PT15M', 'END:VALARM', 'END:VEVENT',
  'BEGIN:VEVENT', 'UID:a', 'RECURRENCE-ID:20260410T080000Z', 'DTSTART:20260410T120000Z',
  'DTEND:20260410T130000Z', 'SUMMARY:moved', 'END:VEVENT', 'END:VCALENDAR', ''].join('\r\n');

test('convertSeriesToZone keeps the first instant and maps every slot reference', () => {
  const r = ical.convertSeriesToZone(utcSeries(), Z);
  assert.equal(r.error, undefined);
  const t = r.text;
  assert.match(t, /DTSTART;TZID=Europe\/Zurich:20260313T090000\r\n/);   // first occurrence unchanged
  assert.match(t, /DTEND;TZID=Europe\/Zurich:20260313T100000/);
  // Slots after the transition name the zoned series' 09:00, not 10:00.
  assert.match(t, /EXDATE;TZID=Europe\/Zurich:20260403T090000/);
  assert.match(t, /RECURRENCE-ID;TZID=Europe\/Zurich:20260410T090000/);
  // The override's own chosen times keep their instants (12:00Z = 14:00 CEST).
  assert.match(t, /DTSTART;TZID=Europe\/Zurich:20260410T140000/);
  // UNTIL still ends the series after the same slot.
  assert.match(t, /UNTIL=20260424T065959Z/);
  // Nested VALARM untouched.
  assert.match(t, /TRIGGER:-PT15M/);
});

test('convertSeriesToZone refuses what it cannot convert exactly', () => {
  // First occurrence in the repeated hour.
  const fold = utcSeries().replace('DTSTART:20260313T080000Z', 'DTSTART:20261025T013000Z');
  assert.match(ical.convertSeriesToZone(fold, Z).error, /repeats/);
  // A weekday rule whose UTC day differs from the local day (23:30Z Monday is
  // 00:30 Tuesday in Zurich).
  const cross = utcSeries().replace('DTSTART:20260313T080000Z', 'DTSTART:20260309T233000Z')
    .replace('RRULE:FREQ=WEEKLY;UNTIL=20260424T075959Z', 'RRULE:FREQ=WEEKLY;BYDAY=MO');
  assert.match(ical.convertSeriesToZone(cross, Z).error, /days/);
  // Already zoned.
  assert.ok(ical.convertSeriesToZone(ical.convertSeriesToZone(utcSeries(), Z).text, Z).error);
});

// A weekly UTC series and the occurrence the user opens; stored DTSTART for
// the converted master is what every assertion below reads.
const weekly = start => utcSeries().replace('DTSTART:20260313T080000Z', `DTSTART:${start}`)
  .replace('DTEND:20260313T090000Z', `DTEND:${start.replace(/T(\d\d)/, (_, h) => 'T' + String(+h + 1).padStart(2, '0'))}`)
  .replace('RRULE:FREQ=WEEKLY;UNTIL=20260424T075959Z', 'RRULE:FREQ=WEEKLY')
  .replace(/EXDATE:.*\r\n/, '').replace(/BEGIN:VEVENT\r\nUID:a\r\nRECURRENCE-ID[\s\S]*?END:VEVENT\r\n/, '');
const masterStart = t => /DTSTART;TZID=Europe\/Zurich:(\d{8}T\d{6})/.exec(t)[1];

// The editor shows the opened occurrence at its converted slot; these pass
// that as `shown`, as the editor does.
const slotOf = (occ, first) => ({
  start: ical.convertedSlot(occ.start, at(first), Z), end: ical.convertedSlot(occ.end, at(first), Z),
});

test('convertAndMove: leaving the shown (converted) time puts the series at the first occurrence\u2019s time', () => {
  // Winter-anchored at 09:00 CET (08:00Z); the 3 April occurrence drifted to
  // 10:00 CEST (08:00Z). The editor shows 09:00 and the user leaves it.
  const occ = { start: at('2026-04-03T08:00:00Z'), end: at('2026-04-03T09:00:00Z') };
  const shown = slotOf(occ, '2026-03-13T08:00:00Z');
  assert.equal(shown.start.toISOString(), '2026-04-03T07:00:00.000Z');
  const r = ical.convertAndMove(weekly('20260313T080000Z'), Z, shown, shown);
  assert.equal(r.error, undefined);
  assert.equal(masterStart(r.text), '20260313T090000');
});

test('convertAndMove: typing the drifted time keeps the series there', () => {
  // Same series; the user wants the 10:00 the grid showed and types it.
  const occ = { start: at('2026-04-03T08:00:00Z'), end: at('2026-04-03T09:00:00Z') };
  const target = { start: at('2026-04-03T08:00:00Z'), end: at('2026-04-03T09:00:00Z') };
  const r = ical.convertAndMove(weekly('20260313T080000Z'), Z, slotOf(occ, '2026-03-13T08:00:00Z'), target);
  assert.equal(masterStart(r.text), '20260313T100000');
});

test('convertAndMove: the mirror case — a summer-anchored series fixed in winter', () => {
  // Anchored at 10:00 CEST (08:00Z) in April; the 6 November occurrence
  // drifted to 09:00 CET (08:00Z) and is shown at its converted 10:00.
  const occ = { start: at('2026-11-06T08:00:00Z'), end: at('2026-11-06T09:00:00Z') };
  const shown = slotOf(occ, '2026-04-03T08:00:00Z');
  assert.equal(shown.start.toISOString(), '2026-11-06T09:00:00.000Z');
  assert.equal(masterStart(ical.convertAndMove(weekly('20260403T080000Z'), Z, shown, shown).text), '20260403T100000');
});

test('convertAndMove: a real change moves the series by the wall-clock difference', () => {
  const occ = { start: at('2026-04-03T08:00:00Z'), end: at('2026-04-03T09:00:00Z') };
  // Shown at 09:00 on 3 April; asking for 11:00 on the 4th moves the series
  // one day and two hours.
  const target = { start: at('2026-04-04T09:00:00Z'), end: at('2026-04-04T10:00:00Z') };
  const r = ical.convertAndMove(weekly('20260313T080000Z'), Z, slotOf(occ, '2026-03-13T08:00:00Z'), target);
  assert.equal(masterStart(r.text), '20260314T110000');
  assert.match(r.text, /DTEND;TZID=Europe\/Zurich:20260314T120000/);
});

test('conversionMovesOccurrences catches a DST change inside a short series', () => {
  // Daily 20 March – 10 April: three weeks across the 29 March switch.
  assert.equal(ical.conversionMovesOccurrences(at('2026-03-20T08:00:00Z'),
    'FREQ=DAILY;UNTIL=20260410T080000Z', Z), true);
  // ... but not one that ends before it.
  assert.equal(ical.conversionMovesOccurrences(at('2026-03-01T08:00:00Z'),
    'FREQ=DAILY;UNTIL=20260320T080000Z', Z), false);
});

test('convertedSlot puts a drifted occurrence back at the first occurrence’s local time', () => {
  // A winter-anchored 09:00 series: its 3 April occurrence (08:00Z = 10:00
  // CEST) belongs at 09:00 CEST (07:00Z) once converted — where a split's
  // tail must start.
  assert.equal(ical.convertedSlot(at('2026-04-03T08:00:00Z'), at('2026-03-13T08:00:00Z'), Z).toISOString(),
    '2026-04-03T07:00:00.000Z');
  // An occurrence in the anchor's own season doesn't move.
  assert.equal(ical.convertedSlot(at('2026-03-20T08:00:00Z'), at('2026-03-13T08:00:00Z'), Z).toISOString(),
    '2026-03-20T08:00:00.000Z');
});

test('convertSeriesToZone leaves an override stored in another zone exactly as it is', () => {
  const t = utcSeries().replace('DTSTART:20260410T120000Z', 'DTSTART;TZID=America/New_York:20260410T080000')
    .replace('DTEND:20260410T130000Z', 'DTEND;TZID=America/New_York:20260410T090000');
  const r = ical.convertSeriesToZone(t, Z);
  assert.equal(r.error, undefined);
  assert.match(r.text, /\r\nDTSTART;TZID=America\/New_York:20260410T080000\r\n/);
  assert.match(r.text, /\r\nDTEND;TZID=America\/New_York:20260410T090000\r\n/);
});

test('convertSeriesToZone keeps exceptions attached to RDATE occurrences', () => {
  // An added occurrence at 10:00 CEST (08:00Z) keeps its instant, and so do
  // the EXDATE and override naming it — while rule slots move to 09:00.
  const t = utcSeries().replace('EXDATE:20260403T080000Z', 'RDATE:20260711T080000Z\r\nEXDATE:20260403T080000Z,20260711T080000Z')
    .replace('RECURRENCE-ID:20260410T080000Z', 'RECURRENCE-ID:20260711T080000Z')
    .replace('UNTIL=20260424T075959Z', 'UNTIL=20261231T000000Z');
  const r = ical.convertSeriesToZone(t, Z);
  assert.equal(r.error, undefined);
  assert.match(r.text, /RDATE;TZID=Europe\/Zurich:20260711T100000/);
  assert.match(r.text, /EXDATE;TZID=Europe\/Zurich:20260403T090000,20260711T100000/);
  assert.match(r.text, /RECURRENCE-ID;TZID=Europe\/Zurich:20260711T100000/);
  const info = ical.utcSeriesInfo(t);
  assert.equal(ical.convertedRef(at('2026-07-11T08:00:00Z'), info, Z).toISOString(), '2026-07-11T08:00:00.000Z');
  assert.equal(ical.convertedRef(at('2026-04-03T08:00:00Z'), info, Z).toISOString(), '2026-04-03T07:00:00.000Z');
});

test('convertSeriesToZone refuses monthly/yearly and sub-daily rules whose dates depend on UTC days', () => {
  // 23:30Z on 31 January is 00:30 on 1 February in Zurich: monthly on the
  // 31st (skipping short months) would become monthly on the 1st.
  const monthly = utcSeries().replace('DTSTART:20260313T080000Z', 'DTSTART:20260131T233000Z')
    .replace('RRULE:FREQ=WEEKLY;UNTIL=20260424T075959Z', 'RRULE:FREQ=MONTHLY');
  assert.match(ical.convertSeriesToZone(monthly, Z).error, /days/);
  const yearly = monthly.replace('RRULE:FREQ=MONTHLY', 'RRULE:FREQ=YEARLY');
  assert.match(ical.convertSeriesToZone(yearly, Z).error, /days/);
  // The same rule anchored on the same local and UTC day converts.
  const sameDay = monthly.replace('DTSTART:20260131T233000Z', 'DTSTART:20260131T080000Z');
  assert.equal(ical.convertSeriesToZone(sameDay, Z).error, undefined);
  const hourly = utcSeries().replace('RRULE:FREQ=WEEKLY;UNTIL=20260424T075959Z', 'RRULE:FREQ=HOURLY;COUNT=48');
  assert.match(ical.convertSeriesToZone(hourly, Z).error, /times of day/);
});

test('utcSeriesInfo distinguishes calweb series; conversion notice and preview', () => {
  assert.equal(ical.utcSeriesInfo(utcSeries()).calweb, true);
  assert.equal(ical.utcSeriesInfo(utcSeries('-//Apple Inc.//EN')).calweb, false);
  const info = ical.utcSeriesInfo(utcSeries());
  assert.equal(ical.conversionMovesOccurrences(info.start, 'FREQ=WEEKLY', Z), true);
  assert.equal(ical.conversionMovesOccurrences(info.start, 'FREQ=WEEKLY', 'Asia/Tokyo'), false);
  const rows = ical.conversionPreview(info.start, 'FREQ=WEEKLY', Z, at('2026-03-01T00:00:00Z'), 2);
  assert.deepEqual(rows.map(r => [r.before.toISOString(), r.after.toISOString()]), [
    ['2026-04-03T08:00:00.000Z', '2026-04-03T07:00:00.000Z'],
    ['2026-04-10T08:00:00.000Z', '2026-04-10T07:00:00.000Z'],
  ]);
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
