// grid.js — the calendar projections: month grid, week view, agenda list.
// All three render the same expanded-instance cache; navigation adjusts the
// loaded range via store.setRange.

import * as store from '/shared/store.js';
import { $, h, esc, fmtTime, fmtDate, showError } from './main.js';
import { openEventDialog } from './event-dialog.js';
import { openEventDetail } from './event-detail.js';

let view = 'month';
let cursor = startOfDay(new Date());

const DAY = 864e5;
function startOfDay(d) { const x = new Date(d); x.setHours(0, 0, 0, 0); return x; }
function addDays(d, n) { return new Date(+d + n * DAY); }
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
  return { start: startOfDay(cursor), end: addDays(startOfDay(cursor), 30) };
}

export function gridNav(delta) {
  if (delta === 0) cursor = startOfDay(new Date());
  else if (view === 'month') cursor = new Date(cursor.getFullYear(), cursor.getMonth() + delta, 1);
  else if (view === 'week') cursor = addDays(cursor, 7 * delta);
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
  $('#cal-title').textContent = title();
  if (!store.state.range) { reload(); return; }
  const instances = store.visibleInstances();
  main.innerHTML = '';
  if (view === 'month') main.append(renderMonth(instances));
  else if (view === 'week') main.append(renderWeek(instances));
  else main.append(renderAgenda(instances));
}

// ---- month ---------------------------------------------------------------

function chipFor(inst) {
  const color = store.calColor(inst.calHref);
  const tentative = inst.myPartstat === 'TENTATIVE' ? ' tentative' : '';
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
      const cell = h(`<div class="cell${day.getMonth() !== cursor.getMonth() ? ' other' : ''}${sameDay(day, today) ? ' today' : ''}" role="button" tabindex="0"><span class="num">${day.getDate()}</span></div>`);
      // reserve vertical room so single-day chips clear the spanning bars
      if (laneCount) cell.append(h(`<div class="bar-space" style="height:${laneCount * 1.25}rem"></div>`));
      const single = eventsOnDay(instances, day).filter(i => +i.end - +i.start <= DAY);
      for (const inst of single.slice(0, MAX)) cell.append(chipFor(inst));
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
        const bar = h(`<button class="span-bar${s.contLeft ? ' cl' : ''}${s.contRight ? ' cr' : ''}"
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

// ---- week ----------------------------------------------------------------

const HOUR_REM = 3;

function renderWeek(instances) {
  const { start } = range();
  const today = startOfDay(new Date());
  const el = h('<div class="week"></div>');

  el.append(h('<div class="head"></div>'));
  for (let d = 0; d < 7; d++) {
    const day = addDays(start, d);
    el.append(h(`<div class="head${sameDay(day, today) ? ' today' : ''}">${esc(fmtDate(day))}</div>`));
  }

  el.append(h('<div class="allday"></div>'));
  for (let d = 0; d < 7; d++) {
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

  for (let d = 0; d < 7; d++) {
    const day = addDays(start, d);
    const col = h(`<div class="col${sameDay(day, today) ? ' today' : ''}"></div>`);
    for (let hr = 1; hr < 24; hr++) col.append(h(`<div class="hline" style="top:${hr * HOUR_REM}rem"></div>`));
    for (const inst of eventsOnDay(instances, day).filter(i => !i.allDay)) {
      const s = Math.max(0, (inst.start - day) / 36e5);
      const e = Math.min(24, (inst.end - day) / 36e5);
      const block = h(`<button class="block" style="top:${s * HOUR_REM}rem;height:${Math.max(.6, (e - s) * HOUR_REM)}rem;background:${esc(store.calColor(inst.calHref))}">${esc(fmtTime(inst.start))} ${esc(inst.summary)}</button>`);
      block.addEventListener('click', ev => { ev.stopPropagation(); openEventDetail(inst); });
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
      const item = h(`<button class="item">
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
