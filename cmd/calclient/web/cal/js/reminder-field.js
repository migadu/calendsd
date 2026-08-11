// reminder-field.js — the reminder (VALARM) list control shared by the event
// and task editors: a list of reminder rows, each a preset offset or a custom
// "N minutes/hours/days/weeks before/after", with Add and per-row remove. It
// only writes plain DISPLAY VALARMs; when they actually fire is up to whichever
// CalDAV client reads them (calweb itself doesn't pop notifications).
//
// An object already carrying an alarm the editor can't model (an EMAIL alarm,
// an absolute trigger, a mismatched RELATED) opens "kept as is": the existing
// alarms are preserved untouched until the user hits Replace, which clears them
// and switches to an editable, empty list — the same contract the single-alarm
// dropdown used, just extended to a list.

import * as ical from '/shared/ical.js';
import { $, html, frag } from '/shared/dom.js';

const UNITS = ['minutes', 'hours', 'days', 'weeks'];

// createReminderField(triggers, opts) → { el, getTriggers }.
//   triggers: iCal trigger strings to prefill (e.g. ['-PT15M', '-P1D']).
//   opts.custom: the stored alarms are unmodeled — open in "kept as is".
// getTriggers() returns the current list of trigger strings, or null to mean
// "leave the stored alarms exactly as they are" (kept-as-is, never replaced).
export function createReminderField(triggers = [], opts = {}) {
  let custom = !!opts.custom;
  const el = html`<span class="reminders"></span>`;
  const list = html`<span class="rem-list"></span>`;
  const addBtn = html`<button type="button" class="ghost rem-add">＋ Add reminder</button>`;
  addBtn.addEventListener('click', () => list.append(makeRow('-PT15M')));

  // makeRow renders one reminder: a preset dropdown with a "Custom…" escape
  // hatch that reveals amount/unit/before-after controls. row._trigger() reads
  // the row back to an iCal trigger duration.
  function makeRow(trigger) {
    const row = html`<span class="rem-row">
      <select class="rem-preset" aria-label="Reminder time">
        ${ical.REMINDER_PRESETS.map(([v, label]) => frag`<option value="${v}">${label}</option>`)}
        <option value="custom">Custom…</option>
      </select>
      <span class="rem-custom" hidden>
        <input type="number" class="rem-amount" min="1" max="999" value="10" aria-label="Amount">
        <select class="rem-unit" aria-label="Unit">${UNITS.map(u => frag`<option value="${u}">${u}</option>`)}</select>
        <select class="rem-when" aria-label="Before or after">
          <option value="before">before</option>
          <option value="after">after</option>
        </select>
      </span>
      <button type="button" class="rem-remove" aria-label="Remove reminder">×</button>
    </span>`;
    const preset = $('.rem-preset', row), custBox = $('.rem-custom', row);
    const amount = $('.rem-amount', row), unit = $('.rem-unit', row), when = $('.rem-when', row);
    // A trigger that matches a preset selects it; anything else opens Custom,
    // seeded from the trigger's largest clean unit.
    const hit = ical.REMINDER_PRESETS.find(([v]) => ical.parseDuration(v) === ical.parseDuration(trigger));
    if (hit) {
      preset.value = hit[0];
    } else {
      const off = ical.triggerToOffset(trigger) || { when: 'before', amount: 15, unit: 'minutes' };
      preset.value = 'custom';
      amount.value = Math.max(1, off.amount); unit.value = off.unit;
      when.value = off.when === 'after' ? 'after' : 'before';
    }
    const syncCustom = () => { custBox.hidden = preset.value !== 'custom'; };
    syncCustom();
    preset.addEventListener('change', syncCustom);
    $('.rem-remove', row).addEventListener('click', () => row.remove());
    row._trigger = () => preset.value !== 'custom' ? preset.value
      : ical.offsetToTrigger({ when: when.value, amount: amount.value, unit: unit.value });
    return row;
  }

  // "kept as is": show a note and a Replace button rather than editable rows, so
  // an alarm shape we can't model survives an unrelated save untouched.
  function renderKept() {
    const box = html`<span class="rem-kept">Custom reminder — kept as is.
      <button type="button" class="ghost rem-replace">Replace…</button></span>`;
    $('.rem-replace', box).addEventListener('click', () => { custom = false; renderEditable([]); });
    el.replaceChildren(box);
  }
  function renderEditable(trigs) {
    list.replaceChildren();
    for (const t of trigs) list.append(makeRow(t));
    el.replaceChildren(list, addBtn);
  }

  if (custom) renderKept(); else renderEditable(triggers);

  return {
    el,
    getTriggers: () => custom ? null
      : [...list.querySelectorAll('.rem-row')].map(r => r._trigger()).filter(Boolean),
  };
}
