// mini-month.js — the small month calendar used for jump-to-date: mounted in
// the sidebar (always visible on wide screens) and in a popover under the
// title bar (the only affordance on narrow screens, where the sidebar is an
// off-canvas drawer). Clicking a day moves the main view there; ‹ › browse
// months. Days inside the main view's visible range are tinted and the
// cursor day is solid, so the widget also shows *where you are*, not just
// where today is.

import * as store from '/shared/store.js';
import { $, html } from '/shared/dom.js';
import { icon } from '/shared/icons.js';
import { startOfDay, addDays, sameDay } from '/shared/time.js';
import { currentCalendarRoute, currentRange, setGridView, gridNav } from './grid.js';

// createMiniMonth() → { el, refresh() }. refresh() redraws from the grid's
// cursor/range and the week-start pref; main.js calls it on cursor changes
// (grid.onCursorChange) and pref changes. The displayed month follows the
// cursor — browsing with ‹ › is local until the next main-view navigation
// snaps it back.
export function createMiniMonth() {
  let shown = null;          // first-of-month Date currently displayed
  let lastCursorMonth = null; // (year*12+month) the cursor was on last refresh
  const el = html`<div class="mm" role="group" aria-label="Choose a date"></div>`;

  function refresh() {
    const { view, date } = currentCalendarRoute();
    const cursor = date instanceof Date ? date : startOfDay(new Date());
    // The displayed month follows the cursor: any main-view navigation snaps
    // a browsed-away month back. Browsing with ‹ › stays local until then.
    const cm = cursor.getFullYear() * 12 + cursor.getMonth();
    if (!shown || lastCursorMonth !== cm) shown = new Date(cursor.getFullYear(), cursor.getMonth(), 1);
    lastCursorMonth = cm;
    const today = startOfDay(new Date());
    const range = currentRange();
    const wk = store.state.prefs.weekStart === 0 ? 0 : 1;

    el.replaceChildren();
    const head = html`<div class="mm-head">
      <button class="icon-btn mm-prev" aria-label="Previous month">${icon('chevLeft', 14)}</button>
      <span class="mm-title" aria-live="polite">${shown.toLocaleDateString([], { month: 'long', year: 'numeric' })}</span>
      <button class="icon-btn mm-next" aria-label="Next month">${icon('chevRight', 14)}</button>
    </div>`;
    $('.mm-prev', head).addEventListener('click', () => { shown = new Date(shown.getFullYear(), shown.getMonth() - 1, 1); refresh(); });
    $('.mm-next', head).addEventListener('click', () => { shown = new Date(shown.getFullYear(), shown.getMonth() + 1, 1); refresh(); });
    el.append(head);

    const grid = html`<div class="mm-grid" role="grid"></div>`;
    const dow = html`<div class="mm-dow" role="row"></div>`;
    for (let i = 0; i < 7; i++) {
      dow.append(html`<span role="columnheader">${addDays(startOfWeekOf(shown, wk), i)
        .toLocaleDateString([], { weekday: 'narrow' })}</span>`);
    }
    grid.append(dow);

    const first = startOfWeekOf(shown, wk);
    // Tint the days the main view is showing — except in agenda, whose
    // 30-day window is an implementation detail, not a visible selection.
    const inRange = d => view !== 'agenda' && range && +d >= +range.start && +d < +range.end;
    for (let i = 0; i < 42; i++) {
      const day = addDays(first, i);
      const other = day.getMonth() !== shown.getMonth();
      const cls = ['mm-day'];
      if (other) cls.push('other');
      if (sameDay(day, today)) cls.push('today');
      if (inRange(day)) cls.push('in-range');
      if (!other && sameDay(day, cursor)) cls.push('sel');
      const b = html`<button type="button" role="gridcell" class="${cls.join(' ')}"
        aria-label="${day.toLocaleDateString([], { weekday: 'long', month: 'long', day: 'numeric', year: 'numeric' })}"
        aria-current="${sameDay(day, today) ? 'date' : 'false'}">${day.getDate()}</button>`;
      b.addEventListener('click', () => setGridView(view, day, { replace: true }));
      grid.append(b);
    }
    el.append(grid);
  }

  function startOfWeekOf(d, wk) {
    const s = startOfDay(d);
    return addDays(s, -((s.getDay() - wk + 7) % 7));
  }

  return { el, refresh };
}

// ---- title-bar date popover ------------------------------------------------
//
// The #cal-title span doubles as the popover trigger (role=button in the
// markup): clicking it drops a mini-month + Today button right under the
// title. Closed by choosing a day, Today, clicking anywhere outside, or
// Escape (captured before the app-wide Escape handler).

let pop = null;

export function closeTitlePopover() {
  if (!pop) return;
  pop.remove();
  pop = null;
  $('#cal-title').setAttribute('aria-expanded', 'false');
}

export function wireTitlePopover() {
  const title = $('#cal-title');
  const toggle = () => {
    if (pop) { closeTitlePopover(); return; }
    const mm = createMiniMonth();
    mm.refresh();
    const today = html`<button class="primary mm-today">Today</button>`;
    today.addEventListener('click', () => { closeTitlePopover(); gridNav(0); });
    pop = html`<div id="cal-popover" role="dialog" aria-label="Choose a date"></div>`;
    pop.append(mm.el, today);
    document.body.append(pop);
    // position under the title, clamped into the viewport
    const r = title.getBoundingClientRect();
    const w = pop.offsetWidth;
    pop.style.left = Math.max(8, Math.min(window.innerWidth - w - 8, r.left + r.width / 2 - w / 2)) + 'px';
    pop.style.top = r.bottom + 6 + 'px';
    title.setAttribute('aria-expanded', 'true');
    pop.querySelector('.mm-day')?.focus();
    // choosing a day (or Today) both navigates and closes
    pop.addEventListener('click', ev => {
      if (ev.target.closest('.mm-day, .mm-today')) closeTitlePopover();
    });
  };
  // Registered once; both no-op unless the popover is open. Escape is
  // captured so the app-wide Escape handler doesn't also fire.
  document.addEventListener('click', ev => { if (pop && !pop.contains(ev.target) && title !== ev.target) closeTitlePopover(); });
  document.addEventListener('keydown', ev => {
    if (ev.key !== 'Escape' || !pop) return;
    ev.stopPropagation();
    closeTitlePopover();
    title.focus();
  }, true);
  title.addEventListener('click', toggle);
  title.addEventListener('keydown', ev => {
    if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); toggle(); }
  });
}
