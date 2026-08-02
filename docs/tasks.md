# Tasks (VTODO) in calweb — Design & Implementation Plan

## Problem

The server fully supports VTODO task lists (verified against the dev server:
VTODO-only MKCALENDAR → 201, VTODO PUT → 201, `calendar-query` with a VTODO
comp-filter and **no time-range** returns the objects, and the component set is
enforced — a VEVENT PUT into a task list 403s). Other CalDAV clients create
task lists. calweb deliberately hides them: the sidebar filters on
`supportsEvents`, so an account's tasks are simply invisible. Journal
(VJOURNAL) stays out — near-zero client ecosystem, all-new UI, no demand.

## Approach: a list view, not a grid projection

Tasks are a list interaction (check off, group by due), not a calendar
projection. v1 adds a **Tasks view** beside Calendar and Contacts, backed by a
`store/tasks.js` module that mirrors how events work (calendar-query into an
in-memory cache, ETag-guarded text-surgery writes, bus events, re-render).

The nav item is **always visible once signed in** — not capability-gated and
not a settings toggle. A settings switch would be a feature flag posing as a
preference; capability-gating (the earlier idea) has a bootstrap dead end: with
no task-capable calendar the view is unreachable, so the "create your first
task list" affordance could never be seen. One extra word in the nav is cheaper
than either. The view's empty state offers **New task list**.

## Components

### 1. `store/calendars.js` — one new flag

`supportsTasks: comps.length === 0 || comps.includes('VTODO')` beside the
existing `supportsEvents` (absent set = all component types, per RFC 4791).
`taskLists()` (exported from the tasks module) = calendars with
`supportsTasks`, including read-only shares (view-only there).

### 2. `shared/ical.js` — VTODO build + generalized surgery

- `buildTodo({uid, summary, due, allDayDue, description})` → single-VTODO
  VCALENDAR, mirroring `buildEvent`. `DUE` as `VALUE=DATE` or UTC DATE-TIME.
- **Generalize `masterEventRange`/`setMasterProps` with a component parameter**
  (`comp = 'VEVENT'` default, so every existing call site is untouched):
  `setMasterProps(text, props, 'VTODO')` edits the first VTODO without a
  RECURRENCE-ID and preserves VALARMs and unmodeled properties, exactly as it
  does for events. `addExdate`/`addOverride` stay VEVENT-only (recurring tasks
  are out of scope).

### 3. New module: `shared/store/tasks.js`

- `state.tasks: Map(calHref → [task])` (added to `core.js` state) +
  `emit('tasks')`.
- Task model from `parseICalDate`/`propValue`:
  `{href, calHref, etag, uid, summary, notes, due: {allDay, date} | null,
  done, priority}`. `done` = `STATUS:COMPLETED` (or `COMPLETED` present).
- `loadTasks()` — calendar-query per task list (VTODO comp-filter, **no
  time-range, no expand**: many todos have neither DTSTART nor DUE). Keeps its
  own `calHref → ctag` map and skips lists whose ctag hasn't moved, so the
  30-second poll refresh is cheap. Non-207 keeps old data (transient-error
  behavior copied from events).
- `saveTask(model, calHref)` — create via `buildTodo` +
  `putObject(href, …, null)`; edit via `setMasterProps(text, props, 'VTODO')`
  on the fetched object (SUMMARY / DUE / DESCRIPTION), preserving alarms and
  X-props. ETag conflicts surface through the existing 412 path.
- `setTaskDone(task, done)` — `setMasterProps` with
  `STATUS:COMPLETED` + `COMPLETED:<utc now>` + `PERCENT-COMPLETE:100`, or
  `STATUS:NEEDS-ACTION` + `COMPLETED: null` + `PERCENT-COMPLETE: null`.
- `deleteTask(task)` — `deleteObject`.
- `createTaskList(name, displayName)` — MKCALENDAR with a VTODO-only
  component set, then `loadCalendars()`.
- `dueBucket(task, now) → 'overdue' | 'today' | 'upcoming' | 'someday'` —
  pure, exported for unit tests; the view groups with it.
- Re-export the public names from the `shared/store.js` facade.

### 4. View: `cal/js/tasks.js` + routing

- Route `#tasks` in `main.js` (`currentView` list, `render()` dispatch), nav
  link in `cal/index.html`. The existing
  `body:not([data-view="calendar"])` rule already hides the calendar sidebar
  and date controls.
- Layout: a quick-add row (title input + optional due date + task-list picker,
  shown when >1 writable list), then sections **Overdue / Today / Upcoming /
  Someday** via `dueBucket`, each task a row: checkbox, title, due label
  (`fmtDate`), list dot in the calendar's color. Completed tasks live behind a
  "Show completed" toggle (plain view state, not a pref, for v1).
- Row click → edit dialog (title, due, notes, Delete) built with `html`/`frag`,
  `openOverlay`, and `confirmDialog(..., {danger: true})` for delete. Tasks on
  read-only shares render without checkbox/edit (same `isReadOnly` rule the
  editor uses).
- Empty state (no task-capable calendar): explainer + **New task list** via
  `promptDialog`.
- Refresh: the view loads on open and re-runs `loadTasks()` on the `calendars`
  bus event (the 30 s ctag poll) while it is the active view; `tasks` bus
  events re-render. No store-level cross-wiring.

### 5. Tests

- `tests/ical.test.js`: `buildTodo` output; `setMasterProps` with the VTODO
  component (edits the todo, leaves a sibling VEVENT and nested VALARM alone;
  default parameter still targets VEVENT).
- New `tests/tasks.test.js` (the store module is import-light but pulls
  `core.js`; the pure parts — model parsing helpers and `dueBucket` — live so
  they can be imported without DOM): due-date forms (DATE, UTC DATE-TIME,
  absent), done detection, bucketing edges (due 23:59 today vs. tomorrow,
  overdue-but-completed stays out of Overdue).
- e2e `tasks.spec.js`: create the task list if absent, quick-add a uniquely
  named task, toggle it complete, reopen and verify; `afterEach` sweeps with
  `deleteEventsContaining` (it crawls all collections and matches any `.ics`
  body, so it covers task objects unchanged).

## Explicitly out of scope for v1

- **Recurring tasks** (VTODO recurrence-on-completion semantics), subtasks
  (`RELATED-TO`), priority editing (stored, untouched, not displayed).
- **Grid integration** (due-task chips in the calendar) — revisit once the
  list view has proven itself.
- **WebDAV-Push for task lists** — the 30 s poll covers v1;
  `push-sync.js` subscribes event calendars only.
- Task import via the Settings `.ics` import (it targets `defaultCal`, which
  is VEVENT-shaped; imports containing VTODOs can be a follow-up).

## Scope

Two new files (`store/tasks.js` ~130 lines, `cal/js/tasks.js` ~180 lines),
small edits (`calendars.js` flag, `core.js` state, `ical.js` buildTodo +
component parameter, `main.js`/`index.html` routing, `cal.css` styles), unit +
e2e tests. No Go changes, no schema changes, no new protocol.
