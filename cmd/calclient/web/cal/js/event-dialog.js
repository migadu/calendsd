// event-dialog.js — the New/Edit event dialog: plain-language fields on top
// of PUT .ics, guest chips with directory autocomplete, an availability
// strip (outbox free-busy), and series-vs-occurrence editing for recurring
// events.

import * as store from '/shared/store.js';
import * as ical from '/shared/ical.js';
import { $, h, esc, fmtTime, fmtDate, toast, showError } from './main.js';

function localDT(d) {
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}
function localDate(d) {
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

// parse a simple RRULE into the preset controls; returns null if too
// complex to represent (then a free-text field takes over).
function parseRRule(rrule) {
  if (!rrule) return { freq: '', ends: 'never' };
  const parts = Object.fromEntries(rrule.split(';').map(kv => kv.split('=')));
  const known = ['FREQ', 'COUNT', 'UNTIL', 'INTERVAL'];
  if (Object.keys(parts).some(k => !known.includes(k)) || (parts.INTERVAL && parts.INTERVAL !== '1')) return null;
  if (!['DAILY', 'WEEKLY', 'MONTHLY', 'YEARLY'].includes(parts.FREQ)) return null;
  return {
    freq: parts.FREQ,
    ends: parts.COUNT ? 'count' : parts.UNTIL ? 'until' : 'never',
    count: parts.COUNT || '10',
    until: parts.UNTIL ? `${parts.UNTIL.slice(0, 4)}-${parts.UNTIL.slice(4, 6)}-${parts.UNTIL.slice(6, 8)}` : '',
  };
}
function buildRRule(d) {
  if (!d.freq) return '';
  let r = 'FREQ=' + d.freq;
  if (d.ends === 'count') r += ';COUNT=' + Math.max(1, parseInt(d.count || '1', 10));
  if (d.ends === 'until' && d.until) r += ';UNTIL=' + ical.dateOnly(d.until) + 'T235959Z';
  return r;
}

// openEventDialog(null, date?, timed?) creates; openEventDialog(instance) edits.
export async function openEventDialog(inst, defaultDay, timed) {
  let master = null; // {text, etag} of the stored object when editing
  if (inst) {
    try { master = await store.getObject(inst.href); }
    catch (e) { showError(e); return; }
  }

  const now = new Date();
  const base = defaultDay ? new Date(defaultDay) : now;
  if (!timed) base.setHours(now.getHours() + 1, 0, 0, 0);
  const start = inst ? inst.start : base;
  const end = inst ? inst.end : new Date(+base + (store.state.prefs.defaultDuration || 60) * 60000);
  const rrulePreset = inst ? parseRRule(inst.rrule) : { freq: '', ends: 'never' };
  const isRecurring = !!(inst && (inst.rrule || inst.rid));
  const guests = (inst?.attendees ?? []).map(a => ({ ...a }));

  const dlg = h(`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="${inst ? 'Edit event' : 'New event'}">
    <h2>${inst ? 'Edit event' : 'New event'}</h2>
    ${isRecurring ? `<label class="row" style="margin-bottom:.6rem">Apply to
      <select id="ev-scope">
        <option value="one">only this event</option>
        <option value="all">all events in the series</option>
      </select></label>` : ''}
    <div class="grid">
      <label class="full">Title<input id="ev-title" type="text" value="${esc(inst?.summary === '(no title)' ? '' : inst?.summary ?? '')}" placeholder="Add a title"></label>
      <label>Calendar<select id="ev-cal"></select></label>
      <label class="row" style="align-self:end"><input type="checkbox" id="ev-allday" ${inst?.allDay ? 'checked' : ''}> All day</label>
      <label>Starts<input id="ev-start"></label>
      <label>Ends<input id="ev-end"></label>
      <label>Repeat<select id="ev-freq" ${isRecurring ? 'disabled' : ''}>
        <option value="">Does not repeat</option>
        <option value="DAILY">Every day</option>
        <option value="WEEKLY">Every week</option>
        <option value="MONTHLY">Every month</option>
        <option value="YEARLY">Every year</option>
      </select></label>
      <label id="ev-ends-wrap" hidden>Repeat ends<span style="display:flex;gap:.3rem;align-items:center">
        <select id="ev-ends">
          <option value="never">never</option>
          <option value="count">after a number of times…</option>
          <option value="until">on a date…</option>
        </select>
        <input id="ev-count" type="number" min="1" value="10" style="width:4.5rem" hidden>
        <span id="ev-count-unit" hidden>times</span>
        <input id="ev-until" type="date" hidden>
      </span></label>
      <label class="full">Location<input id="ev-loc" type="text" value="${esc(inst?.location ?? '')}" placeholder="Add a location"></label>
      <label class="full">Meeting link<input id="ev-url" type="url" value="${esc(inst?.url ?? '')}" placeholder="e.g. https://zoom.us/j/123456789"></label>
      <label class="full row"><input type="checkbox" id="ev-private" ${inst?.confidential ? 'checked' : ''}> Private — anyone this calendar is shared with read-only sees only “Busy”, not the title or details</label>
      <label class="full">Guests<span class="suggest">
        <span class="chips" id="ev-chips"><input id="ev-guest" type="email" placeholder="Invite by name or email…" autocomplete="off"></span>
        <span class="suggest-list" id="ev-suggest" hidden></span></span></label>
      <span class="full hint" id="ev-guest-hint" hidden>When you save, your guests are notified of the change automatically; removing a guest cancels their copy.</span>
      <span class="full avail" id="ev-avail"></span>
      <label class="full">Notes<textarea id="ev-desc" rows="3">${esc(inst?.description ?? '')}</textarea></label>
    </div>
    <p class="error" id="ev-error"></p>
    <div class="actions">
      ${inst ? '<button class="danger" id="ev-delete">Delete…</button>' : ''}
      <button id="ev-cancel">Cancel</button>
      <button class="primary" id="ev-save">Save</button>
    </div>
  </div></div>`);

  const q = sel => $(sel, dlg);
  const close = () => dlg.remove();
  dlg.addEventListener('click', ev => { if (ev.target === dlg) close(); });
  // Esc cancels the editor (unless a child, e.g. the guest list, handled it).
  dlg.addEventListener('keydown', ev => { if (ev.key === 'Escape') { ev.stopPropagation(); close(); } });
  // Enter submits from a single-line text field. Skip the Notes textarea (Enter
  // = newline), the guest field (owns Enter for autocomplete), and <select>
  // elements (Enter/Space open the native menu — hijacking it would make the
  // dropdowns feel unusable by keyboard).
  dlg.addEventListener('keydown', ev => {
    const t = ev.target;
    if (ev.key !== 'Enter' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.id === 'ev-guest') return;
    ev.preventDefault();
    q('#ev-save').click();
  });
  q('#ev-cancel').addEventListener('click', close);

  // calendars — an event can only live on a writable calendar, so the picker
  // lists just those (a read-only share the event currently sits on is added
  // so its name still shows, but the whole editor is disabled below). Changing
  // the calendar of an existing event moves it there on save.
  const roCal = !!inst && store.isReadOnly(inst.calHref);
  const calSel = q('#ev-cal');
  const writable = store.eventCalendars();
  const calOptions = inst && !writable.some(c => c.href === inst.calHref)
    ? [store.state.calendars.find(c => c.href === inst.calHref), ...writable].filter(Boolean)
    : writable;
  for (const c of calOptions) {
    calSel.append(h(`<option value="${esc(c.href)}">${esc(c.displayName || c.name)}</option>`));
  }
  calSel.value = inst?.calHref || store.state.prefs.defaultCal || calOptions[0]?.href || '';

  // Moving a single occurrence to another calendar isn't well-defined (the
  // per-occurrence edit only adds an override to the master), so the picker is
  // locked to the current calendar when "only this event" is chosen, and
  // always for a read-only share.
  const scopeSel = q('#ev-scope');
  function syncCalLock() {
    const locked = roCal || (scopeSel && scopeSel.value === 'one');
    calSel.disabled = locked;
    if (locked && inst) calSel.value = inst.calHref;
  }
  syncCalLock();
  scopeSel?.addEventListener('change', syncCalLock);

  // date/time inputs switch type with the all-day toggle
  const allDayBox = q('#ev-allday');
  function syncDateInputs() {
    const allDay = allDayBox.checked;
    const s = q('#ev-start'), e = q('#ev-end');
    s.type = e.type = allDay ? 'date' : 'datetime-local';
    if (allDay) {
      s.value = localDate(start); e.value = localDate(new Date(Math.max(+start, +end - 864e5)));
    } else {
      s.value = localDT(start); e.value = localDT(end);
    }
  }
  syncDateInputs();

  // Moving the start drags the end with it, keeping the duration fixed — so
  // changing a 1–2pm slot to 5pm makes it 5–6pm. We track the previous start
  // and shift the end by the same delta. Re-baseline whenever the all-day
  // toggle rewrites both inputs.
  const startInp = q('#ev-start'), endInp = q('#ev-end');
  // Native datetime-local pickers show 12- or 24-hour time per the element's
  // locale, not the app's. Force it to follow the user's clock preference via
  // `lang` (honored by Chromium): a 24-hour locale when 12-hour is off, a
  // 12-hour one when it's on, so the editor matches the rest of the UI.
  const timeLang = store.state.prefs.hour12 ? 'en-US' : 'en-GB';
  startInp.setAttribute('lang', timeLang);
  endInp.setAttribute('lang', timeLang);
  const parseInput = inp => {
    if (!inp.value) return null;
    return inp.type === 'date' ? new Date(inp.value + 'T00:00') : new Date(inp.value);
  };
  const setInput = (inp, d) => { inp.value = inp.type === 'date' ? localDate(d) : localDT(d); };
  let prevStart = parseInput(startInp);
  startInp.addEventListener('input', () => {
    const ns = parseInput(startInp);
    if (!ns) return;
    const delta = prevStart ? +ns - +prevStart : 0;
    const ce = parseInput(endInp);
    if (delta && ce) setInput(endInp, new Date(+ce + delta));
    prevStart = ns;
  });
  allDayBox.addEventListener('change', () => { syncDateInputs(); prevStart = parseInput(startInp); });

  // repeat controls
  const freqSel = q('#ev-freq'), endsWrap = q('#ev-ends-wrap'), endsSel = q('#ev-ends');
  if (rrulePreset) {
    freqSel.value = rrulePreset.freq;
    endsSel.value = rrulePreset.ends;
    q('#ev-count').value = rrulePreset.count ?? '10';
    q('#ev-until').value = rrulePreset.until ?? '';
  } else {
    // complex RRULE: keep it verbatim, disable the preset UI
    freqSel.append(h('<option value="CUSTOM" selected>Custom repeat (kept as is)</option>'));
  }
  function syncRepeat() {
    endsWrap.hidden = !freqSel.value || freqSel.value === 'CUSTOM';
    const byCount = endsSel.value === 'count';
    q('#ev-count').hidden = !byCount;
    q('#ev-count-unit').hidden = !byCount;
    q('#ev-until').hidden = endsSel.value !== 'until';
  }
  syncRepeat();
  freqSel.addEventListener('change', syncRepeat);
  endsSel.addEventListener('change', syncRepeat);

  // guests
  const chips = q('#ev-chips'), guestInput = q('#ev-guest'), suggest = q('#ev-suggest');
  function renderChips() {
    for (const g of chips.querySelectorAll('.guest')) g.remove();
    for (const g of guests) {
      const cls = g.partstat === 'ACCEPTED' ? ' accepted' : g.partstat === 'DECLINED' ? ' declined' : '';
      const chip = h(`<span class="guest${cls}" title="${esc(g.partstat ?? '')}">${esc(g.email)}<button aria-label="Remove ${esc(g.email)}">×</button></span>`);
      $('button', chip).addEventListener('click', () => { guests.splice(guests.indexOf(g), 1); renderChips(); });
      chips.insertBefore(chip, guestInput);
    }
    q('#ev-guest-hint').hidden = !guests.length;
    q('#ev-avail').innerHTML = guests.length
      ? '<button type="button" class="ghost" id="ev-check">Check availability</button>' : '';
    $('#ev-check', dlg)?.addEventListener('click', checkAvailability);
  }
  function addGuest(email, partstat) {
    email = email.trim().replace(/,$/, '');
    if (!email || !email.includes('@') || guests.some(g => g.email === email)) return;
    guests.push({ email, partstat: partstat || '' });
    renderChips();
    guestInput.value = '';
    closeSuggest();
  }
  // Autocomplete with keyboard navigation: ↑/↓ move the highlight, Enter
  // picks it (or adds the typed text), Esc closes the list.
  let suggestTimer = null, active = -1;
  function highlight(i) {
    const items = [...suggest.children];
    active = items.length ? (i + items.length) % items.length : -1;
    items.forEach((b, n) => b.classList.toggle('active', n === active));
    if (active >= 0) items[active].scrollIntoView({ block: 'nearest' });
  }
  function closeSuggest() { suggest.hidden = true; suggest.innerHTML = ''; active = -1; }
  guestInput.addEventListener('input', () => {
    clearTimeout(suggestTimer);
    suggestTimer = setTimeout(async () => {
      const found = await store.searchDirectory(guestInput.value);
      suggest.innerHTML = '';
      for (const f of found.filter(f => !guests.some(g => g.email === f.email))) {
        const b = h(`<button type="button">${esc(f.name || f.email)} <span class="mail">${esc(f.email)}</span></button>`);
        b.dataset.email = f.email;
        b.addEventListener('click', () => { addGuest(f.email); guestInput.focus(); });
        b.addEventListener('mousemove', () => highlight([...suggest.children].indexOf(b)));
        suggest.append(b);
      }
      suggest.hidden = !suggest.children.length;
      active = -1;
    }, 200);
  });
  guestInput.addEventListener('keydown', ev => {
    const open = !suggest.hidden && suggest.children.length;
    if (ev.key === 'ArrowDown' && open) { ev.preventDefault(); highlight(active + 1); return; }
    if (ev.key === 'ArrowUp' && open) { ev.preventDefault(); highlight(active - 1); return; }
    if (ev.key === 'Enter' || ev.key === ',') {
      ev.preventDefault();
      if (open && active >= 0) addGuest(suggest.children[active].dataset.email);
      else addGuest(guestInput.value);
      return;
    }
    if (ev.key === 'Escape' && open) { ev.stopPropagation(); closeSuggest(); return; }
    if (ev.key === 'Backspace' && !guestInput.value && guests.length) { guests.pop(); renderChips(); }
  });
  guestInput.addEventListener('blur', () => setTimeout(closeSuggest, 150));
  renderChips();

  // availability strip: a per-guest timeline for the event's day, zoomed to a
  // readable window, with an hour axis, the proposed slot highlighted, busy
  // blocks (hover for their times), and a per-guest + overall verdict.
  async function checkAvailability() {
    const box = q('#ev-avail');
    const { start: s } = readTimes();
    if (!s) return;
    const dayStart = new Date(s); dayStart.setHours(0, 0, 0, 0);
    const dayEnd = new Date(+dayStart + 864e5);
    box.innerHTML = '<span class="avail-loading">Checking availability…</span>';
    try {
      const fb = await store.freeBusy(guests.map(g => g.email), dayStart, dayEnd);
      const { start: es, end: ee } = readTimes();
      if (!es || !ee) { box.innerHTML = ''; return; }
      const hourOf = t => (+t - +dayStart) / 3600e3;

      // When editing, this event already occupies its slot on the invitees'
      // calendars, so it would show as a conflict against itself. Subtract the
      // event's own (saved) interval from each guest's busy time before judging.
      const selfStart = inst ? +inst.start : null, selfEnd = inst ? +inst.end : null;
      const minusSelf = busy => {
        if (selfStart == null) return busy;
        const out = [];
        for (const b of busy) {
          const bs = +b.start, be = +b.end;
          if (be <= selfStart || bs >= selfEnd) { out.push(b); continue; }
          if (bs < selfStart) out.push({ start: b.start, end: new Date(selfStart) });
          if (be > selfEnd) out.push({ start: new Date(selfEnd), end: b.end });
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
      const hourLabel = hh => fmtTime(new Date(+dayStart + hh * 3600e3)).replace(':00', '');
      const step = Math.max(1, Math.ceil(span / 8));

      box.innerHTML = '';
      box.append(h(`<div class="avail-cap">${esc(fmtDate(es))} · proposed <b>${esc(fmtTime(es))} – ${esc(fmtTime(ee))}</b></div>`));

      // hour axis, aligned to the tracks below
      let ticks = '';
      for (let hh = lo; hh <= hi; hh += step) ticks += `<span class="tick" style="left:${(hh - lo) / span * 100}%">${esc(hourLabel(hh))}</span>`;
      box.append(h(`<div class="lane axis"><span class="who"></span><span class="track">${ticks}</span><span class="verdict"></span></div>`));

      let allFree = true;
      for (const [email, busy] of lanes) {
        const lane = h(`<div class="lane"><span class="who" title="${esc(email)}">${esc(email)}</span><span class="track"></span><span class="verdict"></span></div>`);
        const track = $('.track', lane);
        for (let hh = lo; hh <= hi; hh += step) track.append(h(`<span class="grid" style="left:${(hh - lo) / span * 100}%"></span>`));
        track.append(h(`<span class="slot" style="left:${pos(es)}%;width:${Math.max(1.5, pos(ee) - pos(es))}%"></span>`));
        let clash = false;
        for (const b of busy) {
          const allDay = (+b.end - +b.start) >= 23.5 * 3600e3;
          const tip = allDay ? 'Busy all day' : `Busy ${fmtTime(b.start)} – ${fmtTime(b.end)}`;
          track.append(h(`<span class="busy" style="left:${pos(b.start)}%;width:${Math.max(1.5, pos(b.end) - pos(b.start))}%" title="${esc(tip)}"></span>`));
          if (+b.start < +ee && +b.end > +es) clash = true;
        }
        const v = $('.verdict', lane);
        v.textContent = clash ? '● busy' : '○ free';
        v.className = 'verdict ' + (clash ? 'bad' : 'ok');
        if (clash) allFree = false;
        box.append(lane);
      }
      box.append(h(`<div class="avail-sum ${allFree ? 'ok' : 'bad'}">${allFree ? '✓ Everyone is free at the proposed time' : '⚠ Someone is busy at the proposed time'}</div>`));
      box.append(h('<div class="avail-legend"><span><i class="k-slot"></i> proposed time</span><span><i class="k-busy"></i> busy</span></div>'));
    } catch (e) {
      box.innerHTML = '';
      showError(e);
    }
  }

  function readTimes() {
    const allDay = allDayBox.checked;
    const sv = q('#ev-start').value, ev = q('#ev-end').value;
    if (!sv || !ev) return {};
    if (allDay) {
      const s = new Date(sv + 'T00:00');
      return { allDay, start: s, end: new Date(+new Date(ev + 'T00:00') + 864e5), sVal: sv, eVal: ev };
    }
    return { allDay, start: new Date(sv), end: new Date(ev), sVal: sv, eVal: ev };
  }

  // save ------------------------------------------------------------------
  q('#ev-save').addEventListener('click', async () => {
    const t = readTimes();
    const title = q('#ev-title').value.trim() || 'Untitled event';
    if (!t.start || !t.end || t.end <= t.start) {
      q('#ev-error').textContent = 'Please pick an end after the start.';
      return;
    }
    const rrule = freqSel.value === 'CUSTOM' ? inst.rrule : buildRRule({
      freq: freqSel.value, ends: endsSel.value,
      count: q('#ev-count').value, until: q('#ev-until').value,
    });
    const fields = {
      uid: inst?.uid || crypto.randomUUID(),
      summary: title,
      allDay: t.allDay,
      start: t.allDay ? t.sVal : t.start.toISOString(),
      end: t.allDay ? localDate(new Date(+new Date(t.eVal + 'T00:00') + 864e5)) : t.end.toISOString(),
      location: q('#ev-loc').value.trim(),
      url: q('#ev-url').value.trim(),
      description: q('#ev-desc').value.trim(),
      organizer: guests.length ? store.myEmail() : '',
      attendees: guests,
      rrule,
      confidential: q('#ev-private').checked,
    };
    // Property lines shared by the in-place edit paths. null removes the
    // property; the attendee list is rewritten wholesale when guests change.
    const commonProps = {
      SUMMARY: `SUMMARY:${ical.icalEscape(fields.summary)}`,
      LOCATION: fields.location ? `LOCATION:${ical.icalEscape(fields.location)}` : null,
      DESCRIPTION: fields.description ? `DESCRIPTION:${ical.icalEscape(fields.description)}` : null,
      ORGANIZER: fields.organizer
        ? `ORGANIZER;CN=${ical.icalEscape(fields.organizer)}:mailto:${fields.organizer}` : null,
      ATTENDEE: guests.length
        ? guests.map(g => `ATTENDEE;PARTSTAT=${g.partstat || 'NEEDS-ACTION'};RSVP=TRUE:mailto:${g.email}`)
        : null,
      // null removes CLASS, restoring the PUBLIC default.
      CLASS: fields.confidential ? 'CLASS:CONFIDENTIAL' : null,
    };
    try {
      if (!inst) {
        await store.putObject(store.eventHref(calSel.value, fields.uid),
          ical.buildEvent(fields), null);
      } else if (isRecurring && q('#ev-scope').value === 'one') {
        const text = ical.addOverride(master.text, ical.eventLines({
          ...fields, rrule: '', recurrenceId: store.ridValue(inst),
        }));
        await store.putObject(inst.href, text, master.etag);
      } else if (isRecurring) {
        // "All events": edit the master VEVENT in place so overrides, alarms,
        // EXDATEs, and unmodeled properties survive. Shift the series by the
        // change made to *this* occurrence rather than re-anchoring it to the
        // clicked date, and never rewrite the RRULE — the server expands
        // recurrences, so the master rule isn't available here to reconstruct.
        const m0 = ical.components(ical.parse(master.text), 'VEVENT')
          .find(v => !ical.prop(v, 'RECURRENCE-ID'));
        const props = { ...commonProps };
        const ds = m0 && ical.prop(m0, 'DTSTART');
        const de = m0 && ical.prop(m0, 'DTEND');
        const startDelta = +t.start - +inst.start;
        const endDelta = +t.end - +inst.end;
        if (ds && startDelta) { const l = ical.shiftDate('DTSTART', ds, startDelta); if (l) props.DTSTART = l; }
        if (de && endDelta) { const l = ical.shiftDate('DTEND', de, endDelta); if (l) props.DTEND = l; }
        await store.putObject(inst.href, ical.setMasterProps(master.text, props), master.etag);
      } else {
        // Plain event: rewrite the master's fields in place (no series to
        // re-anchor), preserving alarms and unmodeled properties.
        const props = {
          ...commonProps,
          DTSTART: fields.allDay ? `DTSTART;VALUE=DATE:${ical.dateOnly(fields.start)}` : `DTSTART:${ical.utc(fields.start)}`,
          DTEND: fields.allDay ? `DTEND;VALUE=DATE:${ical.dateOnly(fields.end)}` : `DTEND:${ical.utc(fields.end)}`,
          RRULE: fields.rrule ? `RRULE:${fields.rrule}` : null,
        };
        await store.putObject(inst.href, ical.setMasterProps(master.text, props), master.etag);
      }
      // Calendar changed: relocate the object after the in-place edit. The
      // picker is locked for the single-occurrence scope, so a differing value
      // here always means the whole object should move.
      if (inst && calSel.value && calSel.value !== inst.calHref) {
        await store.moveObject(inst.href, calSel.value + inst.href.slice(inst.href.lastIndexOf('/') + 1));
      }
      close();
      toast(guests.length ? 'Saved — invitations are on their way.' : 'Saved.');
      await store.refreshEvents();
    } catch (e) {
      if (e.conflict) { q('#ev-error').textContent = e.message; store.refreshEvents().catch(() => {}); }
      else showError(e);
    }
  });

  // delete ----------------------------------------------------------------
  $('#ev-delete', dlg)?.addEventListener('click', async () => {
    const one = isRecurring && q('#ev-scope').value === 'one';
    const msg = one ? 'Delete only this occurrence?' :
      isRecurring ? 'Delete the whole series?' : 'Delete this event?';
    if (!confirm(msg)) return;
    try {
      if (one) {
        const text = ical.addExdate(master.text, store.ridValue(inst), inst.allDay);
        await store.putObject(inst.href, text, master.etag);
      } else {
        await store.deleteObject(inst.href);
      }
      close();
      toast('Deleted.');
      await store.refreshEvents();
    } catch (e) { showError(e); }
  });

  // A read-only share can be viewed but not written — disable the write
  // actions and say why, rather than let the save 403.
  if (roCal) {
    q('#ev-save').disabled = true;
    const del = $('#ev-delete', dlg);
    if (del) del.disabled = true;
    q('#ev-error').textContent = 'This event is on a calendar shared with you read-only, so it can’t be changed.';
  }

  $('#dialog-host').append(dlg);
  q('#ev-title').focus();
}
