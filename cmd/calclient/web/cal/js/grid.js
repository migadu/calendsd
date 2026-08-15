// grid.js — the calendar projections: month grid, week view, agenda list.
// All three render the same expanded-instance cache; navigation adjusts the
// loaded range via store.setRange. The pure placement math (overlap columns,
// multi-day lanes) lives in layout.js.

import * as store from '/shared/store.js';
import { $, html, frag } from '/shared/dom.js';
import { icon } from '/shared/icons.js';
import { DAY, HOUR, startOfDay, addDays, sameDay, nextDate, isoWeek,
  startOfWeek as weekStartOf, tzDayStart as tzDayStartIn } from '/shared/time.js';
import { layoutColumns, spans } from './layout.js';
import { fmtTime, fmtDate, toast, showError } from './ui.js';
import { openEventDialog } from './event-dialog.js';
import * as router from './router.js';

// The grid view (day/week/month/agenda), the navigated date, and the agenda's
// search filter all live in the URL — the router is the source of truth. These
// module vars mirror the current route; applyCalendarRoute() writes them from a
// parsed route, and setGridView/gridNav/the search box push updates back out.
const VIEWS = ['day', 'week', 'month', 'agenda'];
let agendaQuery = '';
let view = store.state.prefs.defaultView || 'month';
let cursor = startOfDay(new Date());

// Views that need to mirror the cursor (the sidebar mini-month) subscribe
// here; applyCalendarRoute notifies after every intentional navigation.
const cursorListeners = new Set();
export function onCursorChange(fn) { cursorListeners.add(fn); }
function notifyCursor() { for (const fn of cursorListeners) fn(); }

function parseLocalDate(s) {
  const m = /^(\d{4})-(\d{2})-(\d{2})/.exec(s || '');
  return m ? new Date(+m[1], +m[2] - 1, +m[3]) : startOfDay(new Date());
}

// applyCalendarRoute sets the view/date/filter from a /cal/calendar/… route and
// redraws. Called by main.js's router handler, so it runs on every intentional
// calendar navigation (not on background refreshes, which reuse these vars).
export function applyCalendarRoute({ view: v, date, q } = {}) {
  let nextView = VIEWS.includes(v) ? v : (store.state.prefs.defaultView || 'month');
  if (narrowMQ.matches && nextView === 'week') nextView = 'day';
  const nextCursor = date ? parseLocalDate(date) : startOfDay(new Date());
  // Jump a timed view to the now-line only when the view or day actually
  // changed. Re-entering the same route — e.g. closing an event overlay, which
  // navigates back to the current calendar path — must keep the user's scroll.
  if (nextView !== view || +nextCursor !== +cursor) centerNextRender = true;
  view = nextView;
  cursor = nextCursor;
  agendaQuery = q || '';
  reload();
  notifyCursor();
}

// currentCalendarRoute is the state main.js needs to rebuild the calendar path
// (e.g. when closing an event overlay back to where the grid was).
export function currentCalendarRoute() { return { view, date: cursor, q: agendaQuery }; }

// currentRange exposes the loaded window (start inclusive, end exclusive) so
// the mini-month can tint the days the main view is showing.
export function currentRange() { return range(); }

// Opening an event is a navigation now (/cal/event/<uid>), so it lands in the
// URL and the Back button closes it. A recurring occurrence carries its start so
// the exact instance reopens on reload.
function openEvent(inst) {
  router.navigate(router.eventPath(inst.uid, { occ: inst.recurring ? inst.start : null }));
}

// On a phone the 7-column week is unusable; fall back to the single-day view
// on load and whenever the viewport crosses into the narrow range.
const narrowMQ = window.matchMedia('(max-width: 760px)');
if (narrowMQ.matches && view === 'week') view = 'day';
narrowMQ.addEventListener('change', e => {
  // replace, not push: a viewport crossing (window drag, tablet rotate) isn't a
  // user view-switch, so it must not stack history entries.
  if (e.matches && view === 'week' && !$('#topbar').hidden) setGridView('day', null, { replace: true });
});

// Preference-aware wrappers over the pure helpers in /shared/time.js.
function tzDayStart(day) { return tzDayStartIn(day, store.state.prefs.tz); }
function startOfWeek(d) { return weekStartOf(d, store.state.prefs.weekStart ?? 1); }

// monthWeeks is how many week-rows the month view needs: five when the month
// fits (e.g. 28-day February starting on the week's first day), six otherwise.
// A fixed six rows used to leave a dead strip at the bottom of those months.
function monthWeeks() {
  const first = new Date(cursor.getFullYear(), cursor.getMonth(), 1);
  const lead = (first.getDay() - (store.state.prefs.weekStart === 0 ? 0 : 1) + 7) % 7;
  const dim = new Date(cursor.getFullYear(), cursor.getMonth() + 1, 0).getDate();
  return Math.ceil((lead + dim) / 7);
}

// visible range for the current view (used for data loading)
function range() {
  if (view === 'month') {
    const first = new Date(cursor.getFullYear(), cursor.getMonth(), 1);
    const start = startOfWeek(first);
    return { start, end: addDays(start, monthWeeks() * 7) };
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

// Date navigation (prev/next/Today) writes the new date into the URL — replace,
// not push, so holding next doesn't flood the history. The router handler calls
// applyCalendarRoute, which recomputes the cursor and redraws.
export function gridNav(delta) {
  let c;
  if (delta === 0) c = startOfDay(new Date());
  else if (view === 'month') c = new Date(cursor.getFullYear(), cursor.getMonth() + delta, 1);
  else if (view === 'week') c = addDays(cursor, 7 * delta);
  else if (view === 'day') c = addDays(cursor, delta);
  else c = addDays(cursor, 30 * delta);
  router.navigate(router.calendarPath({ view, date: c, q: agendaQuery }), { replace: true });
}

// Switching view pushes a new history entry (so Back returns to the prior view).
// Leaving the agenda drops the filter from the path; entering it keeps whatever
// term is set. `anchor` (from month's "+more") sets the date being expanded.
// `replace` swaps the entry instead of pushing — for non-user switches like the
// narrow-screen week→day coercion that shouldn't grow the history.
export function setGridView(v, anchor, { replace = false } = {}) {
  const date = anchor ? startOfDay(anchor) : cursor;
  router.navigate(router.calendarPath({ view: v, date, q: v === 'agenda' ? agendaQuery : '' }), { replace });
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
  for (const b of document.querySelectorAll('#view-seg button')) {
    const on = b.dataset.view === view;
    b.classList.toggle('active', on);
    b.setAttribute('aria-pressed', String(on));
  }
  $('#cal-title').textContent = title();
  // First paint: the calendar list (and with it the range fetch) hasn't
  // landed yet — say so instead of drawing a confidently empty grid. An
  // account with no event-capable calendars gets an explicit empty state
  // pointing at the creation flow (the sidebar is off-canvas on phones).
  if (!store.state.calendarsLoaded) {
    main.replaceChildren(html`<div class="ct-loading"><span class="spinner" aria-hidden="true"></span>Loading your calendar…</div>`);
    // Only kick the initial range load when there isn't one yet — reload()
    // re-enters renderGrid synchronously (setRange assigns state.range before
    // its first await), so an unconditional call here would recurse until
    // the calendar list lands.
    if (!store.state.range) reload();
    return;
  }
  if (!store.state.calendars.some(c => c.supportsEvents)) {
    main.replaceChildren(html`<div class="empty">
      <p>No calendars yet.</p>
      <p>Create one to start scheduling.</p>
      <button class="primary" id="grid-new-calendar">${icon('plus', 14)} New calendar</button>
    </div>`);
    $('#grid-new-calendar', main).addEventListener('click', () => $('#new-calendar').click());
    return;
  }
  if (!store.state.range) { reload(); return; }
  const instances = store.visibleInstances();
  const prevScroll = main.scrollTop;
  // Keep keyboard focus stable across a re-render (e.g. the 30 s poll
  // redrawing mid-tab / mid-search): the agenda search keeps its focus and
  // caret, and a focused event chip/block is re-focused by its data-key.
  const search = main.querySelector('#agenda-q');
  const searchFocus = search && document.activeElement === search
    ? { start: search.selectionStart, end: search.selectionEnd } : null;
  const focusKey = document.activeElement?.closest?.('#content [data-key]')?.dataset.key ?? null;
  main.innerHTML = '';
  if (view === 'month') main.append(renderMonth(instances));
  else if (view === 'week') main.append(renderTimed(instances, 7));
  else if (view === 'day') main.append(renderTimed(instances, 1));
  else main.append(renderAgenda(instances));
  if (searchFocus) {
    const q = main.querySelector('#agenda-q');
    if (q) { q.focus(); try { q.setSelectionRange(searchFocus.start, searchFocus.end); } catch { /* unsupported */ } }
  }
  if (focusKey) {
    main.querySelector(`[data-key="${CSS.escape(focusKey)}"]`)
      ?.focus({ preventScroll: true });
  }
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
// Pointer-based drag (mouse and pen). Touch is deliberately excluded: a
// finger-drag must scroll the grid, and receiving pointermove reliably would
// need touch-action:none on every chip, which would break scrolling. Touch
// users move events through Edit instead; a tap still opens the detail.
// Recurring events need the editor's per-occurrence/series choice and
// attendees can't move someone else's meeting, so those show a hint toast.

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
    if (ev.button !== 0 || ev.pointerType === 'touch') return;
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
    if (ev.button !== 0 || ev.pointerType === 'touch') return;
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
    ? html`<button class="chip${tentative}" data-key="${key}" style="background:${color};color:${store.inkOn(color)}">${inst.summary}</button>`
    : html`<button class="chip timed${tentative}" data-key="${key}"><span class="dot" style="background:${color}"></span>${fmtTime(inst.start)} ${inst.summary}</button>`;
  chip.addEventListener('click', ev => { ev.stopPropagation(); openEvent(inst); });
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
  const ROWS = monthWeeks();   // 5 when the month fits, else 6
  for (let w = 0; w < ROWS; w++) {
    const weekStart = addDays(start, w * 7);
    const { multi, laneCount } = spans(instances, weekStart);
    const row = html`<div class="weeks-row"></div>`;
    const MAX = 3;
    for (let d = 0; d < 7; d++) {
      const day = addDays(weekStart, d);
      const cell = html`<div class="cell${day.getMonth() !== cursor.getMonth() ? ' other' : ''}${sameDay(day, today) ? ' today' : ''}" role="button" tabindex="0" aria-label="${day.toLocaleDateString([], { weekday: 'long', month: 'long', day: 'numeric' })} — add event" data-date="${+day}"><span class="num">${day.getDate()}</span></div>`;
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
      // A role="button" div doesn't get native Space/Enter activation, so wire
      // both (and stop Space from scrolling the page).
      cell.addEventListener('keydown', ev => { if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); create(); } });
      row.append(cell);
    }
    // spanning-bar overlay, aligned to the same 7 columns
    if (multi.length) {
      const bars = html`<div class="week-bars"></div>`;
      for (const s of multi) {
        const color = store.calColor(s.inst.calHref);
        const bar = html`<button class="span-bar${s.contLeft ? ' cl' : ''}${s.contRight ? ' cr' : ''}${unconfirmedCls(s.inst)}"
          data-key="${store.instanceKey(s.inst)}"
          style="grid-column:${s.colStart + 1}/${s.colEnd + 2};grid-row:${s.lane + 1};background:${color};color:${store.inkOn(color)}"
          title="${s.inst.summary}">${s.contLeft ? '‹ ' : ''}${s.inst.summary}</button>`;
        // (the ‹ continuation marker is text, not an icon: it prefixes a
        // truncated title inside the bar and must flow with the text)
        bar.addEventListener('click', ev => { ev.stopPropagation(); openEvent(s.inst); });
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
    // The all-day lane: chips share the day's width side-by-side (they all
    // overlap by definition — they cover the whole day), and the empty lane
    // is clickable to create an all-day event, like every other empty region.
    const lane = html`<div class="allday" role="button" tabindex="0"
      aria-label="${day.toLocaleDateString([], { weekday: 'long', month: 'long', day: 'numeric' })} — add all-day event"></div>`;
    for (const inst of eventsOnDay(instances, day).filter(i => i.allDay)) lane.append(chipFor(inst));
    const create = () => openEventDialog(null, day, { allDay: true });
    lane.addEventListener('click', ev => { if (ev.target === lane) create(); });
    lane.addEventListener('keydown', ev => { if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); create(); } });
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
      const block = html`<button class="block${unconfirmedCls(inst)}" data-key="${store.instanceKey(inst)}" style="top:${s * HOUR_REM}rem;height:${Math.max(.6, (e - s) * HOUR_REM)}rem;left:calc(${p.col * w}% + 2px);width:calc(${w}% - 4px);right:auto;background:${store.calColor(inst.calHref)};color:${store.calInk(inst.calHref)}">${fmtTime(inst.start)} ${inst.summary}</button>`;
      block.addEventListener('click', ev => { ev.stopPropagation(); openEvent(inst); });
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
      openEventDialog(null, when, { timed: true });
    });
    el.append(col);
  }
  return el;
}

// ---- agenda --------------------------------------------------------------

// A search runs against the whole calendar server-side, so results outlive the
// loaded window and each keystroke would otherwise fire a REPORT: debounce, and
// drop any run whose list was replaced or whose term has moved on (see runSearch).
let searchTimer = null;

function renderAgenda(instances) {
  const el = html`<div class="agenda"></div>`;
  const input = html`<input id="agenda-q" type="search" placeholder="Search all events…" aria-label="Search all events" value="${agendaQuery}">`;
  const search = html`<div class="agenda-search"></div>`;
  search.append(input);
  const list = html`<div class="agenda-list"></div>`;
  el.append(search, list);
  // Mirror the term into the URL (so a reload restores the search) but only via
  // replaceUrl — a full navigate would re-render the grid and steal focus from
  // the field mid-keystroke. scheduleSearch refreshes just the results list.
  input.addEventListener('input', () => {
    agendaQuery = input.value;
    router.replaceUrl(router.calendarPath({ view: 'agenda', date: cursor, q: agendaQuery.trim() }));
    scheduleSearch(list);
  });
  renderAgendaBody(list, instances);
  return el;
}

// With no term, the agenda is the plain windowed list; with one, it is the
// whole-calendar search. Called on (re)render — a background refresh re-runs the
// search silently rather than blanking to a spinner (see runSearch).
function renderAgendaBody(list, windowInstances) {
  if (agendaQuery.trim()) runSearch(list);
  else fillWindowAgenda(list, windowInstances);
}

function scheduleSearch(list) {
  clearTimeout(searchTimer);
  if (!agendaQuery.trim()) { fillWindowAgenda(list, store.visibleInstances()); return; }
  searchTimer = setTimeout(() => runSearch(list), 220);
}

async function runSearch(list) {
  const q = agendaQuery.trim();
  // Only show the spinner on a first/empty search; a re-run keeps prior results
  // visible until the new ones land, so the 30 s poll doesn't cause a flicker.
  if (!list.querySelector('.item')) setEmpty(list, 'Searching…');
  let results;
  try { results = await store.searchEvents(q); }
  catch { if (list.isConnected && q === agendaQuery.trim()) setEmpty(list, 'Search failed. Please try again.'); return; }
  // Drop a stale run: the list was replaced by a re-render (isConnected false),
  // or the term moved on while this was in flight. Guarding on the live list and
  // current term — rather than a global token — means a background re-render's
  // own fresh search isn't invalidated by an older keystroke's pending one.
  if (!list.isConnected || q !== agendaQuery.trim()) return;
  fillSearchResults(list, results, q);
}

function setEmpty(list, msg) {
  list.innerHTML = '';
  list.append(html`<p class="empty">${msg}</p>`);
}

// One agenda row. Shared by the windowed list and the search results.
function agendaItem(inst) {
  const item = html`<button class="item${unconfirmedCls(inst)}" data-key="${store.instanceKey(inst)}">
    <span class="dot" style="background:${store.calColor(inst.calHref)}"></span>
    <span class="time">${inst.allDay ? 'all day' : fmtTime(inst.start) + ' – ' + fmtTime(inst.end)}</span>
    <span class="what">${inst.summary}</span>
    ${inst.location && frag`<span class="where">${icon('pin', 12)} ${inst.location}</span>`}
  </button>`;
  item.addEventListener('click', () => openEvent(inst));
  return item;
}

function fillWindowAgenda(list, instances) {
  const { start, end } = range();
  const today = startOfDay(new Date());
  list.innerHTML = '';
  let any = false;
  for (let day = new Date(start); day < end; day = addDays(day, 1)) {
    const evs = eventsOnDay(instances, day);
    if (!evs.length) continue;
    any = true;
    list.append(html`<h3 class="${sameDay(day, today) ? 'today' : ''}">${day.toLocaleDateString([], { weekday: 'long', month: 'long', day: 'numeric' })}</h3>`);
    for (const inst of evs) list.append(agendaItem(inst));
  }
  if (!any) setEmpty(list, 'Nothing planned in this period. Click “New event” to add something.');
}

// Search results span arbitrary dates, so headers carry the year and events are
// grouped by their own start day (already sorted by searchEvents).
function fillSearchResults(list, results, q) {
  const today = startOfDay(new Date());
  list.innerHTML = '';
  if (!results.length) { setEmpty(list, `No events match “${q}”.`); return; }
  let dayKey = null;
  for (const inst of results) {
    const k = +tzDayStart(inst.start);
    if (k !== dayKey) {
      dayKey = k;
      const day = new Date(k);
      list.append(html`<h3 class="${sameDay(day, today) ? 'today' : ''}">${day.toLocaleDateString([], { weekday: 'long', month: 'long', day: 'numeric', year: 'numeric' })}</h3>`);
    }
    list.append(agendaItem(inst));
  }
}
