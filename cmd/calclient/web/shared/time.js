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
  // Wall-clock fields of the instant in tz, read back as if they were UTC,
  // minus the instant itself. formatToParts is exact; re-parsing a localized
  // string through Date (the old way) depends on the device's own zone.
  const t = Math.floor(+date / 1000) * 1000;
  return wallMs(zoneFields(new Date(t), tz || deviceZone())) - t;
}

export function tzDayStart(day, tz) {
  if (!tz) return startOfDay(day);                 // '' = device zone: local midnight is right
  const guess = Date.UTC(day.getFullYear(), day.getMonth(), day.getDate());
  return new Date(guess - tzOffsetMs(new Date(guess), tz));
}

// ---- zoned wall-clock time ---------------------------------------------------
// calweb stores timed events as local wall-clock time plus an IANA zone
// (DTSTART;TZID=Europe/Zurich:20260313T090000), so a recurring series keeps
// its local hour across daylight-saving changes (docs/dst-recurring-events.md).
// These helpers convert between instants and a zone's wall-clock fields
// {y, mo, d, h, mi, s} (mo is 1-based) using Intl only.

// deviceZone is the browser's IANA zone name, or '' when it reports none.
export function deviceZone() {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || ''; } catch { return ''; }
}

const fmtCache = new Map();
function zoneFormatter(tz) {
  let f = fmtCache.get(tz);
  if (!f) {
    f = new Intl.DateTimeFormat('en-US', {
      timeZone: tz, hourCycle: 'h23',
      year: 'numeric', month: 'numeric', day: 'numeric',
      hour: 'numeric', minute: 'numeric', second: 'numeric',
    });
    fmtCache.set(tz, f);
  }
  return f;
}

// isKnownZone reports whether Intl can resolve tz (an IANA name). Outlook-style
// names ("W. Europe Standard Time") are not known.
export function isKnownZone(tz) {
  if (!tz) return false;
  try { zoneFormatter(tz); return true; } catch { return false; }
}

// writeZone resolves the zone calweb writes timed events in: the display-zone
// preference, else the device zone, else 'UTC' (today's behavior) when neither
// names a zone Intl knows.
export function writeZone(prefTz) {
  if (isKnownZone(prefTz)) return prefTz;
  const dev = deviceZone();
  return isKnownZone(dev) ? dev : 'UTC';
}

// zoneFields returns the wall-clock fields of an instant in tz.
export function zoneFields(date, tz) {
  const f = { y: 0, mo: 0, d: 0, h: 0, mi: 0, s: 0 };
  for (const p of zoneFormatter(tz).formatToParts(date)) {
    if (p.type === 'year') f.y = +p.value;
    else if (p.type === 'month') f.mo = +p.value;
    else if (p.type === 'day') f.d = +p.value;
    else if (p.type === 'hour') f.h = +p.value % 24;
    else if (p.type === 'minute') f.mi = +p.value;
    else if (p.type === 'second') f.s = +p.value;
  }
  return f;
}

// wallMs reads wall-clock fields as if they were UTC: a timestamp used only
// for calendar arithmetic (differences in wall time, adding days), never as an
// instant.
export function wallMs(f) { return Date.UTC(f.y, f.mo - 1, f.d, f.h, f.mi, f.s || 0); }

// fieldsFromWallMs is wallMs's inverse.
export function fieldsFromWallMs(ms) {
  const d = new Date(ms);
  return { y: d.getUTCFullYear(), mo: d.getUTCMonth() + 1, d: d.getUTCDate(),
    h: d.getUTCHours(), mi: d.getUTCMinutes(), s: d.getUTCSeconds() };
}

// zonedToInstant resolves wall-clock fields in tz to an instant, per the
// DATE-TIME rules of RFC 5545 §3.3.5: a time that occurs twice (a fold) is
// the earlier instant; a time that doesn't exist (a gap) is interpreted with
// the offset in effect before the gap, i.e. it moves forward by the gap's
// size. Candidates come from the offsets around the date and are validated by
// formatting back, so no one-hour or single-correction assumption is made.
export function zonedToInstant(f, tz) {
  const L = wallMs(f);
  const offsets = new Set();
  for (const k of [-3, -1, 0, 1, 3]) offsets.add(tzOffsetMs(new Date(L + k * DAY), tz));
  const hits = [];
  for (const o of offsets) {
    const t = L - o;
    if (wallMs(zoneFields(new Date(t), tz)) === L) hits.push(t);
  }
  if (hits.length) return new Date(Math.min(...hits));
  // Gap: the offset before it is the one a day earlier (transitions are never
  // closer together than that).
  return new Date(L - tzOffsetMs(new Date(L - DAY), tz));
}

// isFoldSecond reports whether an instant is the second occurrence of a
// repeated wall time in tz — the one local fields alone cannot name.
export function isFoldSecond(date, tz) {
  return +zonedToInstant(zoneFields(date, tz), tz) !== +date;
}

// shiftWallDays moves an instant by whole calendar days of tz's wall clock,
// keeping its local time (one day across a DST change is 23 or 25 hours).
export function shiftWallDays(date, days, tz) {
  if (!days) return new Date(date);
  const f = zoneFields(date, tz);
  return zonedToInstant(fieldsFromWallMs(wallMs(f) + days * DAY), tz);
}

// weekdayIn is the day of week (0 = Sunday) of an instant in tz.
export function weekdayIn(date, tz) {
  const f = zoneFields(date, tz);
  return new Date(Date.UTC(f.y, f.mo - 1, f.d)).getUTCDay();
}

// dateValueIn is the YYYY-MM-DD calendar date of an instant in tz (a date
// input value); fieldsFromInputs combines a date input value and {h, m} into
// wall-clock fields.
export function dateValueIn(date, tz) {
  const f = zoneFields(date, tz);
  return `${f.y}-${p2(f.mo)}-${p2(f.d)}`;
}
export function fieldsFromInputs(dateValue, t) {
  const [y, mo, d] = dateValue.split('-').map(Number);
  return { y, mo, d, h: t ? t.h : 0, mi: t ? t.m : 0, s: 0 };
}

// formatHM renders an hour/minute pair like formatTimeOfDay.
export function formatHM(h, m, hour12) {
  if (!hour12) return `${p2(h)}:${p2(m)}`;
  return `${(h % 12) || 12}:${p2(m)} ${h < 12 ? 'AM' : 'PM'}`;
}
