// grid.js — the calendar projections: month grid, week view, agenda list.
// All three render the same expanded-instance cache; navigation adjusts the
// loaded range via store.setRange. The pure placement math (overlap columns,
// multi-day lanes) lives in layout.js.

import * as store from '/shared/store.js';
import { $, html, frag } from '/shared/dom.js';
import { DAY, HOUR, startOfDay, addDays, sameDay, nextDate, isoWeek,
  startOfWeek as weekStartOf, tzDayStart as tzDayStartIn } from '/shared/time.js';
import { layoutColumns, spans } from './layout.js';
import { fmtTime, fmtDate, toast, showError } from './ui.js';
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

// Preference-aware wrappers over the pure helpers in /shared/time.js.
function tzDayStart(day) { return tzDayStartIn(day, store.state.prefs.tz); }
function startOfWeek(d) { return weekStartOf(d, store.state.prefs.weekStart ?? 1); }

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
  centerNextRender = true;
  reload();
}

export function setGridView(v, anchor) {
  view = v;
  centerNextRender = true;
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

// instances overlapping the calendar date `day`, with the day's boundaries
// taken in the display zone so an event near midnight lands in the right column.
function eventsOnDay(instances, day) {
  const s = +tzDayStart(day), e = +tzDayStart(nextDate(day));
  return instances.filter(i => +i.start < e && +i.end > s);
}

// centerNextRender scrolls the next timed render to the current-time line
// instead of preserving the viewport. Set by intentional navigation (Today,
// prev/next, switching to week/day); cleared once consumed, so background
// refreshes (the 30s poll) keep the user's scroll position steady.
let centerNextRender = true;

export function renderGrid(main) {
  for (const b of document.querySelectorAll('#view-seg button')) b.classList.toggle('active', b.dataset.view === view);
  $('#cal-title').textContent = title();
  if (!store.state.range) { reload(); return; }
  const instances = store.visibleInstances();
  const prevScroll = main.scrollTop;
  main.innerHTML = '';
  if (view === 'month') main.append(renderMonth(instances));
  else if (view === 'week') main.append(renderTimed(instances, 7));
  else if (view === 'day') main.append(renderTimed(instances, 1));
  else main.append(renderAgenda(instances));
  // Timed views scroll; keep the viewport where it was across a re-render,
  // except right after navigation, when we jump to the current-time line.
  if (view === 'week' || view === 'day') {
    if (centerNextRender) { centerOnNow(main); centerNextRender = false; }
    else main.scrollTop = prevScroll;
  }
}

// nowTopRem is the current time's offset (in rem) down the 24-hour column,
// measured in the display zone so it lines up with the zone's hour labels.
function nowTopRem() {
  const n = new Date();
  return Math.max(0, (n - tzDayStart(n)) / HOUR) * HOUR_REM;
}

// centerOnNow scrolls the timed grid so the current-time line sits mid-viewport
// (Google-style). With no line visible (today isn't in range) it rests at the
// top rather than leaving the previous position dangling.
function centerOnNow(main) {
  const line = main.querySelector('.now-line');
  if (!line) { main.scrollTop = 0; return; }
  const target = main.scrollTop + (line.getBoundingClientRect().top - main.getBoundingClientRect().top)
    - main.clientHeight / 2;
  main.scrollTop = Math.max(0, target);
}

// Nudge the current-time line down every minute without a full re-render.
setInterval(() => {
  const top = nowTopRem() + 'rem';
  for (const line of document.querySelectorAll('.week .now-line')) line.style.top = top;
}, 60_000);

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
// semi-transparent in every view; a no-reply-at-all additionally gets a
// dashed edge so it visibly asks to be answered. A cancelled event is
// struck through until the invitations panel removes it.
function unconfirmedCls(inst) {
  if (inst.cancelled) return ' tentative cancelled';
  // An unanswered invite must stand out, so it is NOT dimmed like a "maybe":
  // needs-action gets the bold dashed edge + "?" badge (see cal.css) at full
  // opacity; only a tentative "maybe" reply gets the faded/italic treatment.
  return inst.myPartstat === 'NEEDS-ACTION' ? ' needs-action'
    : inst.myPartstat === 'TENTATIVE' ? ' tentative' : '';
}

function chipFor(inst) {
  const color = store.calColor(inst.calHref);
  const tentative = unconfirmedCls(inst);
  const key = store.instanceKey(inst);
  const chip = inst.allDay
    ? html`<button class="chip${tentative}" data-key="${key}" style="background:${color}">${inst.summary}</button>`
    : html`<button class="chip timed${tentative}" data-key="${key}"><span class="dot" style="background:${color}"></span>${fmtTime(inst.start)} ${inst.summary}</button>`;
  chip.addEventListener('click', ev => { ev.stopPropagation(); openEventDetail(inst); });
  return chip;
}

// flashInstances briefly highlights the grid elements for the given instance
// keys (see store.instanceKey), drawing the eye to events that just changed
// under an incoming push. Returns how many elements were on screen to flash, so
// the caller can fall back to a toast when the change is out of view.
export function flashInstances(keys) {
  const want = new Set(keys);
  let n = 0;
  for (const el of document.querySelectorAll('#content [data-key]')) {
    if (!want.has(el.dataset.key)) continue;
    el.classList.remove('flash-change');
    void el.offsetWidth;            // restart the animation if it is re-flashed
    el.classList.add('flash-change');
    setTimeout(() => el.classList.remove('flash-change'), 3200);
    n++;
  }
  return n;
}

function renderMonth(instances) {
  const { start } = range();
  const today = startOfDay(new Date());
  const el = html`<div class="month"></div>`;
  const dow = html`<div class="dow"></div>`;
  for (let i = 0; i < 7; i++) {
    dow.append(html`<div>${addDays(start, i).toLocaleDateString([], { weekday: 'short' })}</div>`);
  }
  el.append(dow);
  const weeks = html`<div class="weeks"></div>`;
  for (let w = 0; w < 6; w++) {
    const weekStart = addDays(start, w * 7);
    const { multi, laneCount } = spans(instances, weekStart);
    const row = html`<div class="weeks-row"></div>`;
    const MAX = 3;
    for (let d = 0; d < 7; d++) {
      const day = addDays(weekStart, d);
      const cell = html`<div class="cell${day.getMonth() !== cursor.getMonth() ? ' other' : ''}${sameDay(day, today) ? ' today' : ''}" role="button" tabindex="0" data-date="${+day}"><span class="num">${day.getDate()}</span></div>`;
      // reserve vertical room so single-day chips clear the spanning bars
      if (laneCount) cell.append(html`<div class="bar-space" style="height:${laneCount * 1.25}rem"></div>`);
      const single = eventsOnDay(instances, day).filter(i => +i.end - +i.start <= DAY);
      for (const inst of single.slice(0, MAX)) {
        const chip = chipFor(inst);
        attachChipDrag(chip, inst);
        cell.append(chip);
      }
      const extra = single.length - MAX;
      if (extra > 0) {
        const more = html`<button class="more">+${extra} more</button>`;
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
      const bars = html`<div class="week-bars"></div>`;
      for (const s of multi) {
        const color = store.calColor(s.inst.calHref);
        const bar = html`<button class="span-bar${s.contLeft ? ' cl' : ''}${s.contRight ? ' cr' : ''}${unconfirmedCls(s.inst)}"
          data-key="${store.instanceKey(s.inst)}"
          style="grid-column:${s.colStart + 1}/${s.colEnd + 2};grid-row:${s.lane + 1};background:${color}"
          title="${s.inst.summary}">${s.contLeft ? '‹ ' : ''}${s.inst.summary}</button>`;
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

function renderTimed(instances, days) {
  const { start } = range();
  const today = startOfDay(new Date());
  const el = html`<div class="week"></div>`;
  el.style.gridTemplateColumns = `3.4rem repeat(${days}, minmax(0, 1fr))`;

  // corner cell above the hour gutter carries the ISO week number (Google/Apple
  // do the same). Anchor mid-week so it is right whether the week starts Mon or Sun.
  const wk = isoWeek(addDays(start, days > 1 ? 3 : 0));
  el.append(html`<div class="head weeknum" title="Week ${wk}">W${wk}</div>`);
  for (let d = 0; d < days; d++) {
    const day = addDays(start, d);
    el.append(html`<div class="head${sameDay(day, today) ? ' today' : ''}">${fmtDate(day)}</div>`);
  }

  el.append(html`<div class="allday"></div>`);
  for (let d = 0; d < days; d++) {
    const day = addDays(start, d);
    const lane = html`<div class="allday"></div>`;
    for (const inst of eventsOnDay(instances, day).filter(i => i.allDay)) lane.append(chipFor(inst));
    el.append(lane);
  }

  const labels = html`<div class="hours"></div>`;
  for (let hr = 0; hr < 24; hr++) {
    labels.append(html`<div class="hourlabel">${hr === 0 ? '' : (store.state.prefs.hour12 ? ((hr % 12) || 12) + (hr < 12 ? ' am' : ' pm') : hr + ':00')}</div>`);
  }
  el.append(labels);

  for (let d = 0; d < days; d++) {
    const day = addDays(start, d);
    const top = tzDayStart(day);   // this column's midnight in the display zone
    const col = html`<div class="col${sameDay(day, today) ? ' today' : ''}" data-date="${+day}"></div>`;
    for (let hr = 1; hr < 24; hr++) col.append(html`<div class="hline" style="top:${hr * HOUR_REM}rem"></div>`);
    for (const p of layoutColumns(eventsOnDay(instances, day).filter(i => !i.allDay))) {
      const inst = p.inst;
      const s = Math.max(0, (inst.start - top) / HOUR);
      const e = Math.min(24, (inst.end - top) / HOUR);
      const w = 100 / p.cols;
      const block = html`<button class="block${unconfirmedCls(inst)}" data-key="${store.instanceKey(inst)}" style="top:${s * HOUR_REM}rem;height:${Math.max(.6, (e - s) * HOUR_REM)}rem;left:calc(${p.col * w}% + 2px);width:calc(${w}% - 4px);right:auto;background:${store.calColor(inst.calHref)}">${fmtTime(inst.start)} ${inst.summary}</button>`;
      block.addEventListener('click', ev => { ev.stopPropagation(); openEventDetail(inst); });
      attachBlockDrag(block, inst, day);
      col.append(block);
    }
    // the current-time line, drawn only in today's column
    if (sameDay(day, today)) col.append(html`<div class="now-line" style="top:${nowTopRem()}rem"></div>`);
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
  const el = html`<div class="agenda"></div>`;
  let any = false;
  for (let day = new Date(start); day < end; day = addDays(day, 1)) {
    const evs = eventsOnDay(instances, day);
    if (!evs.length) continue;
    any = true;
    el.append(html`<h3 class="${sameDay(day, today) ? 'today' : ''}">${day.toLocaleDateString([], { weekday: 'long', month: 'long', day: 'numeric' })}</h3>`);
    for (const inst of evs) {
      const item = html`<button class="item${unconfirmedCls(inst)}" data-key="${store.instanceKey(inst)}">
        <span class="dot" style="background:${store.calColor(inst.calHref)}"></span>
        <span class="time">${inst.allDay ? 'all day' : fmtTime(inst.start) + ' – ' + fmtTime(inst.end)}</span>
        <span class="what">${inst.summary}</span>
        ${inst.location && frag`<span class="where">📍 ${inst.location}</span>`}
      </button>`;
      item.addEventListener('click', () => openEventDetail(inst));
      el.append(item);
    }
  }
  if (!any) el.append(html`<p class="empty">Nothing planned in this period. Click “＋ New event” to add something.</p>`);
  return el;
}
