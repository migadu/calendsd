// event-dialog.js — the New/Edit event dialog: plain-language fields on top
// of PUT .ics and series-vs-occurrence editing for recurring events. The
// guest chips live in guest-field.js and the availability strip in
// availability.js; this module owns the form itself and the save paths.

import * as store from '/shared/store.js';
import * as ical from '/shared/ical.js';
import { $, html, frag } from '/shared/dom.js';
import { DAY, localDate, formatTimeOfDay, parseTimeOfDay } from '/shared/time.js';
import { toast, showError } from './ui.js';
import { createGuestField } from './guest-field.js';
import { checkAvailability } from './availability.js';
import { confirmDialog, openOverlay, submitOnEnter } from '/shared/dialogs.js';

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
  const rrulePreset = inst ? ical.parseRRulePreset(inst.rrule) : { freq: '', ends: 'never' };
  const isRecurring = !!(inst && (inst.rrule || inst.rid));
  const guests = (inst?.attendees ?? []).map(a => ({ ...a }));

  const dlg = html`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="${inst ? 'Edit event' : 'New event'}">
    <h2>${inst ? 'Edit event' : 'New event'}</h2>
    ${isRecurring && frag`<label class="row" style="margin-bottom:.6rem">Apply to
      <select id="ev-scope">
        <option value="one">only this event</option>
        <option value="all">all events in the series</option>
      </select></label>`}
    <div class="grid">
      <label class="full">Title<input id="ev-title" type="text" value="${inst?.summary === '(no title)' ? '' : inst?.summary ?? ''}" placeholder="Add a title"></label>
      <label>Calendar<select id="ev-cal"></select></label>
      <label class="row" style="align-self:end"><input type="checkbox" id="ev-allday" ${inst?.allDay ? 'checked' : ''}> All day</label>
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
      <label>Reminder<select id="ev-remind">
        <option value="">No reminder</option>
        ${ical.REMINDER_PRESETS.map(([val, label]) => frag`<option value="${val}">${label}</option>`)}
      </select></label>
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
  const close = () => dlg.remove();
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
  function syncDateInputs() {
    const allDay = allDayBox.checked;
    startT.hidden = endT.hidden = allDay;
    startD.value = localDate(start);
    // all-day shows the inclusive last day, not the exclusive DTEND date
    endD.value = allDay ? localDate(new Date(Math.max(+start, +end - DAY))) : localDate(end);
    if (!allDay) { startT.value = formatTimeOfDay(start, hour12); endT.value = formatTimeOfDay(end, hour12); }
  }
  syncDateInputs();

  // readPair/setPair combine a date input and its time field into a Date.
  // A time the parser doesn't understand reads as null (flagged .invalid).
  const readPair = (dInp, tInp) => {
    if (!dInp.value) return null;
    const d = new Date(dInp.value + 'T00:00');
    if (allDayBox.checked) return d;
    const t = parseTimeOfDay(tInp.value);
    if (!t) return null;
    d.setHours(t.h, t.m);
    return d;
  };
  const setPair = (dInp, tInp, d) => {
    dInp.value = localDate(d);
    if (!allDayBox.checked) tInp.value = formatTimeOfDay(d, hour12);
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
  if (rrulePreset) {
    freqSel.value = rrulePreset.freq;
    endsSel.value = rrulePreset.ends;
    q('#ev-count').value = rrulePreset.count ?? '10';
    q('#ev-until').value = rrulePreset.until ?? '';
  } else {
    // complex RRULE: keep it verbatim, disable the preset UI
    freqSel.append(html`<option value="CUSTOM" selected>Custom repeat (kept as is)</option>`);
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

  // reminder: read the stored master's alarm onto the dropdown. A shape we
  // can't reproduce (an EMAIL alarm, several alarms, an absolute trigger)
  // surfaces as "Custom" and is left untouched unless the user picks something
  // else — the same "kept as is" contract the Custom repeat option uses.
  const remindSel = q('#ev-remind');
  const masterVevent = master && ical.components(ical.parse(master.text), 'VEVENT')
    .find(v => !ical.prop(v, 'RECURRENCE-ID'));
  const curReminder = masterVevent ? ical.alarmPreset(masterVevent) : '';
  if (curReminder === 'CUSTOM') {
    remindSel.append(html`<option value="CUSTOM" selected>Custom reminder (kept as is)</option>`);
  } else {
    remindSel.value = curReminder;
  }

  // save ------------------------------------------------------------------
  q('#ev-save').addEventListener('click', async () => {
    const t = readTimes();
    const title = q('#ev-title').value.trim() || 'Untitled event';
    if (t.badTime) {
      q('#ev-error').textContent = `Please enter a time like ${hour12 ? '1:30 PM' : '13:00'}.`;
      return;
    }
    if (!t.start || !t.end || t.end <= t.start) {
      q('#ev-error').textContent = 'Please pick an end after the start.';
      return;
    }
    const rrule = freqSel.value === 'CUSTOM' ? inst.rrule : ical.buildRRule({
      freq: freqSel.value, ends: endsSel.value,
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
      // '' = no reminder; a preset trigger writes one DISPLAY VALARM. 'CUSTOM'
      // means the stored alarm is one we don't model, so leave it as it was.
      alarm: remindSel.value === 'CUSTOM' ? '' : remindSel.value,
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
    // applyReminder rewrites the alarm on an in-place master edit, but only once
    // the user has picked a modeled reminder — a "Custom (kept as is)" selection
    // leaves whatever alarms setMasterProps already preserved.
    const applyReminder = text => remindSel.value === 'CUSTOM'
      ? text : ical.setMasterAlarm(text, fields.alarm, fields.summary);
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
        await store.putObject(inst.href, applyReminder(ical.setMasterProps(master.text, props)), master.etag);
      } else {
        // Plain event: rewrite the master's fields in place (no series to
        // re-anchor), preserving alarms and unmodeled properties.
        const props = {
          ...commonProps,
          DTSTART: fields.allDay ? `DTSTART;VALUE=DATE:${ical.dateOnly(fields.start)}` : `DTSTART:${ical.utc(fields.start)}`,
          DTEND: fields.allDay ? `DTEND;VALUE=DATE:${ical.dateOnly(fields.end)}` : `DTEND:${ical.utc(fields.end)}`,
          RRULE: fields.rrule ? `RRULE:${fields.rrule}` : null,
        };
        await store.putObject(inst.href, applyReminder(ical.setMasterProps(master.text, props)), master.etag);
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
    if (!await confirmDialog(msg, { confirmLabel: 'Delete', danger: true })) return;
    try {
      await store.deleteEvent(inst, one ? 'one' : 'all');
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
}
