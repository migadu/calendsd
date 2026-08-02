// Unit tests for the grid's placement math in web/cal/js/layout.js:
// overlap columns for timed events and lane assignment for multi-day bars.

import test from 'node:test';
import assert from 'node:assert/strict';
import { layoutColumns, spans } from '../web/cal/js/layout.js';
import { DAY, startOfWeek } from '../web/shared/time.js';

const at = (h, m = 0) => new Date(2026, 6, 15, h, m);
const ev = (start, end, name) => ({ start, end, summary: name });

test('layoutColumns: non-overlapping events all get the full width', () => {
  const placed = layoutColumns([ev(at(9), at(10), 'a'), ev(at(10), at(11), 'b')]);
  for (const p of placed) { assert.equal(p.col, 0); assert.equal(p.cols, 1); }
});

test('layoutColumns: overlapping events split into side-by-side columns', () => {
  const a = ev(at(9), at(11), 'a'), b = ev(at(10), at(12), 'b'), c = ev(at(13), at(14), 'c');
  const placed = layoutColumns([c, b, a]);   // input order must not matter
  const byName = Object.fromEntries(placed.map(p => [p.inst.summary, p]));
  assert.equal(byName.a.col, 0);
  assert.equal(byName.b.col, 1);
  assert.equal(byName.a.cols, 2);
  assert.equal(byName.b.cols, 2);
  assert.equal(byName.c.cols, 1);            // its own cluster, full width
});

test('layoutColumns: freed column is reused within a cluster', () => {
  // a 9-10, b 9-12, c 10-11: c overlaps only b, so it takes a\'s freed column 0
  const placed = layoutColumns([ev(at(9), at(10), 'a'), ev(at(9), at(12), 'b'), ev(at(10), at(11), 'c')]);
  const byName = Object.fromEntries(placed.map(p => [p.inst.summary, p]));
  assert.equal(byName.c.col, byName.a.col);
  assert.equal(byName.b.cols, 2);
});

test('spans: single-day events are excluded, multi-day get columns and lanes', () => {
  const weekStart = startOfWeek(new Date(2026, 6, 15), 1); // Mon 2026-07-13
  const day = n => new Date(+weekStart + n * DAY);
  const instances = [
    ev(day(1), day(4), 'threeDays'),          // Tue–Thu
    ev(day(2), day(5), 'overlapping'),        // Wed–Fri, must take lane 2
    ev(day(1), new Date(+day(1) + 3600e3), 'timed'),  // 1h — not a span
  ];
  const { multi, laneCount } = spans(instances, weekStart);
  assert.deepEqual(multi.map(s => s.inst.summary).sort(), ['overlapping', 'threeDays']);
  const byName = Object.fromEntries(multi.map(s => [s.inst.summary, s]));
  assert.equal(byName.threeDays.colStart, 1);
  assert.equal(byName.threeDays.colEnd, 3);   // end is exclusive → covers Tue,Wed,Thu
  assert.equal(byName.threeDays.lane, 0);
  assert.equal(byName.overlapping.lane, 1);
  assert.equal(laneCount, 2);
});

test('spans: events crossing the week edges are clamped and flagged', () => {
  const weekStart = startOfWeek(new Date(2026, 6, 15), 1);
  const before = new Date(+weekStart - 2 * DAY);
  const after = new Date(+weekStart + 9 * DAY);
  const { multi } = spans([ev(before, after, 'long')], weekStart);
  assert.equal(multi.length, 1);
  assert.equal(multi[0].colStart, 0);
  assert.equal(multi[0].colEnd, 6);
  assert.ok(multi[0].contLeft);
  assert.ok(multi[0].contRight);
});
