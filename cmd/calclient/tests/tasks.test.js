// Unit tests for the tasks store's pure logic: VTODO parsing into the task
// model and the due-date bucketing the list view groups by.

import test from 'node:test';
import assert from 'node:assert/strict';
import * as ical from '../web/shared/ical.js';
import { taskFrom, dueBucket } from '../web/shared/store/tasks.js';

function todoFrom(lines) {
  const text = ['BEGIN:VCALENDAR', 'VERSION:2.0', 'BEGIN:VTODO',
    ...lines, 'END:VTODO', 'END:VCALENDAR'].join('\r\n') + '\r\n';
  return ical.components(ical.parse(text), 'VTODO')[0];
}

test('taskFrom parses summary, notes, and due forms', () => {
  const t = taskFrom(todoFrom([
    'UID:t1', 'SUMMARY:Water\\, the plants', 'DESCRIPTION:back\\nporch',
    'DUE;VALUE=DATE:20260810',
  ]), '/cal/t1.ics', '/cal/', '"e1"');
  assert.equal(t.summary, 'Water, the plants');
  assert.equal(t.notes, 'back\nporch');
  assert.equal(t.due.allDay, true);
  assert.equal(+t.due.date, +new Date(2026, 7, 10));
  assert.equal(t.done, false);

  const timed = taskFrom(todoFrom(['UID:t2', 'SUMMARY:x', 'DUE:20260810T170000Z']), '/c/t2.ics', '/c/', '');
  assert.equal(timed.due.allDay, false);
  assert.equal(+timed.due.date, Date.UTC(2026, 7, 10, 17));

  const undated = taskFrom(todoFrom(['UID:t3', 'SUMMARY:x']), '/c/t3.ics', '/c/', '');
  assert.equal(undated.due, null);
});

test('taskFrom detects completion via STATUS or COMPLETED', () => {
  assert.equal(taskFrom(todoFrom(['UID:a', 'SUMMARY:x', 'STATUS:COMPLETED']), '/c/a', '/c/', '').done, true);
  assert.equal(taskFrom(todoFrom(['UID:b', 'SUMMARY:x', 'COMPLETED:20260801T100000Z']), '/c/b', '/c/', '').done, true);
  assert.equal(taskFrom(todoFrom(['UID:c', 'SUMMARY:x', 'STATUS:NEEDS-ACTION']), '/c/c', '/c/', '').done, false);
});

test('dueBucket groups by day boundaries, done always wins', () => {
  const now = new Date(2026, 7, 10, 15, 0);   // Aug 10, 3pm
  const task = (due, done = false) => ({ due, done });
  const day = (d, h = 0) => ({ allDay: h === 0, date: new Date(2026, 7, d, h) });

  assert.equal(dueBucket(task(null), now), 'someday');
  assert.equal(dueBucket(task(day(9)), now), 'overdue');
  assert.equal(dueBucket(task(day(10)), now), 'today');
  assert.equal(dueBucket(task(day(10, 23)), now), 'today');   // due 11pm today: still today
  assert.equal(dueBucket(task(day(10, 9)), now), 'today');    // earlier today: today, not overdue
  assert.equal(dueBucket(task(day(11)), now), 'upcoming');
  assert.equal(dueBucket(task(day(9), true), now), 'done');   // overdue-but-finished must not alarm
});

test('buildTodo renders DATE and DATE-TIME due forms', () => {
  const dated = ical.buildTodo({ uid: 'u1', summary: 'a;b', due: '2026-08-10' });
  assert.match(dated, /BEGIN:VTODO/);
  assert.match(dated, /SUMMARY:a\\;b/);
  assert.match(dated, /DUE;VALUE=DATE:20260810/);
  assert.match(dated, /STATUS:NEEDS-ACTION/);
  const timed = ical.buildTodo({ uid: 'u2', summary: 'x', due: '2026-08-10T17:00:00Z' });
  assert.match(timed, /DUE:20260810T170000Z/);
  const undated = ical.buildTodo({ uid: 'u3', summary: 'x' });
  assert.doesNotMatch(undated, /DUE/);
});

test('setMasterProps with VTODO edits the todo, spares siblings and alarms', () => {
  const text = [
    'BEGIN:VCALENDAR', 'VERSION:2.0',
    'BEGIN:VEVENT', 'UID:ev', 'SUMMARY:the event', 'END:VEVENT',
    'BEGIN:VTODO', 'UID:td', 'SUMMARY:old title', 'X-APP:keep',
    'BEGIN:VALARM', 'ACTION:DISPLAY', 'DESCRIPTION:ring', 'END:VALARM',
    'END:VTODO',
    'END:VCALENDAR'].join('\r\n') + '\r\n';
  const out = ical.setMasterProps(text, {
    SUMMARY: 'SUMMARY:new title',
    STATUS: 'STATUS:COMPLETED',
  }, 'VTODO');
  assert.match(out, /SUMMARY:new title/);
  assert.match(out, /STATUS:COMPLETED/);
  assert.match(out, /SUMMARY:the event/);    // sibling VEVENT untouched
  assert.match(out, /X-APP:keep/);           // unmodeled property preserved
  assert.match(out, /DESCRIPTION:ring/);     // VALARM untouched
  // and the default component is still VEVENT
  const evOut = ical.setMasterProps(text, { SUMMARY: 'SUMMARY:renamed event' });
  assert.match(evOut, /SUMMARY:renamed event/);
  assert.match(evOut, /SUMMARY:old title/);
});
