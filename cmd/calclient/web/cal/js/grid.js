// grid.js — the calendar projections: month grid, week view, agenda list.
// All three render the same expanded-instance cache; navigation adjusts the
// loaded range via store.setRange.

import * as store from '/shared/store.js';
import { $, h, esc, fmtTime, fmtDate, toast, showError } from './main.js';
import { openEventDialog } from './event-dialog.js';
import { openEventDetail } from './event-detail.js';

let view = store.state.prefs.defaultView || 'month';
let cursor = startOfDay(new Date());

// On a phone the 7-column week is unusable; fall back to the single-day view
// on load and whenever the viewport crosses into the narrow range.
const narrowMQ = window.matchMedia('(max-width: 760px)');
if (narrowMQ.matches && view === 'week') view = 'day';
narrowMQ.addEventListener('change', e => {
  if (e.matches && view === 'week' && !$('#topbar').hidden) setGridView('day');
});

const DAY = 864e5;
function startOfDay(d) { const x = new Date(d); x.setHours(0, 0, 0, 0); return x; }
function addDays(d, n) { return new Date(+d + n * DAY); }

// ISO-8601 week number, anchored on the Thursday of the date's ISO week so it
// is stable regardless of the user's Monday/Sunday week-start preference.
function isoWeek(date) {
  const d = new Date(Date.UTC(date.getFullYear(), date.getMonth(), date.getDate()));
  d.setUTCDate(d.getUTCDate() - ((d.getUTCDay() + 6) % 7) + 3);   // Thursday of this week
  const jan4 = new Date(Date.UTC(d.getUTCFullYear(), 0, 4));
  jan4.setUTCDate(jan4.getUTCDate() - ((jan4.getUTCDay() + 6) % 7) + 3);
  return 1 + Math.round((d - jan4) / (7 * DAY));
}
function startOfWeek(d) {
  const ws = store.state.prefs.weekStart ?? 1;
  const x = startOfDay(d);
  const shift = (x.getDay() - ws + 7) % 7;
  return addDays(x, -shift);
}
function sameDay(a, b) { return a.toDateString() === b.toDateString(); }

// visible range for the current view (used for data loading)
function range() {
  if (view === 'month') {
    const first = new Date(cursor.getFullYear(), cursor.getMonth(), 1);
    const start = startOfWeek(first);
    return { start, end: addDays(start, 42) };
  }
  if (view === 'week') {
    const start = startOfWeek(cursor);
    return { start, end: addDays(start, 7) };
  }
  if (view === 'day') {
    const start = startOfDay(cursor);
    return { start, end: addDays(start, 1) };
  }
  return { start: startOfDay(cursor), end: addDays(startOfDay(cursor), 30) };
}

export function gridNav(delta) {
  if (delta === 0) cursor = startOfDay(new Date());
  else if (view === 'month') cursor = new Date(cursor.getFullYear(), cursor.getMonth() + delta, 1);
  else if (view === 'week') cursor = addDays(cursor, 7 * delta);
  else if (view === 'day') cursor = addDays(cursor, delta);
  else cursor = addDays(cursor, 30 * delta);
  reload();
}

export function setGridView(v, anchor) {
  view = v;
  if (anchor) cursor = startOfDay(anchor);
  for (const b of document.querySelectorAll('#view-seg button')) {
    b.classList.toggle('active', b.dataset.view === v);
  }
  reload();
}

function reload() {
  const { start, end } = range();
  store.setRange(start, end).catch(showError);
  renderGrid($('#content'));   // draw immediately from cache; 'events' redraws
}

function title() {
  const { start, end } = range();
  const opts = { month: 'long', year: 'numeric' };
  if (view === 'month') return cursor.toLocaleDateString([], opts);
  if (view === 'day') return cursor.toLocaleDateString([], { weekday: 'long', month: 'long', day: 'numeric', year: 'numeric' });
  const endIncl = addDays(end, -1);
  const sameMonth = start.getMonth() === endIncl.getMonth();
  const s = start.toLocaleDateString([], { month: 'short', day: 'numeric' });
  const e = endIncl.toLocaleDateString([], sameMonth
    ? { day: 'numeric', year: 'numeric' } : { month: 'short', day: 'numeric', year: 'numeric' });
  return `${s} – ${e}`;
}

// instances overlapping [dayStart, dayStart+1d)
function eventsOnDay(instances, day) {
  const s = +day, e = +day + DAY;
  return instances.filter(i => +i.start < e && +i.end > s);
}

export function renderGrid(main) {
  for (const b of document.querySelectorAll('#view-seg button')) b.classList.toggle('active', b.dataset.view === view);
  $('#cal-title').textContent = title();
  if (!store.state.range) { reload(); return; }
  const instances = store.visibleInstances();
  main.innerHTML = '';
  if (view === 'month') main.append(renderMonth(instances));
  else if (view === 'week') main.append(renderTimed(instances, 7));
  else if (view === 'day') main.append(renderTimed(instances, 1));
  else main.append(renderAgenda(instances));
}

// ---- drag to move --------------------------------------------------------
// Pointer-based drag: month chips move between day cells, week/day blocks
// move in 15-minute steps and across day columns. Recurring events need the
// editor's per-occurrence/series choice and attendees can't move someone
// else's meeting, so those show a hint toast instead of dragging.

const DRAG_MIN_PX = 5;

function moveBlocker(inst) {
  if (store.isReadOnly(inst.calHref)) return 'This calendar is shared with you read-only.';
  if (inst.recurring) return 'Recurring events are moved with Edit (one occurrence or the series).';
  const org = inst.organizer?.toLowerCase();
  if (org && org !== store.myEmail().toLowerCase()) return 'Only the organizer can move this event.';
  return '';
}

// After a drop, the browser still fires the click that opening-the-detail and
// create-on-cell-click listen for; swallow that one click at the capture phase.
function swallowNextClick() {
  const stop = e => { e.stopPropagation(); e.preventDefault(); };
  window.addEventListener('click', stop, true);
  setTimeout(() => window.removeEventListener('click', stop, true));
}

async function commitMove(inst, deltaMs) {
  try { await store.moveEvent(inst, deltaMs); toast('Moved.'); }
  catch (e) { showError(e); store.refreshEvents().catch(() => {}); }
}

// month view: drag a chip onto another day cell (keeps the time of day)
function attachChipDrag(chip, inst) {
  chip.addEventListener('pointerdown', ev => {
    if (ev.button !== 0) return;
    const sx = ev.clientX, sy = ev.clientY;
    let ghost = null, target = null;
    const setTarget = cell => {
      if (target === cell) return;
      target?.classList.remove('drop-target');
      target = cell ?? null;
      target?.classList.add('drop-target');
    };
    const stop = () => {
      window.removeEventListener('pointermove', onMove);
      window.removeEventListener('pointerup', onUp);
      window.removeEventListener('pointercancel', stop);
      ghost?.remove(); setTarget(null);
      chip.classList.remove('drag-src');
    };
    const onMove = mv => {
      if (!ghost) {
        if (Math.hypot(mv.clientX - sx, mv.clientY - sy) < DRAG_MIN_PX) return;
        const why = moveBlocker(inst);
        if (why) { toast(why); stop(); return; }
        ghost = chip.cloneNode(true);
        ghost.classList.add('drag-ghost');
        ghost.style.width = chip.getBoundingClientRect().width + 'px';
        document.body.append(ghost);
        chip.classList.add('drag-src');
      }
      ghost.style.left = mv.clientX + 8 + 'px';
      ghost.style.top = mv.clientY + 10 + 'px';
      setTarget(document.elementFromPoint(mv.clientX, mv.clientY)?.closest('.month .cell'));
    };
    const onUp = () => {
      const drop = target, dragged = !!ghost;
      stop();
      if (!dragged) return;
      swallowNextClick();
      if (!drop?.dataset.date) return;
      const deltaMs = +drop.dataset.date - +startOfDay(inst.start);
      if (deltaMs) commitMove(inst, deltaMs);
    };
    window.addEventListener('pointermove', onMove);
    window.addEventListener('pointerup', onUp);
    window.addEventListener('pointercancel', stop);
  });
}

// week/day view: drag a block vertically (15-min snap) and across day columns
function attachBlockDrag(block, inst, day) {
  block.addEventListener('pointerdown', ev => {
    if (ev.button !== 0) return;
    const sx = ev.clientX, sy = ev.clientY;
    const remPx = parseFloat(getComputedStyle(document.documentElement).fontSize) || 16;
    let dragging = false, deltaMs = 0;
    const stop = () => {
      window.removeEventListener('pointermove', onMove);
      window.removeEventListener('pointerup', onUp);
      window.removeEventListener('pointercancel', stop);
      block.classList.remove('dragging');
      block.style.transform = '';
    };
    const onMove = mv => {
      if (!dragging) {
        if (Math.hypot(mv.clientX - sx, mv.clientY - sy) < DRAG_MIN_PX) return;
        const why = moveBlocker(inst);
        if (why) { toast(why); stop(); return; }
        dragging = true;
        block.classList.add('dragging');   // also pointer-events:none, so elementFromPoint sees the column under it
      }
      const snapMin = Math.round((mv.clientY - sy) / (HOUR_REM * remPx) * 60 / 15) * 15;
      const home = block.parentElement;
      const overCol = document.elementFromPoint(mv.clientX, mv.clientY)?.closest('.week .col');
      const dayMs = overCol?.dataset.date ? +overCol.dataset.date - +day : 0;
      deltaMs = dayMs + snapMin * 60000;
      const dx = overCol && overCol !== home
        ? overCol.getBoundingClientRect().left - home.getBoundingClientRect().left : 0;
      block.style.transform = `translate(${dx}px, ${snapMin / 60 * HOUR_REM}rem)`;
    };
    const onUp = () => {
      const d = deltaMs, was = dragging;
      stop();
      if (!was) return;
      swallowNextClick();
      if (d) commitMove(inst, d);
    };
    window.addEventListener('pointermove', onMove);
    window.addEventListener('pointerup', onUp);
    window.addEventListener('pointercancel', stop);
  });
}

// ---- month ---------------------------------------------------------------

// Invitations we haven't confirmed yet ("maybe" or no reply at all) render
// semi-transparent in every view.
function unconfirmedCls(inst) {
  return inst.myPartstat === 'TENTATIVE' || inst.myPartstat === 'NEEDS-ACTION'
    ? ' tentative' : '';
}

function chipFor(inst) {
  const color = store.calColor(inst.calHref);
  const tentative = unconfirmedCls(inst);
  const chip = inst.allDay
    ? h(`<button class="chip${tentative}" style="background:${esc(color)}">${esc(inst.summary)}</button>`)
    : h(`<button class="chip timed${tentative}"><span class="dot" style="background:${esc(color)}"></span>${esc(fmtTime(inst.start))} ${esc(inst.summary)}</button>`);
  chip.addEventListener('click', ev => { ev.stopPropagation(); openEventDetail(inst); });
  return chip;
}

// spans() returns the multi-day events overlapping a week [weekStart, +7d),
// each assigned a lane (greedy) and the columns it covers within the week.
function spans(instances, weekStart) {
  const weekEnd = addDays(weekStart, 7);
  const col = d => Math.min(6, Math.max(0, Math.floor((startOfDay(d) - weekStart) / DAY)));
  const multi = instances
    .filter(i => +i.end - +i.start > DAY && +i.start < +weekEnd && +i.end > +weekStart
      && (col(new Date(+i.end - 1)) > col(i.start) || +i.start < +weekStart))
    .map(i => ({
      inst: i,
      colStart: col(i.start < weekStart ? weekStart : i.start),
      colEnd: col(new Date(Math.min(+i.end - 1, +weekEnd - 1))),
      contLeft: +i.start < +weekStart,
      contRight: +i.end > +weekEnd,
    }))
    .sort((a, b) => a.colStart - b.colStart || (b.colEnd - b.colStart) - (a.colEnd - a.colStart));
  const laneEnds = []; // last covered column per lane
  for (const s of multi) {
    let lane = laneEnds.findIndex(end => end < s.colStart);
    if (lane === -1) { lane = laneEnds.length; laneEnds.push(-1); }
    laneEnds[lane] = s.colEnd;
    s.lane = lane;
  }
  return { multi, laneCount: laneEnds.length };
}

function renderMonth(instances) {
  const { start } = range();
  const today = startOfDay(new Date());
  const el = h('<div class="month"></div>');
  const dow = h('<div class="dow"></div>');
  for (let i = 0; i < 7; i++) {
    dow.append(h(`<div>${addDays(start, i).toLocaleDateString([], { weekday: 'short' })}</div>`));
  }
  el.append(dow);
  const weeks = h('<div class="weeks"></div>');
  for (let w = 0; w < 6; w++) {
    const weekStart = addDays(start, w * 7);
    const { multi, laneCount } = spans(instances, weekStart);
    const row = h('<div class="weeks-row"></div>');
    const MAX = 3;
    for (let d = 0; d < 7; d++) {
      const day = addDays(weekStart, d);
      const cell = h(`<div class="cell${day.getMonth() !== cursor.getMonth() ? ' other' : ''}${sameDay(day, today) ? ' today' : ''}" role="button" tabindex="0" data-date="${+day}"><span class="num">${day.getDate()}</span></div>`);
      // reserve vertical room so single-day chips clear the spanning bars
      if (laneCount) cell.append(h(`<div class="bar-space" style="height:${laneCount * 1.25}rem"></div>`));
      const single = eventsOnDay(instances, day).filter(i => +i.end - +i.start <= DAY);
      for (const inst of single.slice(0, MAX)) {
        const chip = chipFor(inst);
        attachChipDrag(chip, inst);
        cell.append(chip);
      }
      const extra = single.length - MAX;
      if (extra > 0) {
        const more = h(`<button class="more">+${extra} more</button>`);
        more.addEventListener('click', ev => { ev.stopPropagation(); setGridView('agenda', day); });
        cell.append(more);
      }
      const create = () => openEventDialog(null, day);
      cell.addEventListener('click', create);
      cell.addEventListener('keydown', ev => { if (ev.key === 'Enter') create(); });
      row.append(cell);
    }
    // spanning-bar overlay, aligned to the same 7 columns
    if (multi.length) {
      const bars = h('<div class="week-bars"></div>');
      for (const s of multi) {
        const color = store.calColor(s.inst.calHref);
        const bar = h(`<button class="span-bar${s.contLeft ? ' cl' : ''}${s.contRight ? ' cr' : ''}${unconfirmedCls(s.inst)}"
          style="grid-column:${s.colStart + 1}/${s.colEnd + 2};grid-row:${s.lane + 1};background:${esc(color)}"
          title="${esc(s.inst.summary)}">${s.contLeft ? '‹ ' : ''}${esc(s.inst.summary)}</button>`);
        bar.addEventListener('click', ev => { ev.stopPropagation(); openEventDetail(s.inst); });
        bars.append(bar);
      }
      row.append(bars);
    }
    weeks.append(row);
  }
  el.append(weeks);
  return el;
}

// ---- timed grid (week = 7 columns, day = 1) ------------------------------

const HOUR_REM = 3;

// layoutColumns splits a day's timed events into side-by-side columns so that
// overlapping events sit next to each other instead of stacking. Each event is
// tagged with {col, cols}: its column index and the width of its overlap
// cluster (a maximal run of transitively-overlapping events).
function layoutColumns(evs) {
  const sorted = evs.slice().sort((a, b) => a.start - b.start || b.end - a.end);
  const out = [];
  let cluster = [], clusterEnd = -Infinity;
  const flush = () => {
    const cols = cluster.reduce((m, x) => Math.max(m, x.col), 0) + 1;
    for (const x of cluster) { x.cols = cols; out.push(x); }
    cluster = [];
  };
  for (const inst of sorted) {
    const s = +inst.start, e = +inst.end;
    if (cluster.length && s >= clusterEnd) flush();   // no overlap with the run so far
    const used = new Set(cluster.filter(x => +x.inst.end > s).map(x => x.col));
    let col = 0; while (used.has(col)) col++;          // leftmost free column
    cluster.push({ inst, col });
    clusterEnd = Math.max(clusterEnd, e);
  }
  flush();
  return out;
}

function renderTimed(instances, days) {
  const { start } = range();
  const today = startOfDay(new Date());
  const el = h('<div class="week"></div>');
  el.style.gridTemplateColumns = `3.4rem repeat(${days}, minmax(0, 1fr))`;

  // corner cell above the hour gutter carries the ISO week number (Google/Apple
  // do the same). Anchor mid-week so it is right whether the week starts Mon or Sun.
  const wk = isoWeek(addDays(start, days > 1 ? 3 : 0));
  el.append(h(`<div class="head weeknum" title="Week ${wk}">W${wk}</div>`));
  for (let d = 0; d < days; d++) {
    const day = addDays(start, d);
    el.append(h(`<div class="head${sameDay(day, today) ? ' today' : ''}">${esc(fmtDate(day))}</div>`));
  }

  el.append(h('<div class="allday"></div>'));
  for (let d = 0; d < days; d++) {
    const day = addDays(start, d);
    const lane = h('<div class="allday"></div>');
    for (const inst of eventsOnDay(instances, day).filter(i => i.allDay)) lane.append(chipFor(inst));
    el.append(lane);
  }

  const labels = h('<div class="hours"></div>');
  for (let hr = 0; hr < 24; hr++) {
    labels.append(h(`<div class="hourlabel">${hr === 0 ? '' : (store.state.prefs.hour12 ? ((hr % 12) || 12) + (hr < 12 ? ' am' : ' pm') : hr + ':00')}</div>`));
  }
  el.append(labels);

  for (let d = 0; d < days; d++) {
    const day = addDays(start, d);
    const col = h(`<div class="col${sameDay(day, today) ? ' today' : ''}" data-date="${+day}"></div>`);
    for (let hr = 1; hr < 24; hr++) col.append(h(`<div class="hline" style="top:${hr * HOUR_REM}rem"></div>`));
    for (const p of layoutColumns(eventsOnDay(instances, day).filter(i => !i.allDay))) {
      const inst = p.inst;
      const s = Math.max(0, (inst.start - day) / 36e5);
      const e = Math.min(24, (inst.end - day) / 36e5);
      const w = 100 / p.cols;
      const block = h(`<button class="block${unconfirmedCls(inst)}" style="top:${s * HOUR_REM}rem;height:${Math.max(.6, (e - s) * HOUR_REM)}rem;left:calc(${p.col * w}% + 2px);width:calc(${w}% - 4px);right:auto;background:${esc(store.calColor(inst.calHref))}">${esc(fmtTime(inst.start))} ${esc(inst.summary)}</button>`);
      block.addEventListener('click', ev => { ev.stopPropagation(); openEventDetail(inst); });
      attachBlockDrag(block, inst, day);
      col.append(block);
    }
    col.addEventListener('click', ev => {
      const rect = col.getBoundingClientRect();
      const remPx = parseFloat(getComputedStyle(document.documentElement).fontSize) || 16;
      const hr = Math.floor((ev.clientY - rect.top) / (HOUR_REM * remPx));
      const when = new Date(day);
      when.setHours(Math.min(23, Math.max(0, hr)));
      openEventDialog(null, when, true);
    });
    el.append(col);
  }
  return el;
}

// ---- agenda --------------------------------------------------------------

function renderAgenda(instances) {
  const { start, end } = range();
  const today = startOfDay(new Date());
  const el = h('<div class="agenda"></div>');
  let any = false;
  for (let day = new Date(start); day < end; day = addDays(day, 1)) {
    const evs = eventsOnDay(instances, day);
    if (!evs.length) continue;
    any = true;
    el.append(h(`<h3 class="${sameDay(day, today) ? 'today' : ''}">${esc(day.toLocaleDateString([], { weekday: 'long', month: 'long', day: 'numeric' }))}</h3>`));
    for (const inst of evs) {
      const item = h(`<button class="item${unconfirmedCls(inst)}">
        <span class="dot" style="background:${esc(store.calColor(inst.calHref))}"></span>
        <span class="time">${inst.allDay ? 'all day' : esc(fmtTime(inst.start)) + ' – ' + esc(fmtTime(inst.end))}</span>
        <span class="what">${esc(inst.summary)}</span>
        ${inst.location ? `<span class="where">📍 ${esc(inst.location)}</span>` : ''}
      </button>`);
      item.addEventListener('click', () => openEventDetail(inst));
      el.append(item);
    }
  }
  if (!any) el.append(h('<p class="empty">Nothing planned in this period. Click “＋ New event” to add something.</p>'));
  return el;
}
