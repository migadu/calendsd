// tasks.js — the VTODO task cache and task-level edits, mirroring how
// events.js works: calendar-queries into state.tasks, ETag-guarded
// text-surgery writes that preserve alarms and unmodeled properties, and a
// 'tasks' bus event on every change.

import * as dav from '../dav.js';
import * as ical from '../ical.js';
import { DAY, startOfDay } from '../time.js';
import { state, emit, check } from './core.js';
import { getObject, putObject, deleteObject } from './objects.js';
import { loadCalendars } from './calendars.js';

const { NS, key } = dav;

// taskLists returns the calendars that can hold VTODOs, read-only shares
// included (they render view-only).
export function taskLists() {
  return state.calendars.filter(c => c.supportsTasks);
}

// writableTaskLists narrows to lists a new task may be created on.
export function writableTaskLists() {
  return taskLists().filter(c => !c.readOnly);
}

// loadTasks fetches every task list's VTODOs — with no time-range and no
// expand, since many todos carry neither DTSTART nor DUE. Lists whose ctag
// hasn't moved since the last fetch are skipped, so calling this from the
// 30-second poll is cheap. force=true refetches everything (first render).
const fetchedCtags = new Map();   // calHref -> ctag at last fetch
export async function loadTasks(force = false) {
  const stale = taskLists().filter(c =>
    force || !state.tasks.has(c.href) || fetchedCtags.get(c.href) !== c.ctag);
  if (!stale.length) return;
  await Promise.all(stale.map(fetchListTasks));
  // Drop caches for lists that no longer exist.
  for (const href of state.tasks.keys()) {
    if (!state.calendars.some(c => c.href === href)) state.tasks.delete(href);
  }
  emit('tasks');
}

async function fetchListTasks(cal) {
  const body = dav.xmlDoc('c:calendar-query', '', dav.xml`
    <d:prop><d:getetag/><c:calendar-data/></d:prop>
    <c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VTODO"/></c:comp-filter></c:filter>`);
  const r = await dav.report(cal.href, body, '1');
  if (r.status !== 207) return; // keep old data on transient errors
  const out = [];
  for (const resp of dav.msResponses(r.xml)) {
    const data = resp.props.get(key(NS.c, 'calendar-data'))?.textContent;
    if (!data) continue;
    const etag = resp.props.get(key(NS.d, 'getetag'))?.textContent ?? '';
    // Only the master VTODO; recurring-task overrides are out of scope (the
    // master still renders as a single task).
    const todo = ical.components(ical.parse(data), 'VTODO')
      .find(t => !ical.prop(t, 'RECURRENCE-ID'));
    if (todo) out.push(taskFrom(todo, resp.href, cal.href, etag));
  }
  // Open tasks by due date (undated last), completed at the end.
  out.sort((a, b) => (a.done - b.done)
    || (+(a.due?.date ?? Infinity)) - (+(b.due?.date ?? Infinity))
    || a.summary.localeCompare(b.summary));
  state.tasks.set(cal.href, out);
  fetchedCtags.set(cal.href, cal.ctag);
}

export function taskFrom(todo, href, calHref, etag) {
  const dueProp = ical.prop(todo, 'DUE');
  const status = (ical.propValue(todo, 'STATUS') || '').toUpperCase();
  return {
    href, calHref, etag,
    uid: ical.propValue(todo, 'UID'),
    summary: ical.icalUnescape(ical.propValue(todo, 'SUMMARY')) || '(untitled)',
    notes: ical.icalUnescape(ical.propValue(todo, 'DESCRIPTION')),
    due: dueProp ? ical.parseICalDate(dueProp.value, dueProp.params) : null,
    done: status === 'COMPLETED' || !!ical.prop(todo, 'COMPLETED'),
    // Reminders anchor to DUE (RELATED=END); anything we can't model reads back
    // as {custom:true} so the editor keeps it untouched.
    reminders: ical.readReminders(todo, 'END'),
  };
}

// dueBucket groups a task for the list view. Pure (now injectable for tests).
// Completed tasks always bucket 'done' — an overdue-but-finished task must
// not alarm from the Overdue section.
export function dueBucket(task, now = new Date()) {
  if (task.done) return 'done';
  if (!task.due) return 'someday';
  const today = startOfDay(now);
  const dueDay = startOfDay(task.due.date);
  if (+dueDay < +today) return 'overdue';
  if (+dueDay < +today + DAY) return 'today';
  return 'upcoming';
}

// createTask writes a new VTODO to the given list. due: date input value
// (YYYY-MM-DD) or ''.
export async function createTask(calHref, { summary, due, notes, reminders }) {
  const uid = crypto.randomUUID();
  await putObject(calHref + encodeURIComponent(uid) + '.ics',
    ical.buildTodo({ uid, summary, due: due || null, description: notes || '', alarms: reminders || [] }), null);
  await loadTasks(true);
}

// updateTask rewrites the modeled fields on the stored VTODO in place,
// preserving alarms and properties the editor doesn't model. reminders is a
// list of trigger durations to write (anchored to DUE), or null/undefined to
// leave the stored alarms untouched (the editor's "kept as is" state).
export async function updateTask(task, { summary, due, notes, reminders }) {
  const { text, etag } = await getObject(task.href);
  const props = {
    SUMMARY: `SUMMARY:${ical.icalEscape(summary)}`,
    DUE: due ? `DUE;VALUE=DATE:${ical.dateOnly(due)}` : null,
    DESCRIPTION: notes ? `DESCRIPTION:${ical.icalEscape(notes)}` : null,
  };
  let out = ical.setMasterProps(text, props, 'VTODO');
  if (reminders != null) out = ical.setMasterAlarms(out, reminders, summary, 'VTODO', 'END');
  await putObject(task.href, out, etag);
  await loadTasks(true);
}

// setTaskDone toggles completion, RFC-style: STATUS plus the COMPLETED
// timestamp and PERCENT-COMPLETE both set or both removed.
export async function setTaskDone(task, done) {
  const { text, etag } = await getObject(task.href);
  const props = done ? {
    STATUS: 'STATUS:COMPLETED',
    COMPLETED: `COMPLETED:${ical.utc(new Date().toISOString())}`,
    'PERCENT-COMPLETE': 'PERCENT-COMPLETE:100',
  } : {
    STATUS: 'STATUS:NEEDS-ACTION',
    COMPLETED: null,
    'PERCENT-COMPLETE': null,
  };
  await putObject(task.href, ical.setMasterProps(text, props, 'VTODO'), etag);
  await loadTasks(true);
}

export async function deleteTask(task) {
  await deleteObject(task.href);
  await loadTasks(true);
}

// createTaskList makes a VTODO-only calendar and refreshes the calendar list
// (which the tasks view watches).
export async function createTaskList(name, displayName) {
  const body = dav.xmlDoc('c:mkcalendar', '', dav.xml`<d:set><d:prop>
    <d:displayname>${displayName}</d:displayname>
    <c:supported-calendar-component-set><c:comp name="VTODO"/></c:supported-calendar-component-set>
  </d:prop></d:set>`);
  check(await dav.req('MKCALENDAR', state.calHome + encodeURIComponent(name) + '/', {
    headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
  }), [201]);
  await loadCalendars();
}
