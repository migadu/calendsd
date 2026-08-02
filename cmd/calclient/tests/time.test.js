// Unit tests for web/shared/time.js. Tests that involve the host time zone
// are written relationally (construct local, format local) so they pass in
// any zone; the tz* helpers are pinned to named zones with known offsets.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  DAY, HOUR, startOfDay, addDays, nextDate, sameDay, startOfWeek, isoWeek,
  localDT, localDate, tzOffsetMs, tzDayStart,
} from '../web/shared/time.js';

test('constants', () => {
  assert.equal(DAY, 24 * 60 * 60 * 1000);
  assert.equal(HOUR, 60 * 60 * 1000);
});

test('startOfDay / addDays / nextDate / sameDay', () => {
  const d = new Date(2026, 6, 15, 13, 45, 30);
  assert.equal(+startOfDay(d), +new Date(2026, 6, 15));
  assert.equal(+addDays(startOfDay(d), 2), +startOfDay(d) + 2 * DAY);
  assert.equal(+nextDate(d), +new Date(2026, 6, 16));
  assert.ok(sameDay(d, new Date(2026, 6, 15, 0, 1)));
  assert.ok(!sameDay(d, new Date(2026, 6, 16)));
});

test('startOfWeek honors the week-start preference', () => {
  const wed = new Date(2026, 7, 5); // Wednesday 2026-08-05
  assert.equal(+startOfWeek(wed, 1), +new Date(2026, 7, 3));  // Monday
  assert.equal(+startOfWeek(wed, 0), +new Date(2026, 7, 2));  // Sunday
  const mon = new Date(2026, 7, 3);
  assert.equal(+startOfWeek(mon, 1), +mon);                    // already at start
});

test('isoWeek matches ISO-8601 year boundaries', () => {
  assert.equal(isoWeek(new Date(2026, 0, 1)), 1);    // Thu 2026-01-01 → W1
  assert.equal(isoWeek(new Date(2025, 11, 29)), 1);  // Mon before it → W1 of 2026
  assert.equal(isoWeek(new Date(2026, 11, 28)), 53); // 2026 has 53 ISO weeks
  assert.equal(isoWeek(new Date(2026, 6, 15)), 29);
});

test('localDT / localDate format input values from local time', () => {
  const d = new Date(2026, 6, 5, 9, 7);
  assert.equal(localDT(d), '2026-07-05T09:07');
  assert.equal(localDate(d), '2026-07-05');
});

test('tzOffsetMs is zero for UTC and DST-aware for Zurich', () => {
  const summer = new Date(Date.UTC(2026, 6, 15));
  const winter = new Date(Date.UTC(2026, 0, 15));
  assert.equal(tzOffsetMs(summer, 'UTC'), 0);
  assert.equal(tzOffsetMs(summer, 'Europe/Zurich'), 2 * HOUR);  // CEST
  assert.equal(tzOffsetMs(winter, 'Europe/Zurich'), 1 * HOUR);  // CET
});

test('tzDayStart maps a calendar date to midnight in the display zone', () => {
  const day = new Date(2026, 6, 15);
  // '' = device zone → local midnight
  assert.equal(+tzDayStart(day, ''), +startOfDay(day));
  // UTC → the UTC midnight instant, regardless of the host zone
  assert.equal(+tzDayStart(day, 'UTC'), Date.UTC(2026, 6, 15));
  // Zurich in summer is UTC+2 → its midnight is 22:00Z the previous day
  assert.equal(+tzDayStart(day, 'Europe/Zurich'), Date.UTC(2026, 6, 14, 22));
  // and in winter UTC+1 → 23:00Z
  assert.equal(+tzDayStart(new Date(2026, 0, 15), 'Europe/Zurich'), Date.UTC(2026, 0, 14, 23));
});
