// ical.js — just enough iCalendar/vCard to round-trip the fields the UI
// edits. The server does the real validation; malformed bodies surface as
// 4xx in the raw drawer, which is itself a useful demonstration.

import { DAY, localDate, localDT, zoneFields, zonedToInstant, isKnownZone, isFoldSecond,
  wallMs, fieldsFromWallMs, tzOffsetMs } from './time.js';

export function icalEscape(v) {
  return String(v).replace(/\\/g, '\\\\').replace(/;/g, '\\;')
    .replace(/,/g, '\\,').replace(/\r?\n/g, '\\n');
}

// utc converts a datetime-local input value to iCal basic UTC form.
export function utc(dtLocal) {
  const d = new Date(dtLocal);
  return d.toISOString().replace(/[-:]/g, '').replace(/\.\d{3}Z$/, 'Z');
}

export function newUID() {
  return crypto.randomUUID();
}

function stamp() { return utc(new Date().toISOString()); }

// dateOnly converts a date input value (YYYY-MM-DD) to iCal DATE form.
export function dateOnly(v) { return v.replaceAll('-', ''); }

// eventLines renders the VEVENT body lines (no BEGIN/END wrapper), shared by
// buildEvent and the calweb override editor. attendees: email strings or
// {email, partstat, cn} objects. tz is the zone timed DTSTART/DTEND are
// written in (see dtLine); undefined keeps the legacy UTC form, which the
// protocol demo app relies on. recurrenceIdLine, when given, is a complete
// RECURRENCE-ID line (see occurrenceLine) and wins over recurrenceId.
export function eventLines({ uid, summary, start, end, allDay, rrule, exdates = [],
  organizer, attendees = [], description, location, url, recurrenceId, recurrenceIdLine,
  confidential, alarms, tz }) {
  const dt = (name, v) => allDay ? `${name};VALUE=DATE:${dateOnly(v)}`
    : tz === undefined ? `${name}:${utc(v)}`
    : dtLine(name, new Date(v), tz);
  const l = [
    `UID:${uid}`,
    `DTSTAMP:${stamp()}`,
    dt('DTSTART', start),
    dt('DTEND', end),
    `SUMMARY:${icalEscape(summary)}`,
  ];
  if (recurrenceIdLine) l.push(recurrenceIdLine);
  else if (recurrenceId) {
    // A date-only RECURRENCE-ID (all-day series) must carry VALUE=DATE, or the
    // server reads 20260726 as a truncated DATE-TIME and the override misses.
    l.push(/^\d{8}$/.test(recurrenceId)
      ? `RECURRENCE-ID;VALUE=DATE:${recurrenceId}`
      : `RECURRENCE-ID:${recurrenceId}`);
  }
  // CLASS:CONFIDENTIAL asks the server to show read-only sharees only that the
  // time is busy, hiding the title and the details below (RFC 5545 §3.8.1.3).
  if (confidential) l.push('CLASS:CONFIDENTIAL');
  if (location) l.push(`LOCATION:${icalEscape(location)}`);
  if (description) l.push(`DESCRIPTION:${icalEscape(description)}`);
  if (url) l.push(`URL:${url}`);
  if (rrule) l.push(`RRULE:${rrule}`);
  for (const ex of exdates) l.push(`EXDATE:${ex}`);
  if (organizer) l.push(`ORGANIZER;CN=${icalEscape(organizer)}:mailto:${organizer}`);
  for (const a of attendees) {
    const at = typeof a === 'string' ? { email: a } : a;
    const partstat = at.partstat || 'NEEDS-ACTION';
    l.push(`ATTENDEE;PARTSTAT=${partstat}${at.room ? ';CUTYPE=ROOM' : ''};RSVP=TRUE:mailto:${at.email}`);
  }
  // alarms is a list of iCal trigger durations relative to the start (e.g.
  // '-PT15M', 'PT0S' for at-start); each becomes one DISPLAY reminder, emitted
  // last so they nest as sub-components after the properties.
  for (const trg of alarms || []) l.push(...alarmLines(trg, summary));
  return l;
}

// The presets each reminder row offers: canonical trigger string → label. The
// reminder control matches a stored trigger against this set to pick a row's
// dropdown value, falling back to its Custom controls. 'PT0S' fires at the start.
export const REMINDER_PRESETS = [
  ['PT0S', 'At time of event'],
  ['-PT5M', '5 minutes before'],
  ['-PT15M', '15 minutes before'],
  ['-PT30M', '30 minutes before'],
  ['-PT1H', '1 hour before'],
  ['-P1D', '1 day before'],
];

// alarmLines renders one DISPLAY VALARM (RFC 5545 §3.6.6 requires ACTION,
// TRIGGER and — for DISPLAY — DESCRIPTION). related anchors the trigger to the
// component's start ('START', events) or end ('END', a task's DUE).
function alarmLines(trigger, summary, related = 'START') {
  return ['BEGIN:VALARM', 'ACTION:DISPLAY',
    `DESCRIPTION:${icalEscape(summary || 'Reminder')}`,
    `TRIGGER;RELATED=${related}:${trigger}`, 'END:VALARM'];
}

// The units the reminder editor offers for a custom offset, largest first so
// triggerToOffset prefers "1 week" over "7 days". Each is [unit, milliseconds].
const OFFSET_UNITS = [['weeks', 604800000], ['days', 86400000], ['hours', 3600000], ['minutes', 60000]];

// offsetToTrigger builds an iCal trigger duration from the editor's custom
// controls: when 'at' fires at the event's time (PT0S), 'before'/'after' offset
// by amount of the unit. Returns a canonical duration like '-PT15M' or 'P2D'.
export function offsetToTrigger({ when, amount, unit }) {
  if (when === 'at') return 'PT0S';
  const n = Math.max(1, Math.floor(Number(amount) || 0));
  const sign = when === 'after' ? '' : '-';
  const body = unit === 'minutes' ? `PT${n}M`
    : unit === 'hours' ? `PT${n}H`
    : unit === 'weeks' ? `P${n}W`
    : `P${n}D`;
  return sign + body;
}

// triggerToOffset maps a trigger duration back onto {when, amount, unit} for the
// editor, choosing the largest unit that divides evenly. Returns null when the
// value isn't a duration we can model (an absolute DATE-TIME trigger).
export function triggerToOffset(trigger) {
  const ms = parseDuration(trigger);
  if (ms == null) return null;
  if (ms === 0) return { when: 'at', amount: 0, unit: 'minutes' };
  const when = ms < 0 ? 'before' : 'after';
  const abs = Math.abs(ms);
  for (const [unit, per] of OFFSET_UNITS) {
    if (abs % per === 0) return { when, amount: abs / per, unit };
  }
  return { when, amount: Math.round(abs / 60000), unit: 'minutes' };
}

// buildEvent renders a single-VEVENT VCALENDAR.
export function buildEvent(fields) {
  return ['BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//calclient//EN',
    'BEGIN:VEVENT', ...eventLines(fields), 'END:VEVENT', 'END:VCALENDAR']
    .join('\r\n') + '\r\n';
}

// buildTodo renders a single-VTODO VCALENDAR for the tasks view. due is a
// date input value (YYYY-MM-DD, written as VALUE=DATE) or a Date/ISO string
// (written as UTC DATE-TIME); null omits DUE.
export function buildTodo({ uid, summary, due, description, alarms }) {
  const l = [
    'BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//calclient//EN',
    'BEGIN:VTODO',
    `UID:${uid}`,
    `DTSTAMP:${stamp()}`,
    `SUMMARY:${icalEscape(summary)}`,
    'STATUS:NEEDS-ACTION',
  ];
  if (due) {
    l.push(/^\d{4}-\d{2}-\d{2}$/.test(due)
      ? `DUE;VALUE=DATE:${dateOnly(due)}` : `DUE:${utc(due)}`);
  }
  if (description) l.push(`DESCRIPTION:${icalEscape(description)}`);
  // Task reminders anchor to DUE (RELATED=END) — a VTODO carries no DTSTART to
  // hang a start-relative trigger on.
  for (const trg of alarms || []) l.push(...alarmLines(trg, summary, 'END'));
  l.push('END:VTODO', 'END:VCALENDAR');
  return l.join('\r\n') + '\r\n';
}

// ---- text-level edits on stored objects ---------------------------------
// These operate on unfolded lines so stored properties the client does not
// model (VALARM, X-props, ...) survive untouched.

export function unfold(text) {
  return text.replace(/\r\n[ \t]/g, '').replace(/\n[ \t]/g, '').split(/\r?\n/).filter(Boolean);
}
export function fold(lines) { return lines.join('\r\n') + '\r\n'; }

// foldLong wraps a single content line to ≤75 octets (RFC 5545 §3.1). Naive on
// JS code units, which is fine for the BMP text this client emits.
export function foldLong(line) {
  if (line.length <= 75) return line;
  let out = line.slice(0, 75);
  for (let i = 75; i < line.length; i += 74) out += '\r\n ' + line.slice(i, i + 74);
  return out;
}

// parseICalDate parses a basic iCal DATE or DATE-TIME value into
// {allDay, date, tzid}, or null if it doesn't parse. A trailing Z parses as
// UTC; a TZID Intl knows resolves in that zone (gaps and folds per RFC 5545,
// see zonedToInstant); anything else — floating time, an Outlook-style zone
// name — as device-local time. This module owns the wire-format grammar —
// callers that want other shapes (input values, instants) derive from it.
export function parseICalDate(v, params = {}) {
  if (params.VALUE === 'DATE' || /^\d{8}$/.test(v)) {
    return { allDay: true, date: new Date(+v.slice(0, 4), +v.slice(4, 6) - 1, +v.slice(6, 8)) };
  }
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})(Z?)$/.exec(v);
  if (!m) return null;
  if (!m[7] && isKnownZone(params.TZID)) {
    return { allDay: false, date: zonedToInstant(parseWall(v), params.TZID), tzid: params.TZID };
  }
  return { allDay: false, date: new Date(`${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}${m[7]}`) };
}

// ---- zoned DATE-TIME values ------------------------------------------------
// calweb writes timed events as wall-clock time in an IANA zone so recurring
// series keep their local hour across DST (docs/dst-recurring-events.md).

const p2 = n => String(n).padStart(2, '0');

// formatWall/parseWall convert wall-clock fields to and from the basic
// DATE-TIME form without a Z (20260313T090000).
function formatWall(f) {
  return `${String(f.y).padStart(4, '0')}${p2(f.mo)}${p2(f.d)}T${p2(f.h)}${p2(f.mi)}${p2(f.s || 0)}`;
}
function parseWall(v) {
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})$/.exec(v || '');
  return m ? { y: +m[1], mo: +m[2], d: +m[3], h: +m[4], mi: +m[5], s: +m[6] } : null;
}

// zonedParts is the wall-clock value of an instant in tz (20260313T090000).
export function zonedParts(date, tz) { return formatWall(zoneFields(date, tz)); }

function isUTCZone(tz) { return !tz || tz === 'UTC' || tz === 'Etc/UTC'; }

// dtLine renders a timed date-time property: NAME;TZID=<tz>:<wall> in a zone
// Intl knows, else NAME:<utc>Z. The second occurrence of a repeated local
// time can't be named by wall-clock fields (they resolve to the first), so
// that instant keeps the UTC form rather than silently moving an hour.
export function dtLine(name, date, tz) {
  if (isUTCZone(tz) || !isKnownZone(tz) || isFoldSecond(date, tz)) return `${name}:${utc(date)}`;
  return `${name};TZID=${tz}:${zonedParts(date, tz)}`;
}

// seriesZone is the zone a stored DTSTART's recurrences are computed in:
// its TZID when Intl knows it, 'UTC' for a Z value, and undefined (device
// time) for floating or DATE values.
export function seriesZone(ds) {
  if (!ds) return undefined;
  if (/Z$/.test(ds.value)) return 'UTC';
  if (ds.params?.VALUE === 'DATE' || /^\d{8}$/.test(ds.value)) return undefined;
  return isKnownZone(ds.params?.TZID) ? ds.params.TZID : undefined;
}

// occurrenceLine renders a RECURRENCE-ID or EXDATE naming the occurrence that
// starts at `date`, in the same form as the master's DTSTART (ds): Apple and
// Google compare the value form, not just the instant. allDay series use
// VALUE=DATE; a TZID master gets its zone's wall time; anything else UTC.
export function occurrenceLine(name, date, allDay, ds) {
  if (allDay) return `${name};VALUE=DATE:${dateOnly(localDate(date))}`;
  const tz = ds?.params?.TZID;
  if (!/Z$/.test(ds?.value || '') && isKnownZone(tz) && !isFoldSecond(date, tz)) {
    return `${name};TZID=${tz}:${zonedParts(date, tz)}`;
  }
  return `${name}:${utc(date)}`;
}

// parseLine splits one unfolded content line into {name, params, value}.
function parseLine(line) {
  const idx = line.indexOf(':');
  if (idx < 0) return null;
  const [name, ...paramParts] = splitParams(line.slice(0, idx));
  const params = {};
  for (const p of paramParts) {
    const eq = p.indexOf('=');
    if (eq > 0) params[p.slice(0, eq).toUpperCase()] = p.slice(eq + 1).replace(/^"|"$/g, '');
  }
  return { name: name.toUpperCase(), params, value: line.slice(idx + 1) };
}

// lineInstant is the instant (or all-day date) a date-valued content line
// names, as a number for comparison; NaN when it doesn't parse.
function lineInstant(line) {
  const p = parseLine(line);
  const d = p && parseICalDate(p.value.split(',')[0], p.params);
  return d ? +d.date : NaN;
}

// icalToLocal converts a basic DATE-TIME to a datetime-local input value
// ('' for DATE-only or unparseable input).
export function icalToLocal(v) {
  const p = parseICalDate(String(v || ''));
  return p && !p.allDay ? localDT(p.date) : '';
}

// parseDuration converts an iCal DURATION (RFC 5545 §3.3.6) to milliseconds,
// or null if it can't be parsed.
export function parseDuration(v) {
  const m = /^([+-]?)P(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$/i.exec(String(v).trim());
  if (!m) return null;
  const [w, d, h, mi, s] = [m[2], m[3], m[4], m[5], m[6]].map(x => +(x || 0));
  const secs = (((w * 7 + d) * 24 + h) * 60 + mi) * 60 + s;
  return (m[1] === '-' ? -1 : 1) * secs * 1000;
}

// masterComponentRange finds [begin, end] line indexes (inclusive) of the
// first component of the given type without a RECURRENCE-ID (the recurrence
// master / the plain object).
function masterComponentRange(lines, comp) {
  const begin_ = new RegExp(`^BEGIN:${comp}$`, 'i');
  const end_ = new RegExp(`^END:${comp}$`, 'i');
  let begin = -1;
  for (let i = 0; i < lines.length; i++) {
    if (begin_.test(lines[i])) begin = i;
    else if (end_.test(lines[i]) && begin >= 0) {
      const block = lines.slice(begin, i + 1);
      if (!block.some(l => /^RECURRENCE-ID/i.test(l))) return [begin, i];
      begin = -1;
    }
  }
  return null;
}

// setPartstat rewrites the PARTSTAT of the ATTENDEE matching email (every
// VEVENT in the stream) and strips any iTIP METHOD line, so an inbox
// REQUEST becomes storable calendar data.
export function setPartstat(text, email, partstat) {
  const needle = 'mailto:' + email.toLowerCase();
  const lines = unfold(text).filter(l => !/^METHOD[:;]/i.test(l)).map(l => {
    if (!/^ATTENDEE/i.test(l) || !l.toLowerCase().includes(needle)) return l;
    if (/PARTSTAT=[^;:]*/i.test(l)) return l.replace(/PARTSTAT=[^;:]*/i, 'PARTSTAT=' + partstat);
    return l.replace(/^ATTENDEE/i, 'ATTENDEE;PARTSTAT=' + partstat);
  });
  return fold(lines);
}

// forceSendAttendees adds SCHEDULE-FORCE-SEND=REQUEST to the ATTENDEE lines
// matching the given emails, so the server re-sends their invitation even when
// nothing significant changed (RFC 6638 §7.1) — the way a client resends a
// lost invitation. Idempotent: an ATTENDEE that already has the param is left
// alone, and the server strips it after honoring it.
export function forceSendAttendees(text, emails) {
  const needles = emails.map(e => 'mailto:' + e.toLowerCase());
  const lines = unfold(text).map(l => {
    if (!/^ATTENDEE/i.test(l)) return l;
    const low = l.toLowerCase();
    if (!needles.some(n => low.includes(n)) || /SCHEDULE-FORCE-SEND=/i.test(l)) return l;
    return l.replace(/^ATTENDEE/i, 'ATTENDEE;SCHEDULE-FORCE-SEND=REQUEST');
  });
  return fold(lines);
}

// addExdate excludes one occurrence (dtValue in the same form as DTSTART)
// from the master VEVENT.
export function addExdate(text, dtValue, allDay) {
  return addExdateLine(text, allDay ? `EXDATE;VALUE=DATE:${dtValue}` : `EXDATE:${dtValue}`);
}

// addExdateLine inserts a complete EXDATE line (see occurrenceLine) into the
// master VEVENT.
export function addExdateLine(text, line) {
  const lines = unfold(text);
  const range = masterComponentRange(lines, 'VEVENT');
  if (!range) return text;
  lines.splice(range[1], 0, line);
  return fold(lines);
}

// addOverride appends an override VEVENT (RECURRENCE-ID set by the caller
// via eventLines) before END:VCALENDAR, replacing an existing override for
// the same RECURRENCE-ID if present.
export function addOverride(text, overrideEventLines) {
  let lines = unfold(text);
  const rid = overrideEventLines.find(l => /^RECURRENCE-ID/i.test(l));
  if (rid) {
    // drop an existing override for the same occurrence — compared by instant,
    // since a stored override may name it in another form (UTC vs TZID)
    const at = lineInstant(rid);
    for (let i = 0; i < lines.length; i++) {
      if (/^BEGIN:VEVENT/i.test(lines[i])) {
        const end = lines.findIndex((l, j) => j > i && /^END:VEVENT/i.test(l));
        if (end > i && lines.slice(i, end).some(l => /^RECURRENCE-ID/i.test(l) &&
          (l.toUpperCase() === rid.toUpperCase() || lineInstant(l) === at))) {
          lines = [...lines.slice(0, i), ...lines.slice(end + 1)];
          break;
        }
      }
    }
  }
  const at = lines.findIndex(l => /^END:VCALENDAR/i.test(l));
  lines.splice(at < 0 ? lines.length : at, 0, 'BEGIN:VEVENT', ...overrideEventLines, 'END:VEVENT');
  return fold(lines);
}

// setMasterProps replaces, inserts, or removes top-level properties on the
// master component (the first `comp` — VEVENT by default, VTODO for tasks —
// without a RECURRENCE-ID) while leaving everything else — nested VALARMs,
// sibling components, EXDATEs, and properties this client doesn't model —
// untouched. `props` maps an upper-case NAME to the full iCal line to emit
// (params included), an array of lines (for multi-valued props like ATTENDEE),
// or null to delete the property. Absent props are appended before the
// component's END line.
export function setMasterProps(text, props, comp = 'VEVENT') {
  const lines = unfold(text);
  const range = masterComponentRange(lines, comp);
  if (!range) return text;
  const [b, e] = range;
  const want = new Map(Object.entries(props).map(([k, v]) => [k.toUpperCase(), v]));
  const seen = new Set();
  const push = (arr, val) => {
    if (val != null) for (const x of Array.isArray(val) ? val : [val]) arr.push(x);
  };
  const body = [];
  let depth = 0;
  for (let i = b; i <= e; i++) {
    const line = lines[i];
    if (/^BEGIN:/i.test(line)) { depth++; body.push(line); continue; }
    if (/^END:/i.test(line)) {
      if (--depth === 0) { // closing the master component: flush new properties
        for (const [name, val] of want) if (!seen.has(name)) push(body, val);
      }
      body.push(line);
      continue;
    }
    // Only rewrite the component's direct properties (depth 1); depth 2 is
    // inside a nested VALARM, whose DESCRIPTION/etc must stay untouched.
    if (depth === 1) {
      const name = (/^([^;:]+)/.exec(line)?.[1] || '').toUpperCase();
      if (want.has(name)) {
        if (!seen.has(name)) { seen.add(name); push(body, want.get(name)); }
        continue; // replace on first hit, drop later duplicates / removals
      }
    }
    body.push(line);
  }
  return fold([...lines.slice(0, b), ...body, ...lines.slice(e + 1)]);
}

// setMasterAlarms replaces the reminders on the master component (VEVENT or
// VTODO): it drops every existing VALARM sub-component and re-emits one DISPLAY
// alarm per trigger in `triggers`, before the master's END. An empty list just
// clears the alarms. related anchors the new triggers ('START' for events,
// 'END' for a task's DUE). Only call this once the user has actually chosen the
// reminders in the editor — leaving alarms the editor doesn't model untouched is
// setMasterProps's job, not this one's.
export function setMasterAlarms(text, triggers, description = '', comp = 'VEVENT', related = 'START') {
  const lines = unfold(text);
  const range = masterComponentRange(lines, comp);
  if (!range) return text;
  const [b, e] = range;
  const body = [];
  let depth = 0, inAlarm = false;
  for (let i = b; i <= e; i++) {
    const line = lines[i];
    if (depth === 1 && /^BEGIN:VALARM$/i.test(line)) { inAlarm = true; continue; }
    if (inAlarm) { if (/^END:VALARM$/i.test(line)) inAlarm = false; continue; }
    if (/^BEGIN:/i.test(line)) { depth++; body.push(line); continue; }
    if (/^END:/i.test(line)) {
      if (--depth === 0) for (const trg of triggers) body.push(...alarmLines(trg, description, related));
      body.push(line);
      continue;
    }
    body.push(line);
  }
  return fold([...lines.slice(0, b), ...body, ...lines.slice(e + 1)]);
}

// readReminders maps a component's VALARMs onto the editor's model: a list of
// trigger strings when every alarm is a plain relative DISPLAY reminder we can
// reproduce, or {custom:true} for anything richer (a non-DISPLAY action, an
// absolute trigger, a RELATED that doesn't match) so the editor can keep the
// whole set as is. related is the anchor the reminders are expected to use
// ('START' for events, 'END' for a task's DUE). No alarms → an empty list.
export function readReminders(component, related = 'START') {
  const alarms = (component.children || []).filter(c => c.type === 'VALARM');
  const triggers = [];
  for (const a of alarms) {
    const trig = prop(a, 'TRIGGER');
    if (propValue(a, 'ACTION').toUpperCase() !== 'DISPLAY' || !trig) return { triggers: [], custom: true };
    if ((trig.params.VALUE || '').toUpperCase() === 'DATE-TIME') return { triggers: [], custom: true };
    if ((trig.params.RELATED || 'START').toUpperCase() !== related) return { triggers: [], custom: true };
    if (parseDuration(trig.value) == null) return { triggers: [], custom: true };
    triggers.push(trig.value);
  }
  return { triggers, custom: false };
}

// shiftDate rewrites a DTSTART/DTEND property for a move of the occurrence
// that started at `from` to `to` (both instants), or returns null for forms
// it can't shift safely (floating time, a TZID Intl doesn't know) so the
// caller reports it rather than corrupting the stored value. Each form moves
// by its own kind of delta (docs/dst-recurring-events.md §3):
//   - DATE values by calendar days (from/to read as device-local dates);
//   - UTC values by elapsed time;
//   - TZID values by wall-clock time in the value's OWN zone: from/to are
//     projected into that zone and their wall difference is added to the
//     stored wall time. Moving Zurich 09:00 on 28 March to 09:00 on 29 March
//     is 23 elapsed hours but one wall day, and must stay one day when applied
//     to a master whose first occurrence is in another season.
export function shiftDate(name, prop, { from, to }) {
  const { value, params = {} } = prop;
  if (params.VALUE === 'DATE' || /^\d{8}$/.test(value)) {
    const day = d => Date.UTC(d.getFullYear(), d.getMonth(), d.getDate());
    const days = Math.round((day(to) - day(from)) / DAY);
    const d = new Date(+value.slice(0, 4), +value.slice(4, 6) - 1, +value.slice(6, 8) + days);
    return `${name};VALUE=DATE:${dateOnly(localDate(d))}`;
  }
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$/.exec(value);
  if (m) {
    const d = new Date(Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6]) + (+to - +from));
    return `${name}:${utc(d)}`;
  }
  const tz = params.TZID;
  const wall = parseWall(value);
  if (!tz || !wall || !isKnownZone(tz)) return null;
  const delta = wallMs(zoneFields(to, tz)) - wallMs(zoneFields(from, tz));
  const moved = zonedToInstant(fieldsFromWallMs(wallMs(wall) + delta), tz);
  return `${name};TZID=${tz}:${zonedParts(moved, tz)}`;
}

// icalUnescape reverses icalEscape for display.
export function icalUnescape(v) {
  return String(v).replace(/\\n/gi, '\n').replace(/\\([\\;,])/g, '$1');
}

// ---- RRULE presets --------------------------------------------------------
// The event editor offers only simple repeats (a frequency plus an optional
// end); anything richer is kept verbatim as a "custom" rule.

// parseRRulePreset maps a simple RRULE onto the preset controls
// {freq, ends, count, until}; returns null if the rule is too complex to
// represent, so the editor can preserve it untouched.
export function parseRRulePreset(rrule) {
  if (!rrule) return { freq: '', interval: 1, byday: [], ends: 'never' };
  const parts = Object.fromEntries(rrule.split(';').map(kv => kv.split('=')));
  const known = ['FREQ', 'COUNT', 'UNTIL', 'INTERVAL', 'BYDAY'];
  if (Object.keys(parts).some(k => !known.includes(k))) return null;
  if (!['DAILY', 'WEEKLY', 'MONTHLY', 'YEARLY'].includes(parts.FREQ)) return null;
  // BYDAY is offered only for weekly repeats, and only as plain weekday codes —
  // positional forms like "2MO" stay a "custom" rule the UI leaves untouched.
  const byday = parts.BYDAY ? parts.BYDAY.split(',') : [];
  if (byday.length && (parts.FREQ !== 'WEEKLY' || byday.some(d => !/^(SU|MO|TU|WE|TH|FR|SA)$/.test(d)))) return null;
  return {
    freq: parts.FREQ,
    interval: Math.max(1, parseInt(parts.INTERVAL || '1', 10)),
    byday,
    ends: parts.COUNT ? 'count' : parts.UNTIL ? 'until' : 'never',
    count: parts.COUNT || '10',
    until: parts.UNTIL ? `${parts.UNTIL.slice(0, 4)}-${parts.UNTIL.slice(4, 6)}-${parts.UNTIL.slice(6, 8)}` : '',
  };
}

// buildRRule renders the preset controls back into an RRULE ('' = no repeat).
export function buildRRule(d) {
  if (!d.freq) return '';
  let r = 'FREQ=' + d.freq;
  const interval = Math.max(1, parseInt(d.interval || '1', 10));
  if (interval > 1) r += ';INTERVAL=' + interval;
  if (d.freq === 'WEEKLY' && d.byday?.length) r += ';BYDAY=' + d.byday.join(',');
  if (d.ends === 'count') r += ';COUNT=' + Math.max(1, parseInt(d.count || '1', 10));
  if (d.ends === 'until' && d.until) r += ';UNTIL=' + dateOnly(d.until) + 'T235959Z';
  return r;
}

// ---- recurrence split ("this and following") ------------------------------
// The client splits a series into two plain VEVENTs rather than lean on
// RANGE=THISANDFUTURE: the server's expander strips RRULE per occurrence and
// does not reproduce that range, and a split is what other clients do and every
// client understands. These helpers are pure so the date math is unit-tested.

// rruleParts parses "FREQ=WEEKLY;INTERVAL=2;COUNT=10" into {FREQ, INTERVAL, ...}.
export function rruleParts(rrule) {
  const out = {};
  for (const kv of (rrule || '').split(';')) {
    if (!kv) continue;
    const i = kv.indexOf('=');
    if (i > 0) out[kv.slice(0, i).toUpperCase()] = kv.slice(i + 1);
  }
  return out;
}

// renderRRule renders parts back to a string, FREQ first then a stable order.
function renderRRule(parts) {
  const order = ['FREQ', 'INTERVAL', 'WKST', 'BYDAY', 'BYMONTHDAY', 'BYMONTH', 'BYSETPOS', 'COUNT', 'UNTIL'];
  const keys = [...order.filter(k => k in parts), ...Object.keys(parts).filter(k => !order.includes(k))];
  return keys.filter(k => parts[k] != null && parts[k] !== '').map(k => `${k}=${parts[k]}`).join(';');
}

// canSplitSeries reports whether a series can be split at an arbitrary
// occurrence exactly: only plain FREQ(+INTERVAL)(+COUNT/UNTIL) rules the stepper
// below reproduces. Anything with BY* parts, or monthly/yearly on a day past the
// 28th (real expansion skips short months), is left un-splittable so the editor
// hides the "this and following" option rather than corrupt the series.
//
// tz is the zone the series recurs in (seriesZone of its DTSTART): steps are
// taken on that zone's wall clock, as the server's expander does. Omitted, the
// device's local time is used (DATE series, floating time).
export function canSplitSeries(dtstart, rrule, tz) {
  const p = rruleParts(rrule);
  if (!['DAILY', 'WEEKLY', 'MONTHLY', 'YEARLY'].includes(p.FREQ)) return false;
  for (const k of ['BYDAY', 'BYMONTHDAY', 'BYMONTH', 'BYSETPOS', 'BYWEEKNO', 'BYYEARDAY', 'BYHOUR']) {
    if (p[k]) return false;
  }
  const dayOfMonth = tz ? zoneFields(dtstart, tz).d : dtstart.getDate();
  if ((p.FREQ === 'MONTHLY' || p.FREQ === 'YEARLY') && dayOfMonth > 28) return false;
  return true;
}

// stepOccurrence advances a Date by one recurrence step, on tz's wall clock
// when given (so a weekly 09:00 stays 09:00 across DST), else device-local.
function stepOccurrence(d, freq, interval, tz) {
  const step = (n, get, set) => {
    if (freq === 'DAILY') n[set.date](n[get.date]() + interval);
    else if (freq === 'WEEKLY') n[set.date](n[get.date]() + 7 * interval);
    else if (freq === 'MONTHLY') n[set.month](n[get.month]() + interval);
    else if (freq === 'YEARLY') n[set.year](n[get.year]() + interval);
    return n;
  };
  if (!tz) {
    return step(new Date(d), { date: 'getDate', month: 'getMonth', year: 'getFullYear' },
      { date: 'setDate', month: 'setMonth', year: 'setFullYear' });
  }
  const w = step(new Date(wallMs(zoneFields(d, tz))),
    { date: 'getUTCDate', month: 'getUTCMonth', year: 'getUTCFullYear' },
    { date: 'setUTCDate', month: 'setUTCMonth', year: 'setUTCFullYear' });
  return zonedToInstant(fieldsFromWallMs(+w), tz);
}

// occurrencesBefore counts the occurrences of a splittable series whose start is
// strictly before `cutoff` (a Date). Returns null when the rule isn't splittable.
export function occurrencesBefore(dtstart, rrule, cutoff, tz) {
  if (!canSplitSeries(dtstart, rrule, tz)) return null;
  const p = rruleParts(rrule);
  const interval = Math.max(1, parseInt(p.INTERVAL || '1', 10));
  const total = p.COUNT ? parseInt(p.COUNT, 10) : Infinity;
  let n = 0, occ = new Date(dtstart);
  while (occ < cutoff && n < total && n < 10000) { n++; occ = stepOccurrence(occ, p.FREQ, interval, tz); }
  return n;
}

// nextOccurrence returns the first occurrence of a series at or after `from` (a
// Date), respecting COUNT and UNTIL. Returns null when the rule isn't a plain
// steppable FREQ(+INTERVAL) rule (canSplitSeries) — callers fall back to the
// master's start. If the series ends before `from`, its last real occurrence is
// returned (still in the past, which is correct). Bounded so a rule can't spin.
export function nextOccurrence(dtstart, rrule, from, tz) {
  if (!canSplitSeries(dtstart, rrule, tz)) return null;
  const p = rruleParts(rrule);
  const interval = Math.max(1, parseInt(p.INTERVAL || '1', 10));
  const total = p.COUNT ? parseInt(p.COUNT, 10) : Infinity;
  const until = p.UNTIL ? parseICalDate(p.UNTIL)?.date : null;
  let occ = new Date(dtstart);
  for (let n = 1; occ < from && n < total && n < 10000; n++) {
    const next = stepOccurrence(occ, p.FREQ, interval, tz);
    if (until && next > until) break;   // series ended before `from`
    occ = next;
  }
  return occ;
}

// rruleWithUntil truncates a series to end before a split: drops COUNT and sets
// UNTIL to `until` (an iCal DATE or DATE-TIME value). Backs delete "this and
// following".
export function rruleWithUntil(rrule, until) {
  const p = rruleParts(rrule);
  delete p.COUNT;
  p.UNTIL = until;
  return renderRRule(p);
}

// splitSeriesRRules returns {head, tail, past} for a "this and following" edit:
// `head` truncates the original series just before the split (UNTIL=headUntil),
// `tail` is the rule for the new forward series (its COUNT reduced by the
// occurrences the head keeps), and `past` is that kept count. Returns null when
// the rule can't be split exactly.
export function splitSeriesRRules(dtstart, rrule, cutoff, headUntil, tz) {
  const past = occurrencesBefore(dtstart, rrule, cutoff, tz);
  if (past == null) return null;
  const p = rruleParts(rrule);
  const head = { ...p };
  delete head.COUNT;
  head.UNTIL = headUntil;
  const tail = { ...p };
  if (p.COUNT) {
    const rem = parseInt(p.COUNT, 10) - past;
    if (rem < 1) return null;
    tail.COUNT = String(rem);
  }
  return { head: renderRRule(head), tail: renderRRule(tail), past };
}

// dropForward removes override VEVENTs and EXDATEs at or after `cutoff` (a Date)
// from a stored object. Truncating a series with UNTIL alone would leave orphan
// override components after the split, which the server still emits as lone
// instances — this strips them so the removed tail truly disappears.
export function dropForward(text, cutoff) {
  const lines = unfold(text);
  const atOrAfter = (v, params = {}) => { const d = parseICalDate((v || '').split(',')[0].trim(), params); return d && +d.date >= +cutoff; };
  // EXDATEs live inside the master VEVENT, so filter them within a kept block
  // rather than at the top level.
  const filterExdate = block => block.flatMap(l => {
    if (!/^EXDATE/i.test(l)) return [l];
    const m = /^(EXDATE(?:;[^:]*)?):(.*)$/i.exec(l);
    const all = m[2].split(',');
    const params = parseLine(l)?.params || {};
    const keep = all.filter(v => !atOrAfter(v, params));
    return !keep.length ? [] : [keep.length === all.length ? l : `${m[1]}:${keep.join(',')}`];
  });
  const out = [];
  let i = 0;
  while (i < lines.length) {
    if (/^BEGIN:VEVENT$/i.test(lines[i])) {
      let j = i, depth = 0;
      do {
        if (/^BEGIN:/i.test(lines[j])) depth++;
        else if (/^END:/i.test(lines[j])) depth--;
        j++;
      } while (j < lines.length && depth > 0);
      const block = lines.slice(i, j);
      const rid = block.find(l => /^RECURRENCE-ID/i.test(l));
      const ridP = rid && parseLine(rid);
      if (ridP && atOrAfter(ridP.value, ridP.params)) { i = j; continue; } // drop forward override
      out.push(...filterExdate(block));
      i = j;
      continue;
    }
    out.push(lines[i]);
    i++;
  }
  return fold(out);
}

// ---- converting a UTC series to a zone (docs/dst-recurring-events.md §4) ----
//
// A series stored with a UTC DTSTART recurs at one UTC time, so its local
// time moves with every DST change. convertSeriesToZone rewrites the whole
// object so it recurs at a fixed wall time in tz, keeping the FIRST
// occurrence's instant. Everything that names a recurrence slot (the
// overrides' RECURRENCE-IDs, EXDATEs, the RRULE's UNTIL bound) is mapped to
// the corresponding slot of the converted series — slot k of the UTC series is
// at the anchor's UTC time on some date; the same slot of the zoned series is
// at the anchor's wall time on the matching date, i.e. the UTC slot shifted by
// the anchor's offset. Formatting the old instant in the new zone instead
// would detach every exception after a transition (20260403T080000Z must name
// Zurich 09:00, not 10:00). Explicit times the user chose — an override's own
// DTSTART/DTEND, RDATEs — keep their instants.
//
// All or nothing: returns {text} or {error} with a reason for the user, and
// never a partial rewrite.
export function convertSeriesToZone(text, tz) {
  if (isUTCZone(tz) || !isKnownZone(tz)) return { error: 'There is no local time zone to convert to.' };
  const lines = unfold(text);
  const range = masterComponentRange(lines, 'VEVENT');
  if (!range) return { error: 'The event has no series to convert.' };
  const dsLine = topLevel(lines, range).find(i => /^DTSTART[;:]/i.test(lines[i]));
  const ds = dsLine != null && parseLine(lines[dsLine]);
  const rr = topLevel(lines, range).map(i => parseLine(lines[i])).find(p => p?.name === 'RRULE');
  if (!ds || !/Z$/.test(ds.value) || !rr) return { error: 'The series is not stored in UTC.' };
  const s0 = parseICalDate(ds.value, ds.params).date;
  if (isFoldSecond(s0, tz)) {
    return { error: 'The series starts in the hour that repeats when the clocks go back, which a local time cannot name.' };
  }
  const rule = rr.value.toUpperCase();
  if (/(^|;)BY(HOUR|MINUTE|SECOND)=/.test(rule) || /(^|;)FREQ=(HOURLY|MINUTELY|SECONDLY)(;|$)/.test(rule)) {
    return { error: 'The series repeats at several times of day.' };
  }
  const off0 = tzOffsetMs(s0, tz);
  const utcDay = s0.toISOString().slice(0, 10);
  const wallDay = new Date(+s0 + off0).toISOString().slice(0, 10);
  // Slots map by a constant shift, which keeps every date consistent — unless
  // the rule picks dates by calendar fields and the anchor's UTC date isn't
  // its local date. That covers explicit BY* parts, and the month-day and
  // month a MONTHLY/YEARLY rule takes implicitly from DTSTART (a UTC series
  // on the 31st skips short months; converted to the 1st it wouldn't).
  if (utcDay !== wallDay && (/(^|;)BY(DAY|MONTHDAY|YEARDAY|WEEKNO|MONTH|SETPOS)=/.test(rule)
    || /(^|;)FREQ=(MONTHLY|YEARLY)(;|$)/.test(rule))) {
    return { error: 'The series is anchored to UTC days that differ from local days, so its weekdays or dates would change.' };
  }

  const isUtcValue = v => /^\d{8}T\d{6}Z$/.test(v);
  let failure = '';
  const fail = msg => { failure = failure || msg; return null; };
  // RDATE occurrences keep their instants (below), so a RECURRENCE-ID or
  // EXDATE naming one must keep that instant too, or it would detach from
  // its RDATE; only rule slots move.
  const rdates = new Set();
  for (const i of topLevel(lines, range)) {
    const p = parseLine(lines[i]);
    if (p?.name === 'RDATE' && p.params.VALUE !== 'PERIOD') {
      for (const v of p.value.split(',')) if (isUtcValue(v)) rdates.add(+parseICalDate(v, {}).date);
    }
  }
  const refWall = v => {
    const d = parseICalDate(v, {}).date;
    if (!rdates.has(+d)) return formatWall(fieldsFromWallMs(+d + off0));
    if (isFoldSecond(d, tz)) return fail('An added date falls in the hour that repeats when the clocks go back.');
    return zonedParts(d, tz);
  };
  // A slot reference: a UTC value mapped to the zoned series' slot.
  const slotLine = (p, multi) => {
    const vals = multi ? p.value.split(',') : [p.value];
    if (p.params.RANGE) return fail('An exception applies to a range of occurrences.');
    if (!vals.every(isUtcValue)) return fail('An exception is not stored in UTC.');
    const walls = vals.map(refWall);
    if (walls.some(w => w == null)) return null;
    return `${p.name}${paramsString(p.params, tz)}:${walls.join(',')}`;
  };
  // An explicit instant: kept exactly, written as wall time unless it is the
  // second occurrence of a repeated hour (then it stays as stored). A value
  // that isn't UTC (another zone, a DATE) is already what it means and is
  // left exactly as stored — line included.
  const instantLine = (p, multi, line) => {
    const vals = multi ? p.value.split(',') : [p.value];
    if (!vals.every(isUtcValue)) return line;
    const dates = vals.map(v => parseICalDate(v, {}).date);
    if (dates.some(d => isFoldSecond(d, tz))) {
      if (multi) return fail('A date in the series falls in the hour that repeats when the clocks go back.');
      return line;
    }
    return `${p.name}${paramsString(p.params, tz)}:${dates.map(d => zonedParts(d, tz)).join(',')}`;
  };
  const untilLine = p => {
    const parts = rruleParts(p.value);
    if (parts.UNTIL && isUtcValue(parts.UNTIL)) {
      const wall = formatWall(fieldsFromWallMs(+parseICalDate(parts.UNTIL, {}).date + off0));
      parts.UNTIL = utc(zonedToInstant(parseWall(wall), tz));
    }
    return `RRULE:${renderRRule(parts)}`;
  };

  const out = [];
  let depth = 0, inEvent = false, isMaster = false;
  for (let i = 0; i < lines.length; i++) {
    const l = lines[i];
    if (/^BEGIN:/i.test(l)) {
      depth++;
      if (/^BEGIN:VEVENT$/i.test(l)) { inEvent = true; isMaster = i === range[0]; }
      out.push(l); continue;
    }
    if (/^END:/i.test(l)) { depth--; if (/^END:VEVENT$/i.test(l)) inEvent = false; out.push(l); continue; }
    // Only a VEVENT's own properties (depth 2), never a nested VALARM's.
    const p = inEvent && depth === 2 ? parseLine(l) : null;
    if (!p) { out.push(l); continue; }
    let next = l;
    if (p.name === 'DTSTART' && isMaster) next = `DTSTART${paramsString(p.params, tz)}:${zonedParts(s0, tz)}`;
    else if (p.name === 'DTSTART' || p.name === 'DTEND') next = instantLine(p, false, l);
    else if (p.name === 'RECURRENCE-ID') next = slotLine(p, false);
    else if (p.name === 'EXDATE') next = slotLine(p, true);
    else if (p.name === 'RDATE') next = p.params.VALUE === 'PERIOD' ? fail('The series has extra dates given as periods.') : instantLine(p, true, l);
    else if (p.name === 'RRULE' && isMaster) next = untilLine(p);
    if (next == null) break;
    out.push(next);
  }
  if (failure) return { error: failure };
  return { text: fold(out) };
}

// convertedSlot is where an occurrence of a UTC series starting at `first`
// (at instant `date`) lands once the series is converted to tz: the same
// slot, at the first occurrence's wall time (see convertSeriesToZone).
export function convertedSlot(date, first, tz) {
  return zonedToInstant(fieldsFromWallMs(+date + tzOffsetMs(first, tz)), tz);
}

// convertAndMove converts a UTC series to tz (convertSeriesToZone) and then
// applies an "all events" time edit as a TARGET on the converted series: the
// user opened an occurrence that, once converted, is at `shown` ({start,
// end}: its converted slot — see convertedSlot — or an override's own
// times, which conversion keeps) and asked for it to be at `target`. The
// series moves by the wall-clock difference between the two, so what the
// user typed is where the occurrence, and with it the series, lands.
// (Shifting the UTC master by the elapsed difference first would, for a
// winter-anchored series, turn a corrected summer 10:00 → 09:00 into 08:00.)
// With no time change (target equals shown) this is plain conversion, which
// keeps the first occurrence's local time. Returns {text} or {error}.
export function convertAndMove(text, tz, shown, target) {
  const r = convertSeriesToZone(text, tz);
  if (r.error || !target || (+target.start === +shown.start && +target.end === +shown.end)) return r;
  const lines = unfold(r.text);
  const range = masterComponentRange(lines, 'VEVENT');
  const own = topLevel(lines, range).map(i => parseLine(lines[i]));
  const ds = own.find(p => p?.name === 'DTSTART');
  const de = own.find(p => p?.name === 'DTEND');
  const props = {};
  if (+target.start !== +shown.start) {
    props.DTSTART = shiftDate('DTSTART', ds, { from: shown.start, to: target.start });
  }
  if (de && +target.end !== +shown.end) {
    props.DTEND = shiftDate('DTEND', de, { from: shown.end, to: target.end });
  }
  if (Object.values(props).some(v => v == null)) return { error: 'The series’ times could not be moved.' };
  return { text: setMasterProps(r.text, props) };
}

// topLevel lists the line indexes of a component's own properties (not those
// of nested components) within [range[0], range[1]].
function topLevel(lines, range) {
  const out = [];
  let depth = 0;
  for (let i = range[0]; i <= range[1]; i++) {
    if (/^BEGIN:/i.test(lines[i])) { depth++; continue; }
    if (/^END:/i.test(lines[i])) { depth--; continue; }
    if (depth === 1) out.push(i);
  }
  return out;
}

// paramsString renders params back (VALUE dropped, since every value written
// here is a DATE-TIME), with TZID set to tz or removed when tz is omitted.
function paramsString(params, tz) {
  const out = [];
  for (const [k, v] of Object.entries(params)) {
    if (k === 'TZID' || k === 'VALUE') continue;
    out.push(`;${k}=${/[;:,]/.test(v) ? `"${v}"` : v}`);
  }
  if (tz) out.unshift(`;TZID=${tz}`);
  return out.join('');
}

// utcSeriesInfo describes a recurring master stored in UTC, for the editor's
// conversion notice and preview: {calweb, start (the first occurrence),
// rrule, rdates (instants of its UTC RDATEs)} or null when the master isn't
// a UTC series.
export function utcSeriesInfo(text) {
  const master = components(parse(text), 'VEVENT').find(v => !prop(v, 'RECURRENCE-ID'));
  const ds = master && prop(master, 'DTSTART');
  const rrule = master ? propValue(master, 'RRULE') : '';
  if (!ds || !rrule || !/Z$/.test(ds.value)) return null;
  const prodid = /^PRODID:(.*)$/im.exec(text.replace(/\r\n[ \t]/g, ''))?.[1]?.trim() || '';
  const rdates = master.props.filter(p => p.name === 'RDATE' && p.params.VALUE !== 'PERIOD')
    .flatMap(p => p.value.split(',')).filter(v => /Z$/.test(v)).map(v => +parseICalDate(v, {}).date);
  return { calweb: prodid === '-//calclient//EN', start: parseICalDate(ds.value, ds.params).date, rrule, rdates };
}

// convertedRef is where the occurrence originally at `date` is once the UTC
// series `info` is converted to tz: an RDATE occurrence keeps its instant,
// a rule occurrence moves to its converted slot (convertedSlot).
export function convertedRef(date, info, tz) {
  return info.rdates.includes(+date) ? new Date(date) : convertedSlot(date, info.start, tz);
}

// conversionMovesOccurrences reports whether converting a UTC series
// starting at `start` to tz changes when any occurrence happens: only when
// tz's offset differs somewhere in the series' span from its offset at the
// start. Checked daily over the series' first 400 days (bounded by UNTIL):
// a year covers every annual DST cycle, and daily steps catch a change even
// in a series only weeks long.
export function conversionMovesOccurrences(start, rrule, tz) {
  if (isUTCZone(tz) || !isKnownZone(tz)) return false;
  const until = rruleParts(rrule).UNTIL;
  const end = until ? +parseICalDate(until, {})?.date : Infinity;
  const off0 = tzOffsetMs(start, tz);
  for (let k = 1; k <= 400; k++) {
    const t = +start + k * DAY;
    if (t > end + DAY) break;
    if (tzOffsetMs(new Date(t), tz) !== off0) return true;
  }
  return false;
}

// conversionPreview lists up to n occurrences at or after `from` whose time
// changes when the UTC series is converted to tz: [{before, after}]. Returns
// null for rules the stepper can't reproduce (the editor then describes the
// change instead of listing it).
export function conversionPreview(start, rrule, tz, from, n = 5) {
  if (!canSplitSeries(start, rrule, 'UTC')) return null;
  const p = rruleParts(rrule);
  const interval = Math.max(1, parseInt(p.INTERVAL || '1', 10));
  const total = p.COUNT ? parseInt(p.COUNT, 10) : Infinity;
  const until = p.UNTIL ? +parseICalDate(p.UNTIL, {})?.date : Infinity;
  const off0 = tzOffsetMs(start, tz);
  const out = [];
  let occ = new Date(start);
  for (let k = 0; k < total && k < 2000 && +occ <= until && out.length < n; k++) {
    if (+occ >= +from) {
      const after = zonedToInstant(fieldsFromWallMs(+occ + off0), tz);
      if (+after !== +occ) out.push({ before: occ, after });
    }
    occ = stepOccurrence(occ, p.FREQ, interval, 'UTC');
  }
  return out;
}

// buildFreeBusyRequest renders the METHOD:REQUEST VFREEBUSY body an outbox
// POST expects (RFC 6638 §13.1).
export function buildFreeBusyRequest({ organizer, attendees, start, end }) {
  const l = [
    'BEGIN:VCALENDAR',
    'VERSION:2.0',
    'PRODID:-//calclient//EN',
    'METHOD:REQUEST',
    'BEGIN:VFREEBUSY',
    `UID:${newUID()}`,
    `DTSTAMP:${stamp()}`,
    `DTSTART:${utc(start)}`,
    `DTEND:${utc(end)}`,
    `ORGANIZER:mailto:${organizer}`,
  ];
  for (const a of attendees) l.push(`ATTENDEE:mailto:${a}`);
  l.push('END:VFREEBUSY', 'END:VCALENDAR');
  return l.join('\r\n') + '\r\n';
}

// parse unfolds and parses an iCalendar/vCard stream into a component tree:
// {type, props: [{name, params: {K: V}, value}], children: [...]}.
export function parse(text) {
  const lines = text.replace(/\r\n[ \t]/g, '').replace(/\n[ \t]/g, '').split(/\r?\n/);
  const root = { type: 'STREAM', props: [], children: [] };
  const stack = [root];
  for (const line of lines) {
    if (!line) continue;
    const idx = line.indexOf(':');
    if (idx < 0) continue;
    const left = line.slice(0, idx), value = line.slice(idx + 1);
    const [name, ...paramParts] = splitParams(left);
    const uname = name.toUpperCase();
    if (uname === 'BEGIN') {
      const comp = { type: value.toUpperCase(), props: [], children: [] };
      stack[stack.length - 1].children.push(comp);
      stack.push(comp);
    } else if (uname === 'END') {
      if (stack.length > 1) stack.pop();
    } else {
      const params = {};
      for (const p of paramParts) {
        const eq = p.indexOf('=');
        if (eq > 0) params[p.slice(0, eq).toUpperCase()] = p.slice(eq + 1).replace(/^"|"$/g, '');
        // Valueless vCard 2.1 flags (e.g. PHOTO;BASE64:, TEL;HOME;VOICE:) carry
        // meaning in the name alone; keep them as '' so they round-trip.
        else if (eq < 0 && p) params[p.toUpperCase()] = '';
      }
      stack[stack.length - 1].props.push({ name: uname, params, value });
    }
  }
  return root;
}

// splitParams splits NAME;P=V;P="a;b" on semicolons outside quotes.
function splitParams(left) {
  const out = [];
  let cur = '', inQ = false;
  for (const ch of left) {
    if (ch === '"') inQ = !inQ;
    if (ch === ';' && !inQ) { out.push(cur); cur = ''; continue; }
    cur += ch;
  }
  out.push(cur);
  return out;
}

// components collects all components of a type anywhere in a parsed tree.
export function components(node, type, out = []) {
  for (const c of node.children || []) {
    if (c.type === type) out.push(c);
    components(c, type, out);
  }
  return out;
}

export function prop(comp, name) {
  return comp.props.find(p => p.name === name.toUpperCase()) || null;
}
export function propValue(comp, name) {
  return prop(comp, name)?.value ?? '';
}

// vcardValue splits a structured vCard value (N, ADR) on unescaped ';'.
export function vcardValue(v) {
  const out = [];
  let cur = '', esc = false;
  for (const ch of String(v)) {
    if (esc) { cur += ch; esc = false; }
    else if (ch === '\\') { cur += ch; esc = true; }
    else if (ch === ';') { out.push(cur); cur = ''; }
    else cur += ch;
  }
  out.push(cur);
  return out;
}

// vcardLine reconstructs a folded content line from a parsed property, used to
// carry through properties the editor does not model (PHOTO, CATEGORIES, X-*).
export function vcardLine(prop) {
  // A '' value marks a valueless vCard 2.1 flag (BASE64, JPEG, HOME…) — re-emit
  // it bare (`;BASE64`) rather than as an empty-valued `;BASE64=`.
  const params = Object.entries(prop.params)
    .map(([k, v]) => (v === '' ? `;${k}` : `;${k}=${v}`)).join('');
  return foldLong(`${prop.name}${params}:${prop.value}`);
}

// MANAGED_VCARD: properties the editor owns; everything else is preserved.
export const MANAGED_VCARD = new Set(
  ['BEGIN', 'END', 'VERSION', 'UID', 'PRODID', 'FN', 'N', 'ORG', 'TITLE',
    'EMAIL', 'TEL', 'ADR', 'URL', 'BDAY', 'NOTE']);

// photoMime derives an image MIME type from a PHOTO property's params: the vCard
// TYPE (JPEG/PNG/…), or a bare 2.1 subtype flag (PHOTO;JPEG;BASE64:), defaulting
// to image/jpeg. A TYPE already containing '/' is treated as a full MIME.
function photoMime(params = {}) {
  let t = (params.TYPE || '').toLowerCase();
  if (!t) for (const k of Object.keys(params)) if (/^(jpe?g|png|gif|webp|bmp|tiff?|heic)$/i.test(k)) { t = k.toLowerCase(); break; }
  if (!t) return 'image/jpeg';
  if (t.includes('/')) return t;
  return 'image/' + (t === 'jpg' ? 'jpeg' : t);
}

// photoSrc turns a parsed PHOTO property into a value usable as an <img> src: a
// URI photo (VALUE=uri, or an http/https/data value) is returned as-is; an
// embedded base64 photo (3.0 ENCODING=b/BASE64, or a bare 2.1 BASE64 flag)
// becomes a data: URL. Returns '' when there is no usable image.
export function photoSrc(prop) {
  if (!prop?.value) return '';
  const v = prop.value.trim();
  const params = prop.params || {};
  if ((params.VALUE || '').toLowerCase() === 'uri' || /^(https?|data):/i.test(v)) return v;
  return `data:${photoMime(params)};base64,${v.replace(/\s+/g, '')}`;
}

// buildPhotoLine renders an uploaded `data:` URL as a folded vCard 3.0 PHOTO
// line for the store's `preserve` array. Returns '' if it isn't a data: URL.
export function buildPhotoLine(dataUrl) {
  const m = /^data:([^;,]*)(;base64)?,(.*)$/is.exec(dataUrl || '');
  if (!m) return '';
  const sub = ((m[1] || 'image/jpeg').split('/')[1] || 'jpeg').toUpperCase();
  const b64 = m[2] ? m[3] : btoa(decodeURIComponent(m[3]));
  return vcardLine({ name: 'PHOTO', params: { ENCODING: 'b', TYPE: sub }, value: b64 });
}

// isPhotoLine matches a (possibly Apple item-grouped) PHOTO content line, so the
// editor can swap a contact's photo without disturbing the other preserved
// properties.
export function isPhotoLine(line) {
  return /^(?:item\d+\.)?PHOTO[;:]/i.test(line);
}

// buildVCard renders a vCard 3.0 from the editor model. `preserve` is an array
// of pre-rendered content lines (from vcardLine) for unmodeled properties.
// emails/tels: [{value, type}]; adr: {type, street, city, region, postal, country}.
export function buildVCard(m) {
  const e = icalEscape;
  const first = (m.first || '').trim(), last = (m.last || '').trim();
  const fn = (m.fn || `${first} ${last}`).trim() || 'Unnamed';
  const l = ['BEGIN:VCARD', 'VERSION:3.0', `UID:${m.uid}`,
    `FN:${e(fn)}`, `N:${e(last)};${e(first)};;;`];
  if (m.org) l.push(`ORG:${e(m.org)}`);
  if (m.title) l.push(`TITLE:${e(m.title)}`);
  for (const em of m.emails || []) {
    if (!em.value?.trim()) continue;
    l.push(`EMAIL;TYPE=INTERNET${em.type ? ';TYPE=' + em.type : ''}:${em.value.trim()}`);
  }
  for (const t of m.tels || []) {
    if (!t.value?.trim()) continue;
    l.push(`TEL;TYPE=${t.type || 'VOICE'}:${t.value.trim()}`);
  }
  const a = m.adr;
  if (a && (a.street || a.city || a.region || a.postal || a.country)) {
    l.push(`ADR;TYPE=${a.type || 'HOME'}:;;${e(a.street || '')};${e(a.city || '')};${e(a.region || '')};${e(a.postal || '')};${e(a.country || '')}`);
  }
  if (m.url) l.push(`URL:${m.url.trim()}`);
  if (m.bday) l.push(`BDAY:${m.bday.trim()}`);
  if (m.note) l.push(`NOTE:${e(m.note)}`);
  // Preserved lines are already folded; fold the rest (which contain no
  // embedded newlines yet) and leave preserved ones untouched.
  const folded = l.map(foldLong);
  folded.push(...(m.preserve || []));
  folded.push('END:VCARD');
  return folded.join('\r\n') + '\r\n';
}
