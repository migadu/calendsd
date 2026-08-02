// ui.js — calweb's app-level UI helpers: toast, error display, and the
// preference-aware formatters every view shares. Kept out of main.js so
// widgets can import these without pulling in the boot/routing entry point
// (and its module-load side effects).

import * as store from '/shared/store.js';
import { $, raw, esc } from '/shared/dom.js';
import { DAY, sameDay } from '/shared/time.js';

let toastTimer = null;
export function toast(msg, isError = false) {
  const t = $('#toast');
  t.textContent = msg;
  t.classList.toggle('error', isError);
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, isError ? 6000 : 3000);
}

// linkify escapes text, turns http(s) URLs into clickable links (so a meeting
// link pasted into a location or notes field is joinable), and renders
// newlines as <br>. Returns safe markup, directly interpolatable.
export function linkify(text) {
  const re = /(https?:\/\/[^\s<]+)/g;
  let out = '', last = 0, m;
  while ((m = re.exec(text))) {
    out += esc(text.slice(last, m.index));
    out += `<a href="${esc(m[1])}" target="_blank" rel="noreferrer noopener">${esc(m[1])}</a>`;
    last = m.index + m[1].length;
  }
  out += esc(text.slice(last));
  return raw(out.replace(/\n/g, '<br>'));
}

// showError maps any thrown error to a friendly toast (store.DavError
// already carries friendly text).
export function showError(e) {
  console.error(e);
  toast(e?.message || 'Something went wrong.', true);
  if (e?.conflict) store.refreshEvents().catch(() => {});
}

// tzOpt adds the chosen display time zone to a toLocale* options object
// (empty pref = the device's own zone).
function tzOpt(opts) {
  const tz = store.state.prefs.tz;
  return tz ? { ...opts, timeZone: tz } : opts;
}
export function fmtTime(d) {
  return d.toLocaleTimeString([], tzOpt({ hour: 'numeric', minute: '2-digit', hour12: store.state.prefs.hour12 }));
}
export function fmtDate(d) {
  return d.toLocaleDateString([], tzOpt({ weekday: 'short', month: 'short', day: 'numeric' }));
}
export function fmtRange(inst) {
  if (inst.allDay) {
    const days = Math.round((inst.end - inst.start) / DAY);
    return fmtDate(inst.start) + (days > 1 ? ' – ' + fmtDate(new Date(+inst.end - DAY)) : '') + ' · all day';
  }
  return fmtDate(inst.start) + ' ' + fmtTime(inst.start) + ' – ' +
    (sameDay(inst.start, inst.end) ? '' : fmtDate(inst.end) + ' ') + fmtTime(inst.end);
}
