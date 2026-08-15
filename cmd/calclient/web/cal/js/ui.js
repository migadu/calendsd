// ui.js — calweb's app-level UI helpers: toast, error display, and the
// preference-aware formatters every view shares. Kept out of main.js so
// widgets can import these without pulling in the boot/routing entry point
// (and its module-load side effects).

import * as store from '/shared/store.js';
import { $, raw, esc, frag } from '/shared/dom.js';
import { DAY, sameDay } from '/shared/time.js';

let toastTimer = null;
export function toast(msg, isError = false) {
  const t = $('#toast');
  // Errors announce assertively, everything else politely; set the role
  // before the text so screen readers pick the live region up in the right
  // mode for this announcement.
  t.setAttribute('role', isError ? 'alert' : 'status');
  t.textContent = msg;
  t.classList.toggle('error', isError);
  t.hidden = false;
  // Click to dismiss — an error can outlast its 6s timer, and this lets the
  // reader clear it early. Assigning each call is idempotent (one handler).
  t.onclick = () => { clearTimeout(toastTimer); t.hidden = true; };
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, isError ? 6000 : 3000);
}

// enableOnValid ties a submit button's enabled state to a live check of an
// input, so an action button only lights up once its field is usable (e.g. a
// Share button waits for an @ in the email). Runs once up front, then on every
// keystroke. Stashes the sync on the button so busyWhile can restore validity
// after an async run rather than blindly re-enabling.
export function enableOnValid(input, btn, valid) {
  const sync = () => { btn.disabled = !valid(input.value); };
  btn._syncValid = sync;
  input.addEventListener('input', sync);
  sync();
}

// busyWhile wraps an async click handler so the button disables and shows the
// shared spinner (.is-busy) for the duration of `fn`, blocking the double-click
// that would otherwise fire a second invite or duplicate event. Afterwards it
// restores the enabled state — re-evaluating enableOnValid if one is attached
// (the field may have been cleared on success), else simply re-enabling.
export function busyWhile(btn, fn) {
  return async (...args) => {
    if (btn.disabled) return;
    btn.disabled = true;
    btn.classList.add('is-busy');
    try { return await fn(...args); }
    finally {
      btn.classList.remove('is-busy');
      if (btn._syncValid) btn._syncValid(); else btn.disabled = false;
    }
  };
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

// scheduleStatusInfo classifies an ATTENDEE SCHEDULE-STATUS value (RFC 6638
// §3.2.9, formatted "code;message") for the organizer's view. The leading digit
// is the class: 1.x pending/sent, 2.x delivered, 3.x recipient problem, 5.x
// delivery failure. Returns null for an empty status or a plain 2.x success —
// the PARTSTAT already conveys engagement, so there's no need to announce
// "delivered". Otherwise returns {label, cls, message} where cls is one of
// pending | bad and message is the server's human-readable detail (a tooltip).
export function scheduleStatusInfo(value) {
  if (!value) return null;
  const semi = value.indexOf(';');
  const code = (semi < 0 ? value : value.slice(0, semi)).trim();
  const message = (semi < 0 ? '' : value.slice(semi + 1).trim()) || value;
  switch (code[0]) {
    case '2': return null;                                   // delivered — stay quiet
    case '1': return { label: code === '1.1' ? 'emailed' : 'queued', cls: 'pending', message };
    case '3': return { label: 'bad address', cls: 'bad', message };
    case '5': return { label: 'not sent', cls: 'bad', message };
    default:  return { label: 'unknown', cls: 'pending', message };
  }
}

// scheduleStatusTag renders scheduleStatusInfo() as a delivery pill, or nothing
// when there's no noteworthy status.
export function scheduleStatusTag(value) {
  const info = scheduleStatusInfo(value);
  if (!info) return '';
  return frag`<span class="type-tag ${info.cls}" title="${info.message}">${info.label}</span>`;
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
