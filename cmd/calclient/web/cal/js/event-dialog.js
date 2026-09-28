// event-dialog.js — the New/Edit event dialog: plain-language fields on top
// of PUT .ics and series-vs-occurrence editing for recurring events. The
// guest chips live in guest-field.js and the availability strip in
// availability.js; this module owns the form itself and the save paths.

import * as store from '/shared/store.js';
import * as ical from '/shared/ical.js';
import { $, html, frag } from '/shared/dom.js';
import { DAY, localDate, formatTimeOfDay, parseTimeOfDay, startOfDay, addDays, writeZone, zoneFields,
  zonedToInstant, fieldsFromInputs, fieldsFromWallMs, wallMs, dateValueIn, weekdayIn, formatHM } from '/shared/time.js';
import { toast, showError, busyWhile } from './ui.js';
import { createGuestField } from './guest-field.js';
import { createReminderField } from './reminder-field.js';
import { checkAvailability } from './availability.js';
import { confirmDialog, openOverlay, submitOnEnter } from '/shared/dialogs.js';

// openEventDialog(null, date?, opts?, onClose?) creates; openEventDialog(instance) edits.
// opts: { timed?: true } keeps the clicked hour instead of rounding to the
// next; { allDay: true } pre-checks All day (the all-day lane's create path).
// onClose (the router's, when editing via a /cal/event/<uid>/edit route) replaces
// the bare dlg.remove() so closing/saving navigates back to the calendar URL;
// the create paths pass none and keep the plain overlay behavior. Returns the
// overlay element (or undefined if the stored object failed to load).
export async function openEventDialog(inst, defaultDay, opts, onClose) {
  const o = opts || {};
  let master = null; // {text, etag} of the stored object when editing
  if (inst) {
    try { master = await store.getObject(inst.href); }
    catch (e) { showError(e); return; }
  }

  // Timed fields are shown, typed and written in the display zone (the tz
  // preference, else the device zone), so what the user enters is a wall
  // time in the same zone the grid shows and the event is stored in
  // (docs/dst-recurring-events.md §1). All-day values stay calendar dates.
  const zone = writeZone(store.state.prefs.tz);
  const now = new Date();
  let base;
  if (defaultDay && o.timed) base = new Date(defaultDay);
  else {
    // The next full hour in the display zone, on the clicked day (grid days
    // are device-local dates) or today.
    const nowF = zoneFields(now, zone);
    const day = defaultDay ? localDate(new Date(defaultDay)) : dateValueIn(now, zone);
    base = zonedToInstant(fieldsFromWallMs(wallMs(fieldsFromInputs(day, { h: nowF.h + 1, m: 0 }))), zone);
  }
  let start = inst ? inst.start : base;
  let end = inst ? inst.end : new Date(+base + (store.state.prefs.defaultDuration || 60) * 60000);
  if (!inst && o.allDay) {
    const day = startOfDay(defaultDay ? new Date(defaultDay) : now);
    start = day; end = addDays(day, 1);
  }
  const isRecurring = !!(inst && (inst.rrule || inst.rid));
  const guests = (inst?.attendees ?? []).map(a => ({ ...a }));

  // The master VEVENT (no RECURRENCE-ID) carries the real RRULE — the expanded
  // instance we were opened on had it stripped. Reused for the reminder read
  // and the "this and following" split.
  const masterVevent = master && ical.components(ical.parse(master.text), 'VEVENT')
    .find(v => !ical.prop(v, 'RECURRENCE-ID'));
  const masterRRule = masterVevent ? ical.propValue(masterVevent, 'RRULE') : '';
  // The repeat controls show the series' rule: the occurrence the editor was
  // opened on is a server-expanded instance, whose RRULE is stripped.
  const rrulePreset = inst ? ical.parseRRulePreset(masterRRule || inst.rrule) : { freq: '', ends: 'never' };
  const masterDs = masterVevent && ical.prop(masterVevent, 'DTSTART');
  const masterStart = masterDs ? ical.parseICalDate(masterDs.value, masterDs.params)?.date : null;
  // The zone the stored series recurs in: its TZID, 'UTC' for a UTC DTSTART.
  // Existing series keep it when edited (overrides, the tail of a split),
  // unless the edit converts a UTC series (see utcSeries below).
  const anchorZone = masterDs ? ical.seriesZone(masterDs) : undefined;
  // Offer "this and following" only for a splittable rule and not on the first
  // occurrence (there it's just "all"), so a split can never corrupt a series.
  const canFuture = isRecurring && !!masterRRule && !!masterStart
    && ical.canSplitSeries(masterStart, masterRRule, anchorZone) && +inst.start > +masterStart;
  // A series stored in UTC recurs at a fixed UTC time, so its local time moves
  // with DST. Editing it — any occurrence, in any scope — converts it to the
  // display zone: automatically for series calweb wrote, and only on explicit
  // opt-in (with a preview) for anything else, whose UTC timing may be
  // intentional (§4).
  const utcSeries = isRecurring && master && zone !== 'UTC' ? ical.utcSeriesInfo(master.text) : null;
  const tzMoves = !!utcSeries && ical.conversionMovesOccurrences(utcSeries.start, utcSeries.rrule, zone);
  // Whether this series can be converted at all is known up front (an edit
  // doesn't change what makes it fail), so nothing is shown or offered as
  // converted unless it will be.
  const canConvert = !!utcSeries && !ical.convertSeriesToZone(master.text, zone).error;
  // The occurrence's original slot, and whether it is an override: conversion
  // maps slots, but keeps an override's own chosen times.
  const ridAt = inst && ((inst.rid && ical.parseICalDate(inst.rid, {})?.date) || inst.start);
  const isOverride = !!inst?.rid && !!master && ical.components(ical.parse(master.text), 'VEVENT').some(v => {
    const r = ical.prop(v, 'RECURRENCE-ID');
    const d = r && ical.parseICalDate(r.value, r.params)?.date;
    return !!d && +d === +ridAt;
  });
  // The occurrence is shown where it will be: for a series this edit
  // converts, at its converted position (the first occurrence's local time)
  // rather than where it drifted to — unless it's an override, which keeps
  // its times. The times on screen are what gets saved, so an untouched time
  // lands there in every scope.
  const shownTimes = converting => converting && canConvert && !isOverride
    ? { start: ical.convertedSlot(inst.start, utcSeries.start, zone), end: ical.convertedSlot(inst.end, utcSeries.start, zone) }
    : { start: inst?.start, end: inst?.end };
  if (utcSeries?.calweb) ({ start, end } = shownTimes(true));
  // shown is the baseline an edit is measured from when nothing is converted.
  let shown = { start, end };

  const dlg = html`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="${inst ? 'Edit event' : 'New event'}">
    <h2>${inst ? 'Edit event' : 'New event'}</h2>
    ${isRecurring && frag`<label class="row" style="margin-bottom:.6rem">Apply to
      <select id="ev-scope">
        <option value="one">only this event</option>
        ${canFuture && frag`<option value="future">this and all following events</option>`}
        <option value="all">all events in the series</option>
      </select></label>`}
    ${canConvert && utcSeries.calweb && tzMoves && frag`<p class="note" id="ev-tzfix">This series was stored in UTC; saving fixes its times across daylight-saving changes.</p>`}
    ${canConvert && !utcSeries.calweb && tzMoves && frag`<div id="ev-tzopt">
      <label class="row"><input type="checkbox" id="ev-tzfix-opt"> Keep this series at a fixed local time</label>
      <div class="note" id="ev-tzpreview" hidden></div>
    </div>`}
    <div class="grid">
      <label class="full">Title<input id="ev-title" type="text" value="${inst?.summary === '(no title)' ? '' : inst?.summary ?? ''}" placeholder="Add a title"></label>
      <label>Calendar<select id="ev-cal"></select></label>
      <label class="row" style="align-self:end"><input type="checkbox" id="ev-allday" ${(inst?.allDay || o.allDay) ? 'checked' : ''}> All day</label>
      <label>Starts<span class="dt-pair">
        <input id="ev-start" type="date">
        <input id="ev-start-time" type="text" class="time-field" list="ev-times" autocomplete="off" spellcheck="false">
      </span></label>
      <label>Ends<span class="dt-pair">
        <input id="ev-end" type="date">
        <input id="ev-end-time" type="text" class="time-field" list="ev-times" autocomplete="off" spellcheck="false">
      </span></label>
      <datalist id="ev-times"></datalist>
      <label>Repeat<select id="ev-freq" ${isRecurring ? 'disabled' : ''}>
        <option value="">Does not repeat</option>
        <option value="DAILY">Every day</option>
        <option value="WEEKLY">Every week</option>
        <option value="MONTHLY">Every month</option>
        <option value="YEARLY">Every year</option>
      </select></label>
      <label id="ev-interval-wrap" hidden>Every<span style="display:flex;gap:.3rem;align-items:center">
        <input id="ev-interval" type="number" min="1" max="365" value="1" style="width:4.5rem" ${isRecurring ? 'disabled' : ''}>
        <span id="ev-interval-unit"></span>
      </span></label>
      <label id="ev-byday-wrap" class="full" hidden>On<span id="ev-byday" class="byday"></span></label>
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
      <label class="full" id="ev-remind-field">Reminders</label>
      <label class="full">Location<input id="ev-loc" type="text" value="${inst?.location ?? ''}" placeholder="Add a location"></label>
      <label class="full">Meeting link<input id="ev-url" type="url" value="${inst?.url ?? ''}" placeholder="e.g. https://zoom.us/j/123456789"></label>
      <label class="full row"><input type="checkbox" id="ev-private" ${inst?.confidential ? 'checked' : ''}> Private — anyone this calendar is shared with read-only sees only “Busy”, not the title or details</label>
      <label class="full" id="ev-guests">Guests</label>
      <span class="full hint" id="ev-guest-hint" hidden>When you save, your guests are notified of the change automatically; removing a guest cancels their copy.</span>
      <span class="full avail" id="ev-avail"></span>
      <label class="full">Notes<textarea id="ev-desc" rows="3">${inst?.description ?? ''}</textarea></label>
    </div>
    <p class="error" id="ev-error"></p>
    <div class="actions">
      ${inst && frag`<button class="danger" id="ev-delete">Delete…</button>`}
      <button id="ev-cancel">Cancel</button>
      <button class="primary" id="ev-save">Save</button>
    </div>
  </div></div>`;

  const q = sel => $(sel, dlg);
  const close = onClose || (() => dlg.remove());
  // Scrim click / Esc cancel the editor (unless a child, e.g. the guest
  // list's autocomplete, handled the key first).
  openOverlay(dlg, close);
  // Enter submits from a single-line text field; the guest field owns Enter
  // for its autocomplete.
  submitOnEnter(dlg, q('#ev-save'), t => t.id === 'ev-guest');
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
    calSel.append(html`<option value="${c.href}">${c.displayName || c.name}</option>`);
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

  q('#ev-tzfix-opt')?.addEventListener('change', ev => {
    // Opting in (or out) moves the shown occurrence to where it will be saved.
    ({ start, end } = shownTimes(ev.target.checked));
    shown = { start, end };
    syncDateInputs();
    prevStart = readPair(startD, startT);
    const box = q('#ev-tzpreview');
    box.hidden = !ev.target.checked;
    if (!ev.target.checked) return;
    const rows = ical.conversionPreview(utcSeries.start, utcSeries.rrule, zone, now, 5);
    const fmt = d => d.toLocaleString(undefined, { timeZone: zone, weekday: 'short', day: 'numeric',
      month: 'short', hour: 'numeric', minute: '2-digit', hour12: !!store.state.prefs.hour12 });
    const time = d => d.toLocaleTimeString(undefined, { timeZone: zone, hour: 'numeric', minute: '2-digit',
      hour12: !!store.state.prefs.hour12 });
    box.replaceChildren(rows == null
      ? html`<p>Occurrences during daylight-saving time move by the clock change, so every occurrence keeps the local time of the first one.</p>`
      : rows.length
        ? html`<p>These upcoming occurrences change:</p>`
        : html`<p>No upcoming occurrences change.</p>`);
    for (const r of rows || []) box.append(html`<p>${fmt(r.before)} → ${time(r.after)}</p>`);
  });

  // date/time inputs: a native date input plus our own time text field. The
  // time is not the native datetime-local half because browsers format that
  // per the *browser* locale (Chromium ignores the element's lang attribute),
  // so the app's 12/24-hour preference was not honored. Formatting and parsing
  // the field ourselves makes the clock pref authoritative; a datalist offers
  // half-hour choices in the same format. The all-day toggle hides the time
  // fields rather than switching input types.
  const hour12 = !!store.state.prefs.hour12;
  const startD = q('#ev-start'), endD = q('#ev-end');
  const startT = q('#ev-start-time'), endT = q('#ev-end-time');
  for (let i = 0; i < 48; i++) {
    q('#ev-times').append(html`<option value="${formatTimeOfDay(new Date(2000, 0, 1, i >> 1, (i % 2) * 30), hour12)}"></option>`);
  }
  const allDayBox = q('#ev-allday');
  // start/end are device-local midnights for all-day data and instants for
  // timed data; each shows its calendar date accordingly.
  const datesAreLocal = !!(inst ? inst.allDay : o.allDay);
  const dateOf = d => datesAreLocal ? localDate(d) : dateValueIn(d, zone);
  const timeOf = d => { const f = zoneFields(d, zone); return formatHM(f.h, f.mi, hour12); };
  function syncDateInputs() {
    const allDay = allDayBox.checked;
    startT.hidden = endT.hidden = allDay;
    startD.value = dateOf(start);
    // all-day shows the inclusive last day, not the exclusive DTEND date
    endD.value = allDay && datesAreLocal ? localDate(new Date(Math.max(+start, +end - DAY))) : dateOf(end);
    if (!allDay) { startT.value = timeOf(start); endT.value = timeOf(end); }
  }
  syncDateInputs();

  // readPair/setPair combine a date input and its time field. All-day reads a
  // device-local date; timed reads wall-clock fields in the display zone and
  // resolves them to an instant (a skipped time moves forward, a repeated
  // one takes the earlier instant — see zonedToInstant). A time the parser
  // doesn't understand reads as null (flagged .invalid).
  const readPair = (dInp, tInp) => {
    if (!dInp.value) return null;
    if (allDayBox.checked) return new Date(dInp.value + 'T00:00');
    const t = parseTimeOfDay(tInp.value);
    if (!t) return null;
    return zonedToInstant(fieldsFromInputs(dInp.value, t), zone);
  };
  const setPair = (dInp, tInp, d) => {
    if (allDayBox.checked) { dInp.value = localDate(d); return; }
    dInp.value = dateValueIn(d, zone);
    tInp.value = timeOf(d);
  };
  // Rewrite whatever the user typed ("130p", "9.15") in the canonical format
  // once they leave the field, and mark it when it didn't parse.
  const normalize = tInp => {
    const t = parseTimeOfDay(tInp.value);
    tInp.classList.toggle('invalid', !t);
    if (t) tInp.value = formatTimeOfDay(new Date(2000, 0, 1, t.h, t.m), hour12);
  };

  // Moving the start drags the end with it, keeping the duration fixed — so
  // changing a 1–2pm slot to 5pm makes it 5–6pm. We track the previous start
  // and shift the end by the same delta. Re-baseline whenever the all-day
  // toggle rewrites both inputs.
  let prevStart = readPair(startD, startT);
  function startChanged() {
    const ns = readPair(startD, startT);
    if (!ns) return;
    const delta = prevStart ? +ns - +prevStart : 0;
    const ce = readPair(endD, endT);
    if (delta && ce) setPair(endD, endT, new Date(+ce + delta));
    prevStart = ns;
  }
  startD.addEventListener('input', startChanged);
  // 'change', not 'input': while typing "13:00" the partial "1" already parses
  // (as 01:00) and would yank the end around on every keystroke.
  startT.addEventListener('change', () => { normalize(startT); startChanged(); });
  endT.addEventListener('change', () => normalize(endT));
  allDayBox.addEventListener('change', () => { syncDateInputs(); prevStart = readPair(startD, startT); });

  // repeat controls
  const freqSel = q('#ev-freq'), endsWrap = q('#ev-ends-wrap'), endsSel = q('#ev-ends');
  const intervalWrap = q('#ev-interval-wrap'), bydayWrap = q('#ev-byday-wrap'), bydayBox = q('#ev-byday');
  // Weekday toggles for weekly repeats, ordered from the user's week start.
  const WEEKDAYS = [['SU', 'Sun'], ['MO', 'Mon'], ['TU', 'Tue'], ['WE', 'Wed'], ['TH', 'Thu'], ['FR', 'Fri'], ['SA', 'Sat']];
  const bydaySet = new Set(rrulePreset?.byday ?? []);
  // A new weekly repeat defaults to (and always includes) its own start weekday,
  // so the DTSTART is a real occurrence — otherwise a BYDAY that omits it makes
  // the series start on a different day than the user picked.
  // The weekday is the start's day in the zone the series will recur in.
  const weekdayOf = d => (allDayBox.checked ? d.getDay() : weekdayIn(d, zone));
  if (!inst) bydaySet.add(WEEKDAYS[weekdayOf(start)][0]);
  const startWk = store.state.prefs.weekStart === 0 ? 0 : 1;
  for (let i = 0; i < 7; i++) {
    const [code, label] = WEEKDAYS[(startWk + i) % 7];
    const b = html`<button type="button" class="wd ${bydaySet.has(code) ? 'on' : ''}" data-wd="${code}" ${isRecurring ? 'disabled' : ''}>${label}</button>`;
    b.addEventListener('click', () => { b.classList.toggle('on') ? bydaySet.add(code) : bydaySet.delete(code); });
    bydayBox.append(b);
  }
  if (rrulePreset) {
    freqSel.value = rrulePreset.freq;
    endsSel.value = rrulePreset.ends;
    q('#ev-interval').value = rrulePreset.interval ?? 1;
    q('#ev-count').value = rrulePreset.count ?? '10';
    q('#ev-until').value = rrulePreset.until ?? '';
  } else {
    // complex RRULE: keep it verbatim, disable the preset UI
    freqSel.append(html`<option value="CUSTOM" selected>Custom repeat (kept as is)</option>`);
  }
  const FREQ_UNIT = { DAILY: 'days', WEEKLY: 'weeks', MONTHLY: 'months', YEARLY: 'years' };
  function syncRepeat() {
    const on = !!freqSel.value && freqSel.value !== 'CUSTOM';
    intervalWrap.hidden = !on;
    q('#ev-interval-unit').textContent = FREQ_UNIT[freqSel.value] || '';
    bydayWrap.hidden = freqSel.value !== 'WEEKLY';
    endsWrap.hidden = !on;
    const byCount = endsSel.value === 'count';
    q('#ev-count').hidden = !byCount;
    q('#ev-count-unit').hidden = !byCount;
    q('#ev-until').hidden = endsSel.value !== 'until';
  }
  syncRepeat();
  freqSel.addEventListener('change', syncRepeat);
  endsSel.addEventListener('change', syncRepeat);

  function readTimes() {
    const allDay = allDayBox.checked;
    const sv = startD.value, ev = endD.value;
    if (!sv || !ev) return {};
    if (allDay) {
      const s = new Date(sv + 'T00:00');
      return { allDay, start: s, end: new Date(+new Date(ev + 'T00:00') + DAY), sVal: sv, eVal: ev };
    }
    const s = readPair(startD, startT), e = readPair(endD, endT);
    if (!s || !e) return { badTime: true };
    return { allDay, start: s, end: e, sVal: sv, eVal: ev };
  }

  // guests + availability: the chips widget mutates `guests` and calls back so
  // the hint and the Check-availability button track the list.
  function syncGuestUI() {
    q('#ev-guest-hint').hidden = !guests.length;
    const box = q('#ev-avail');
    box.replaceChildren();
    if (!guests.length) return;
    const check = html`<button type="button" class="ghost" id="ev-check">Check availability</button>`;
    check.addEventListener('click', () =>
      checkAvailability(box, guests, readTimes, inst ? { start: inst.start, end: inst.end } : null));
    box.append(check);
  }
  q('#ev-guests').append(createGuestField(guests, syncGuestUI));
  syncGuestUI();

  // reminders: read the stored master's alarms onto the list control. A shape
  // we can't reproduce (an EMAIL alarm, an absolute or end-relative trigger)
  // surfaces as "kept as is" and is left untouched unless the user replaces it —
  // the same contract the Custom repeat option uses.
  const curReminders = masterVevent ? ical.readReminders(masterVevent) : { triggers: [], custom: false };
  const remField = createReminderField(curReminders.triggers, { custom: curReminders.custom });
  q('#ev-remind-field').append(remField.el);

  // Live validation: keep Save disabled until there's a title and a sane
  // start/end, and surface a time problem inline as the user edits rather than
  // only after a click. A read-only share disables Save wholesale below, so
  // skip live validation there.
  const saveBtn = q('#ev-save');
  function validate() {
    const t = readTimes();
    let msg = '';
    if (t.badTime) msg = `Please enter a time like ${hour12 ? '1:30 PM' : '13:00'}.`;
    else if (t.start && t.end && t.end <= t.start) msg = 'Please pick an end after the start.';
    q('#ev-error').textContent = msg;
    saveBtn.disabled = !q('#ev-title').value.trim() || !t.start || !t.end || !!msg;
  }
  if (!roCal) {
    q('#ev-title').addEventListener('input', validate);
    startD.addEventListener('input', validate);
    endD.addEventListener('change', validate);
    startT.addEventListener('change', validate);
    endT.addEventListener('change', validate);
    allDayBox.addEventListener('change', validate);
    validate();
  }

  // save ------------------------------------------------------------------
  saveBtn.addEventListener('click', busyWhile(saveBtn, async () => {
    const t = readTimes();
    const title = q('#ev-title').value.trim();
    // Save is disabled while any of these hold, so these guard the rare
    // Enter/edge path rather than the normal click.
    if (!title) { q('#ev-title').focus(); return; }
    if (t.badTime) {
      q('#ev-error').textContent = `Please enter a time like ${hour12 ? '1:30 PM' : '13:00'}.`;
      return;
    }
    if (!t.start || !t.end || t.end <= t.start) {
      q('#ev-error').textContent = 'Please pick an end after the start.';
      return;
    }
    const scope = isRecurring ? q('#ev-scope').value : '';
    const rrule = freqSel.value === 'CUSTOM' ? (masterRRule || inst.rrule) : ical.buildRRule({
      freq: freqSel.value, ends: endsSel.value,
      interval: q('#ev-interval').value, byday: (() => {
        // Keep the start weekday in a weekly BYDAY so the series still occurs on
        // its own start date even if the user changed the date after picking days.
        const days = [...bydaySet];
        const wd = t.start && WEEKDAYS[weekdayOf(t.start)][0];
        if (freqSel.value === 'WEEKLY' && days.length && wd && !days.includes(wd)) days.push(wd);
        return days;
      })(),
      count: q('#ev-count').value, until: q('#ev-until').value,
    });
    const fields = {
      uid: inst?.uid || crypto.randomUUID(),
      summary: title,
      allDay: t.allDay,
      start: t.allDay ? t.sVal : t.start.toISOString(),
      end: t.allDay ? localDate(new Date(+new Date(t.eVal + 'T00:00') + DAY)) : t.end.toISOString(),
      location: q('#ev-loc').value.trim(),
      url: q('#ev-url').value.trim(),
      description: q('#ev-desc').value.trim(),
      organizer: guests.length ? store.myEmail() : '',
      attendees: guests,
      rrule,
      confidential: q('#ev-private').checked,
      // A list of trigger durations, one DISPLAY VALARM each; [] clears reminders.
      // null means the stored alarms are ones we don't model — leave them as they
      // are (the control was never switched out of "kept as is").
      alarms: remField.getTriggers(),
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
        ? guests.map(g => `ATTENDEE;PARTSTAT=${g.partstat || 'NEEDS-ACTION'}${g.room ? ';CUTYPE=ROOM' : ''};RSVP=TRUE:mailto:${g.email}`)
        : null,
      // null removes CLASS, restoring the PUBLIC default.
      CLASS: fields.confidential ? 'CLASS:CONFIDENTIAL' : null,
    };
    // applyReminder rewrites the alarms on an in-place master edit, but only once
    // the user has edited the reminder list — a "kept as is" selection (null)
    // leaves whatever alarms setMasterProps already preserved.
    const applyReminder = text => fields.alarms == null
      ? text : ical.setMasterAlarms(text, fields.alarms, fields.summary);
    // UTC-series conversion for this save: automatic for a calweb series,
    // opt-in otherwise, in every scope.
    const optIn = !!q('#ev-tzfix-opt')?.checked;
    const convert = canConvert && (utcSeries.calweb || optIn);
    // convertText converts an object whose master times the edit doesn't
    // touch (the head of a "this and following" split; the series an "only
    // this" override is added to); "all events" edits go through
    // ical.convertAndMove, which applies the typed times after converting.
    // A conversion that can't be done exactly never rewrites anything: an
    // automatic one saves the edit as it is and says why, an opted-in one
    // stops so the user can decide.
    const convertText = text => {
      if (!convert) return { text };
      const r = ical.convertSeriesToZone(text, zone);
      if (!r.error) return { text: r.text, converted: true };
      if (optIn) return { error: `Can’t keep this series at a fixed local time: ${r.error}` };
      toast(`Saved, but the series’ times weren’t fixed: ${r.error}`);
      return { text };
    };
    try {
      if (!inst) {
        await store.putObject(store.eventHref(calSel.value, fields.uid),
          ical.buildEvent({ ...fields, tz: zone }), null);
      } else if (isRecurring && scope === 'one') {
        // An override names its occurrence, and is written, in the series'
        // own form (its zone, or UTC for a UTC series). Converting the series
        // first, it names the converted slot in the display zone.
        let base = master.text, tz = anchorZone || zone;
        let ridLine = store.ridLine('RECURRENCE-ID', inst, masterDs);
        const conv = convertText(master.text);
        if (conv.error) { q('#ev-error').textContent = conv.error; return; }
        if (conv.converted) {
          base = conv.text;
          tz = zone;
          ridLine = ical.occurrenceLine('RECURRENCE-ID', ical.convertedSlot(ridAt, utcSeries.start, zone),
            inst.allDay, { value: '', params: { TZID: zone } });
        }
        const text = ical.addOverride(base, ical.eventLines({
          ...fields, rrule: '', tz, recurrenceIdLine: ridLine,
        }));
        await store.putObject(inst.href, text, master.etag);
      } else if (isRecurring && scope === 'future') {
        // "This and following": split the series. Truncate the original before
        // this occurrence (dropping any now-orphaned forward overrides), then
        // create a fresh series from here carrying the edited fields.
        const headUntil = t.allDay
          ? ical.dateOnly(localDate(new Date(+inst.start - DAY)))
          : ical.utc(new Date(+inst.start - 1000).toISOString());
        const split = ical.splitSeriesRRules(masterStart, masterRRule, inst.start, headUntil, anchorZone);
        if (!split) { q('#ev-error').textContent = 'This repeat is too complex to split.'; return; }
        const head = convertText(ical.dropForward(
          ical.setMasterProps(master.text, { RRULE: `RRULE:${split.head}` }), inst.start));
        if (head.error) { q('#ev-error').textContent = head.error; return; }
        await store.putObject(inst.href, head.text, master.etag);
        // The tail keeps the series' zone: the display zone once converted, UTC
        // for a UTC series left as it is. It starts at the times on screen,
        // which for a converting edit already show the converted position.
        const tailZone = head.converted ? zone : (anchorZone || zone);
        const fwd = { ...fields, uid: crypto.randomUUID(), rrule: split.tail, tz: tailZone };
        await store.putObject(store.eventHref(calSel.value, fwd.uid), ical.buildEvent(fwd), null);
      } else if (isRecurring) {
        // "All events": edit the master VEVENT in place so overrides, alarms,
        // EXDATEs, and unmodeled properties survive. Move the series by the
        // change made to *this* occurrence rather than re-anchoring it to the
        // clicked date, and never rewrite the RRULE — the server expands
        // recurrences, so the master rule isn't available here to reconstruct.
        // Start and end move separately, each by its own form's delta (a zoned
        // value by wall-clock time in its zone, UTC by elapsed time).
        // Converting a UTC series: convert first, then apply the typed times
        // as a target on the converted series (ical.convertAndMove), so the
        // edited occurrence lands exactly where the user put it.
        let out = null;
        if (convert) {
          const r = ical.convertAndMove(applyReminder(ical.setMasterProps(master.text, commonProps)), zone,
            shown, { start: t.start, end: t.end });
          if (!r.error) out = r.text;
          else if (optIn) { q('#ev-error').textContent = `Can’t keep this series at a fixed local time: ${r.error}`; return; }
          else toast(`Saved, but the series’ times weren’t fixed: ${r.error}`);
        }
        if (out == null) {
          // Without conversion (or after an automatic one failed) the stored
          // series keeps its form.
          const m0 = ical.components(ical.parse(master.text), 'VEVENT')
            .find(v => !ical.prop(v, 'RECURRENCE-ID'));
          const props = { ...commonProps };
          const ds = m0 && ical.prop(m0, 'DTSTART');
          const de = m0 && ical.prop(m0, 'DTEND');
          const unsupported = 'This series stores its times in a time zone this app can’t work with, so its date and time can’t be changed here.';
          if (ds && +t.start !== +shown.start) {
            const l = ical.shiftDate('DTSTART', ds, { from: shown.start, to: t.start });
            if (!l) { q('#ev-error').textContent = unsupported; return; }
            props.DTSTART = l;
          }
          if (de && +t.end !== +shown.end) {
            const l = ical.shiftDate('DTEND', de, { from: shown.end, to: t.end });
            if (!l) { q('#ev-error').textContent = unsupported; return; }
            props.DTEND = l;
          }
          out = applyReminder(ical.setMasterProps(master.text, props));
        }
        await store.putObject(inst.href, out, master.etag);
      } else {
        // Plain event: rewrite the master's fields in place (no series to
        // re-anchor), preserving alarms and unmodeled properties. A zoned event
        // keeps its zone; a one-off stored in UTC stays UTC (it's correct as
        // it is) unless it gains a repeat, which makes it a zoned series.
        const dsZone = masterDs && !masterDs.params?.VALUE && ical.seriesZone(masterDs);
        const tz = dsZone && dsZone !== 'UTC' ? dsZone
          : dsZone === 'UTC' && !fields.rrule ? 'UTC' : zone;
        const props = {
          ...commonProps,
          DTSTART: fields.allDay ? `DTSTART;VALUE=DATE:${ical.dateOnly(fields.start)}` : ical.dtLine('DTSTART', new Date(fields.start), tz),
          DTEND: fields.allDay ? `DTEND;VALUE=DATE:${ical.dateOnly(fields.end)}` : ical.dtLine('DTEND', new Date(fields.end), tz),
          RRULE: fields.rrule ? `RRULE:${fields.rrule}` : null,
        };
        await store.putObject(inst.href, applyReminder(ical.setMasterProps(master.text, props)), master.etag);
      }
      // Calendar changed: relocate the object after the in-place edit. The
      // picker is locked for the single-occurrence scope, and the "this and
      // following" split already put the new series on the chosen calendar, so a
      // differing value here only applies to the whole-object edit paths.
      if (inst && scope !== 'future' && calSel.value && calSel.value !== inst.calHref) {
        await store.moveObject(inst.href, calSel.value + inst.href.slice(inst.href.lastIndexOf('/') + 1));
      }
      close();
      toast(guests.length ? 'Saved — invitations are on their way.' : 'Saved.');
      await store.refreshEvents();
    } catch (e) {
      if (e.conflict) { q('#ev-error').textContent = e.message; store.refreshEvents().catch(() => {}); }
      else showError(e);
    }
  }));

  // delete ----------------------------------------------------------------
  $('#ev-delete', dlg)?.addEventListener('click', async () => {
    const scope = isRecurring ? q('#ev-scope').value : 'all';
    const msg = scope === 'one' ? 'Delete only this occurrence?'
      : scope === 'future' ? 'Delete this and all following events?'
      : isRecurring ? 'Delete the whole series?' : 'Delete this event?';
    if (!await confirmDialog(msg, { confirmLabel: 'Delete', danger: true })) return;
    try {
      await store.deleteEvent(inst, scope);
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

  q('#ev-title').focus();
  return dlg;
}
