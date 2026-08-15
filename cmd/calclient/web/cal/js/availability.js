// availability.js — the editor's availability strip: a per-guest timeline for
// the event's day (outbox free-busy), zoomed to a readable window, with an
// hour axis, the proposed slot highlighted, busy blocks (hover for their
// times), and a per-guest + overall verdict.

import * as store from '/shared/store.js';
import { $, html, frag } from '/shared/dom.js';
import { icon } from '/shared/icons.js';
import { DAY, HOUR, startOfDay } from '/shared/time.js';
import { fmtTime, fmtDate, showError } from './ui.js';

// checkAvailability renders the strip into `box` for the proposed slot.
// readTimes() re-reads the editor's current {start, end} — called again after
// the network round-trip because the user may have changed the times while
// the free-busy request was in flight. `self` is the stored event's
// {start, end} when editing, or null when creating.
export async function checkAvailability(box, guests, readTimes, self) {
  const { start: s } = readTimes();
  if (!s) return;
  const dayStart = startOfDay(s);
  const dayEnd = new Date(+dayStart + DAY);
  box.innerHTML = '<span class="avail-loading">Checking availability…</span>';
  try {
    const fb = await store.freeBusy(guests.map(g => g.email), dayStart, dayEnd);
    const { start: es, end: ee } = readTimes();
    if (!es || !ee) { box.innerHTML = ''; return; }
    const hourOf = t => (+t - +dayStart) / HOUR;

    // When editing, this event already occupies its slot on the invitees'
    // calendars, so it would show as a conflict against itself. Subtract the
    // event's own (saved) interval from each guest's busy time before judging.
    const minusSelf = busy => {
      if (!self) return busy;
      const out = [];
      for (const b of busy) {
        const bs = +b.start, be = +b.end;
        if (be <= +self.start || bs >= +self.end) { out.push(b); continue; }
        if (bs < +self.start) out.push({ start: b.start, end: new Date(+self.start) });
        if (be > +self.end) out.push({ start: new Date(+self.end), end: b.end });
      }
      return out;
    };
    const lanes = [...fb].map(([email, busy]) => [email, minusSelf(busy)]);

    // Zoom to business hours, widened to include the slot and any busy time
    // outside them, so the day isn't crammed into a sliver.
    let lo = 7, hi = 20;
    lo = Math.min(lo, Math.floor(hourOf(es)));
    hi = Math.max(hi, Math.ceil(hourOf(ee)));
    for (const [, busy] of lanes) for (const b of busy) {
      lo = Math.min(lo, Math.floor(hourOf(b.start)));
      hi = Math.max(hi, Math.ceil(hourOf(b.end)));
    }
    lo = Math.max(0, lo); hi = Math.min(24, hi); if (hi <= lo) hi = Math.min(24, lo + 1);
    const span = hi - lo;
    const pos = t => Math.min(100, Math.max(0, (hourOf(t) - lo) / span * 100));
    const hourLabel = hh => fmtTime(new Date(+dayStart + hh * HOUR)).replace(':00', '');
    const step = Math.max(1, Math.ceil(span / 8));

    box.innerHTML = '';
    box.append(html`<div class="avail-cap">${fmtDate(es)} · proposed <b>${fmtTime(es)} – ${fmtTime(ee)}</b></div>`);

    // hour axis, aligned to the tracks below
    const ticks = [];
    for (let hh = lo; hh <= hi; hh += step) {
      ticks.push(frag`<span class="tick" style="left:${(hh - lo) / span * 100}%">${hourLabel(hh)}</span>`);
    }
    box.append(html`<div class="lane axis"><span class="who"></span><span class="track">${ticks}</span><span class="verdict"></span></div>`);

    let allFree = true;
    for (const [email, busy] of lanes) {
      const lane = html`<div class="lane"><span class="who" title="${email}">${email}</span><span class="track"></span><span class="verdict"></span></div>`;
      const track = $('.track', lane);
      for (let hh = lo; hh <= hi; hh += step) track.append(html`<span class="grid" style="left:${(hh - lo) / span * 100}%"></span>`);
      track.append(html`<span class="slot" style="left:${pos(es)}%;width:${Math.max(1.5, pos(ee) - pos(es))}%"></span>`);
      let clash = false;
      for (const b of busy) {
        const allDay = (+b.end - +b.start) >= 23.5 * HOUR;
        const tip = allDay ? 'Busy all day' : `Busy ${fmtTime(b.start)} – ${fmtTime(b.end)}`;
        track.append(html`<span class="busy" style="left:${pos(b.start)}%;width:${Math.max(1.5, pos(b.end) - pos(b.start))}%" title="${tip}"></span>`);
        if (+b.start < +ee && +b.end > +es) clash = true;
      }
      const v = $('.verdict', lane);
      v.replaceChildren(frag`${clash ? icon('circleDot', 11) : icon('circle', 11)} ${clash ? 'busy' : 'free'}`);
      v.className = 'verdict ' + (clash ? 'bad' : 'ok');
      if (clash) allFree = false;
      box.append(lane);
    }
    box.append(html`<div class="avail-sum ${allFree ? 'ok' : 'bad'}">${allFree ? frag`${icon('check', 15)} Everyone is free at the proposed time` : frag`${icon('warn', 15)} Someone is busy at the proposed time`}</div>`);
    box.append(html`<div class="avail-legend"><span><i class="k-slot"></i> proposed time</span><span><i class="k-busy"></i> busy</span></div>`);
  } catch (e) {
    box.innerHTML = '';
    showError(e);
  }
}
