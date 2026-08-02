// tasks.js — the Tasks view: a quick-add row, Overdue / Today / Upcoming /
// Someday sections over the VTODO cache, completion checkboxes, and an edit
// dialog. Deliberately a list, not a grid projection — tasks are a checklist
// interaction, and many carry no date at all.

import * as store from '/shared/store.js';
import { $, html, frag } from '/shared/dom.js';
import { localDate } from '/shared/time.js';
import { fmtDate, fmtTime, toast, showError } from './ui.js';
import { openOverlay, confirmDialog, promptDialog } from '/shared/dialogs.js';

const SECTIONS = [
  ['overdue', 'Overdue'], ['today', 'Today'],
  ['upcoming', 'Upcoming'], ['someday', 'Someday'],
];
let showDone = false;   // view state, deliberately not a persisted pref

export function renderTasks(main) {
  main.innerHTML = '';
  const lists = store.taskLists();
  const el = html`<div class="tasks"></div>`;
  main.append(el);

  if (!lists.length) {
    el.append(html`<p class="empty">No task list yet. Tasks live on a task-capable
      calendar (VTODO) — create one to start.</p>`);
    el.append(newListButton());
    return;
  }

  store.loadTasks().catch(showError);   // ctag-guarded; 'tasks' re-renders

  // quick-add: title + optional due date, into the picked (or only) list
  const writable = store.writableTaskLists();
  if (writable.length) {
    const add = html`<form class="task-add">
      <input type="text" id="tk-new" placeholder="Add a task…" required>
      <input type="date" id="tk-due" title="Due date (optional)">
      ${writable.length > 1 && frag`<select id="tk-list">
        ${writable.map(c => frag`<option value="${c.href}">${c.displayName || c.name}</option>`)}
      </select>`}
      <button type="submit" class="primary">Add</button>
    </form>`;
    add.addEventListener('submit', async ev => {
      ev.preventDefault();
      const btn = $('button', add);
      btn.disabled = true;
      try {
        await store.createTask($('#tk-list', add)?.value ?? writable[0].href, {
          summary: $('#tk-new', add).value.trim(),
          due: $('#tk-due', add).value,
        });
        toast('Task added.');
      } catch (e) { showError(e); btn.disabled = false; }
    });
    el.append(add);
  }

  // bucket every list's tasks
  const buckets = new Map(SECTIONS.map(([k]) => [k, []]));
  buckets.set('done', []);
  for (const list of lists) {
    for (const t of store.state.tasks.get(list.href) ?? []) {
      buckets.get(store.dueBucket(t)).push(t);
    }
  }

  let any = false;
  for (const [k, label] of SECTIONS) {
    const tasks = buckets.get(k);
    if (!tasks.length) continue;
    any = true;
    const sec = html`<section class="task-section${k === 'overdue' ? ' overdue' : ''}">
      <h3>${label}</h3></section>`;
    for (const t of tasks) sec.append(taskRow(t));
    el.append(sec);
  }
  const done = buckets.get('done');
  if (done.length) {
    const toggle = html`<button class="ghost task-done-toggle">${showDone
      ? '▾ Hide completed' : `▸ Show completed (${done.length})`}</button>`;
    toggle.addEventListener('click', () => { showDone = !showDone; renderTasks(main); });
    el.append(toggle);
    if (showDone) {
      const sec = html`<section class="task-section done"><h3>Completed</h3></section>`;
      for (const t of done) sec.append(taskRow(t));
      el.append(sec);
    }
  }
  if (!any && !done.length) {
    el.append(html`<p class="empty">Nothing to do. Add a task above.</p>`);
  }
  el.append(newListButton());
}

function fmtDue(due) {
  return due.allDay ? fmtDate(due.date) : `${fmtDate(due.date)} ${fmtTime(due.date)}`;
}

function taskRow(t) {
  const ro = store.isReadOnly(t.calHref);
  const row = html`<div class="task-row${t.done ? ' done' : ''}">
    ${ro ? frag`<span class="task-check" aria-hidden="true"></span>`
         : frag`<input type="checkbox" class="task-check" ${t.done ? 'checked' : ''}
             aria-label="Mark ${t.summary} ${t.done ? 'open' : 'done'}">`}
    <button class="task-main" ${ro ? 'disabled' : ''}>
      <span class="task-title">${t.summary}</span>
      ${t.due && frag`<span class="task-due">${fmtDue(t.due)}</span>`}
      ${t.notes && frag`<span class="task-notes">${t.notes}</span>`}
    </button>
    <span class="dot" style="background:${store.calColor(t.calHref)}"
      title="${store.state.calendars.find(c => c.href === t.calHref)?.displayName ?? ''}"></span>
  </div>`;
  $('input.task-check', row)?.addEventListener('change', async ev => {
    ev.target.disabled = true;
    try { await store.setTaskDone(t, ev.target.checked); }
    catch (e) { showError(e); store.loadTasks(true).catch(() => {}); }
  });
  if (!ro) $('.task-main', row).addEventListener('click', () => taskDialog(t));
  return row;
}

// taskDialog edits title / due / notes, with Delete.
function taskDialog(t) {
  const dlg = html`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="Edit task">
    <h2>Edit task</h2>
    <div class="grid">
      <label class="full">Title<input id="tk-title" type="text" value="${t.summary}"></label>
      <label>Due<input id="tk-edit-due" type="date" value="${t.due?.allDay ? localDate(t.due.date) : ''}"></label>
      <label class="full">Notes<textarea id="tk-notes" rows="3">${t.notes}</textarea></label>
    </div>
    <p class="error" id="tk-error"></p>
    <div class="actions">
      <button class="danger" id="tk-delete">Delete…</button>
      <button id="tk-cancel">Cancel</button>
      <button class="primary" id="tk-save">Save</button>
    </div>
  </div></div>`;
  const close = () => dlg.remove();
  openOverlay(dlg, close);
  $('#tk-cancel', dlg).addEventListener('click', close);
  $('#tk-save', dlg).addEventListener('click', async () => {
    const summary = $('#tk-title', dlg).value.trim();
    if (!summary) { $('#tk-error', dlg).textContent = 'Please enter a title.'; return; }
    try {
      await store.updateTask(t, {
        summary,
        due: $('#tk-edit-due', dlg).value,
        notes: $('#tk-notes', dlg).value.trim(),
      });
      close();
      toast('Task saved.');
    } catch (e) {
      if (e.conflict) { $('#tk-error', dlg).textContent = e.message; store.loadTasks(true).catch(() => {}); }
      else showError(e);
    }
  });
  $('#tk-delete', dlg).addEventListener('click', async () => {
    if (!await confirmDialog('Delete this task?', { confirmLabel: 'Delete', danger: true })) return;
    try { await store.deleteTask(t); close(); toast('Task deleted.'); }
    catch (e) { showError(e); }
  });
  $('#tk-title', dlg).focus();
}

function newListButton() {
  const b = html`<button class="ghost task-new-list">＋ New task list</button>`;
  b.addEventListener('click', async () => {
    const name = await promptDialog('Name for the new task list', { confirmLabel: 'Create' });
    if (!name) return;
    try {
      await store.createTaskList(name.toLowerCase().replace(/[^a-z0-9-]+/g, '-'), name);
      toast(`Task list “${name}” created.`);
    } catch (e) { showError(e); }
  });
  return b;
}
