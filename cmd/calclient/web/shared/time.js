// time.js — date and time-zone helpers shared by both apps. Pure functions
// only (no DOM, no store): this module is unit-tested under node, and the
// pref-dependent wrappers (display zone, week start) live with their callers.

export const DAY = 864e5;   // ms in 24 hours
export const HOUR = 36e5;   // ms in 1 hour

const p2 = n => String(n).padStart(2, '0');

export function startOfDay(d) { const x = new Date(d); x.setHours(0, 0, 0, 0); return x; }

// addDays moves by exact 24-hour steps, which is what the grid's fixed-height
// columns want. For calendar-date arithmetic across DST changes use nextDate.
export function addDays(d, n) { return new Date(+d + n * DAY); }

// nextDate is the following calendar date (DST-safe: 23- and 25-hour days
// still advance by one date).
export function nextDate(day) { return new Date(day.getFullYear(), day.getMonth(), day.getDate() + 1); }

export function sameDay(a, b) { return a.toDateString() === b.toDateString(); }

// startOfWeek returns the week's first midnight; weekStart is 1 = Monday
// (default) or 0 = Sunday, matching Date#getDay.
export function startOfWeek(d, weekStart = 1) {
  const x = startOfDay(d);
  const shift = (x.getDay() - weekStart + 7) % 7;
  return addDays(x, -shift);
}

// ISO-8601 week number, anchored on the Thursday of the date's ISO week so it
// is stable regardless of the user's Monday/Sunday week-start preference.
export function isoWeek(date) {
  const d = new Date(Date.UTC(date.getFullYear(), date.getMonth(), date.getDate()));
  d.setUTCDate(d.getUTCDate() - ((d.getUTCDay() + 6) % 7) + 3);   // Thursday of this week
  const jan4 = new Date(Date.UTC(d.getUTCFullYear(), 0, 4));
  jan4.setUTCDate(jan4.getUTCDate() - ((jan4.getUTCDay() + 6) % 7) + 3);
  return 1 + Math.round((d - jan4) / (7 * DAY));
}

// ---- input-value formatting ----------------------------------------------

// localDT / localDate format a Date as a datetime-local / date input value.
export function localDT(d) {
  return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}T${p2(d.getHours())}:${p2(d.getMinutes())}`;
}
export function localDate(d) {
  return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}`;
}

export function plusHours(h) { const d = new Date(); d.setHours(d.getHours() + h, 0, 0, 0); return d; }

// ---- time-of-day text fields ---------------------------------------------
// The event editor renders time as its own text field next to a native date
// input, because a native datetime-local's time half is formatted per the
// *browser* locale — Chromium ignores the element's lang attribute — so the
// app's 12/24-hour preference could not be honored there. These two are that
// field's display/parse pair, driven by the hour12 pref.

// formatTimeOfDay renders the wall-clock time of `d`: "13:05" or "1:05 PM".
export function formatTimeOfDay(d, hour12) {
  const h = d.getHours(), m = d.getMinutes();
  if (!hour12) return `${p2(h)}:${p2(m)}`;
  return `${(h % 12) || 12}:${p2(m)} ${h < 12 ? 'AM' : 'PM'}`;
}

// parseTimeOfDay accepts what people type in either clock — "9", "9:30",
// "09.30", "0930", "930", "1pm", "1:30 PM", "12a" — and returns {h, m} in
// 24-hour terms, or null when the text isn't a time. Parsing is deliberately
// clock-agnostic: an am/pm suffix always works, its absence means 24-hour.
export function parseTimeOfDay(s) {
  const m = String(s).trim().toLowerCase().match(/^(\d{1,2})(?:[:.h]?(\d{2}))?\s*(?:([ap])\.?m?\.?)?$/);
  if (!m) return null;
  let h = +m[1];
  const min = m[2] ? +m[2] : 0;
  if (min > 59) return null;
  if (m[3]) {                       // am/pm given: hour must be 1–12
    if (h < 1 || h > 12) return null;
    if (m[3] === 'p' && h !== 12) h += 12;
    else if (m[3] === 'a' && h === 12) h = 0;
  } else if (h > 23) return null;
  return { h, m: min };
}

// ---- display time zone ---------------------------------------------------
// The apps can show a time zone other than the browser's. Event labels
// already format in that zone via toLocale* options; vertical grid geometry
// (block tops, the now-line, day membership) must use the SAME zone or blocks
// drift by the browser↔pref offset. These map a calendar date to the absolute
// instant of its midnight *in the given zone* (tz = IANA name, '' = device).

export function tzOffsetMs(date, tz) {
  // Re-reading the instant's wall clock in tz vs in UTC (both parsed the same
  // way) cancels the browser's own zone, leaving tz's offset from UTC.
  return new Date(date.toLocaleString('en-US', { timeZone: tz }))
       - new Date(date.toLocaleString('en-US', { timeZone: 'UTC' }));
}

export function tzDayStart(day, tz) {
  if (!tz) return startOfDay(day);                 // '' = device zone: local midnight is right
  const guess = Date.UTC(day.getFullYear(), day.getMonth(), day.getDate());
  return new Date(guess - tzOffsetMs(new Date(guess), tz));
}
