// ical.js — just enough iCalendar/vCard to round-trip the fields the UI
// edits. The server does the real validation; malformed bodies surface as
// 4xx in the raw drawer, which is itself a useful demonstration.

import { DAY, localDate, localDT } from './time.js';

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
// {email, partstat, cn} objects.
export function eventLines({ uid, summary, start, end, allDay, rrule, exdates = [],
  organizer, attendees = [], description, location, url, recurrenceId, confidential, alarm }) {
  const dt = allDay
    ? v => `;VALUE=DATE:${dateOnly(v)}`
    : v => `:${utc(v)}`;
  const l = [
    `UID:${uid}`,
    `DTSTAMP:${stamp()}`,
    `DTSTART${dt(start)}`,
    `DTEND${dt(end)}`,
    `SUMMARY:${icalEscape(summary)}`,
  ];
  if (recurrenceId) {
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
  // alarm is an iCal trigger duration relative to the start (e.g. '-PT15M',
  // 'PT0S' for at-start); a single DISPLAY reminder, emitted last so it nests
  // as a sub-component after the properties.
  if (alarm) l.push(...alarmLines(alarm, summary));
  return l;
}

// The presets the reminder control offers, newest-first by how the UI reads:
// canonical trigger string → label. Kept here so alarmPreset can match a stored
// trigger against the same set the editor writes. 'PT0S' fires at the start.
export const REMINDER_PRESETS = [
  ['PT0S', 'At time of event'],
  ['-PT5M', '5 minutes before'],
  ['-PT15M', '15 minutes before'],
  ['-PT30M', '30 minutes before'],
  ['-PT1H', '1 hour before'],
  ['-P1D', '1 day before'],
];

// alarmLines renders one DISPLAY VALARM (RFC 5545 §3.6.6 requires ACTION,
// TRIGGER and — for DISPLAY — DESCRIPTION).
function alarmLines(trigger, summary) {
  return ['BEGIN:VALARM', 'ACTION:DISPLAY',
    `DESCRIPTION:${icalEscape(summary || 'Reminder')}`,
    `TRIGGER;RELATED=START:${trigger}`, 'END:VALARM'];
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
export function buildTodo({ uid, summary, due, description }) {
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
// {allDay, date}, or null if it doesn't parse. A trailing Z parses as UTC; a
// floating value as local time. This module owns the wire-format grammar —
// callers that want other shapes (input values, instants) derive from it.
export function parseICalDate(v, params = {}) {
  if (params.VALUE === 'DATE' || /^\d{8}$/.test(v)) {
    return { allDay: true, date: new Date(+v.slice(0, 4), +v.slice(4, 6) - 1, +v.slice(6, 8)) };
  }
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})(Z?)$/.exec(v);
  if (!m) return null;
  return { allDay: false, date: new Date(`${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}${m[7]}`) };
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
  const lines = unfold(text);
  const range = masterComponentRange(lines, 'VEVENT');
  if (!range) return text;
  const ex = allDay ? `EXDATE;VALUE=DATE:${dtValue}` : `EXDATE:${dtValue}`;
  lines.splice(range[1], 0, ex);
  return fold(lines);
}

// addOverride appends an override VEVENT (RECURRENCE-ID set by the caller
// via eventLines) before END:VCALENDAR, replacing an existing override for
// the same RECURRENCE-ID if present.
export function addOverride(text, overrideEventLines) {
  let lines = unfold(text);
  const rid = overrideEventLines.find(l => /^RECURRENCE-ID/i.test(l));
  if (rid) {
    // drop an existing override with the same RECURRENCE-ID
    for (let i = 0; i < lines.length; i++) {
      if (/^BEGIN:VEVENT/i.test(lines[i])) {
        const end = lines.findIndex((l, j) => j > i && /^END:VEVENT/i.test(l));
        if (end > i && lines.slice(i, end).some(l => l.toUpperCase() === rid.toUpperCase())) {
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

// setMasterAlarm replaces the reminder on the master VEVENT: it drops every
// existing VALARM sub-component and, when `trigger` is a non-empty duration,
// inserts a single DISPLAY alarm before the master's END. An empty trigger just
// clears the alarms. Only call this once the user has actually chosen a reminder
// (or "none") in the editor — leaving alarms the editor doesn't model untouched
// is setMasterProps's job, not this one's.
export function setMasterAlarm(text, trigger, description = '') {
  const lines = unfold(text);
  const range = masterComponentRange(lines, 'VEVENT');
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
      if (--depth === 0 && trigger) body.push(...alarmLines(trigger, description));
      body.push(line);
      continue;
    }
    body.push(line);
  }
  return fold([...lines.slice(0, b), ...body, ...lines.slice(e + 1)]);
}

// alarmPreset maps the master VEVENT's reminder onto a REMINDER_PRESETS trigger
// string for the editor's dropdown: '' when there is no alarm, a preset trigger
// when the single alarm is a plain relative-to-start DISPLAY reminder we can
// reproduce, or 'CUSTOM' for anything richer (multiple alarms, an absolute or
// RELATED=END trigger, a non-DISPLAY action) so the editor can keep it as is.
export function alarmPreset(vevent) {
  const alarms = (vevent.children || []).filter(c => c.type === 'VALARM');
  if (!alarms.length) return '';
  if (alarms.length > 1) return 'CUSTOM';
  const a = alarms[0];
  const trig = prop(a, 'TRIGGER');
  if (propValue(a, 'ACTION').toUpperCase() !== 'DISPLAY' || !trig) return 'CUSTOM';
  if ((trig.params.VALUE || '').toUpperCase() === 'DATE-TIME') return 'CUSTOM';
  if ((trig.params.RELATED || 'START').toUpperCase() !== 'START') return 'CUSTOM';
  const ms = parseDuration(trig.value);
  if (ms == null) return 'CUSTOM';
  const hit = REMINDER_PRESETS.find(([t]) => parseDuration(t) === ms);
  return hit ? hit[0] : 'CUSTOM';
}

// shiftDate rewrites a DTSTART/DTEND line shifted by deltaMs, or returns null
// for forms we can't shift safely (TZID or floating local time) so the caller
// leaves the stored value unchanged rather than corrupting it.
export function shiftDate(name, prop, deltaMs) {
  const { value, params = {} } = prop;
  if (params.TZID) return null;
  if (params.VALUE === 'DATE' || /^\d{8}$/.test(value)) {
    const days = Math.round(deltaMs / DAY);
    const d = new Date(+value.slice(0, 4), +value.slice(4, 6) - 1, +value.slice(6, 8) + days);
    return `${name};VALUE=DATE:${dateOnly(localDate(d))}`;
  }
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$/.exec(value);
  if (m) {
    const d = new Date(Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6]) + deltaMs);
    return `${name}:${utc(d.toISOString())}`;
  }
  return null;
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
export function canSplitSeries(dtstart, rrule) {
  const p = rruleParts(rrule);
  if (!['DAILY', 'WEEKLY', 'MONTHLY', 'YEARLY'].includes(p.FREQ)) return false;
  for (const k of ['BYDAY', 'BYMONTHDAY', 'BYMONTH', 'BYSETPOS', 'BYWEEKNO', 'BYYEARDAY', 'BYHOUR']) {
    if (p[k]) return false;
  }
  if ((p.FREQ === 'MONTHLY' || p.FREQ === 'YEARLY') && dtstart.getDate() > 28) return false;
  return true;
}

// stepOccurrence advances a Date by one recurrence step.
function stepOccurrence(d, freq, interval) {
  const n = new Date(d);
  if (freq === 'DAILY') n.setDate(n.getDate() + interval);
  else if (freq === 'WEEKLY') n.setDate(n.getDate() + 7 * interval);
  else if (freq === 'MONTHLY') n.setMonth(n.getMonth() + interval);
  else if (freq === 'YEARLY') n.setFullYear(n.getFullYear() + interval);
  return n;
}

// occurrencesBefore counts the occurrences of a splittable series whose start is
// strictly before `cutoff` (a Date). Returns null when the rule isn't splittable.
export function occurrencesBefore(dtstart, rrule, cutoff) {
  if (!canSplitSeries(dtstart, rrule)) return null;
  const p = rruleParts(rrule);
  const interval = Math.max(1, parseInt(p.INTERVAL || '1', 10));
  const total = p.COUNT ? parseInt(p.COUNT, 10) : Infinity;
  let n = 0, occ = new Date(dtstart);
  while (occ < cutoff && n < total && n < 10000) { n++; occ = stepOccurrence(occ, p.FREQ, interval); }
  return n;
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
export function splitSeriesRRules(dtstart, rrule, cutoff, headUntil) {
  const past = occurrencesBefore(dtstart, rrule, cutoff);
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
  const atOrAfter = v => { const d = parseICalDate((v || '').split(',')[0].trim(), {}); return d && +d.date >= +cutoff; };
  // EXDATEs live inside the master VEVENT, so filter them within a kept block
  // rather than at the top level.
  const filterExdate = block => block.flatMap(l => {
    if (!/^EXDATE/i.test(l)) return [l];
    const m = /^(EXDATE(?:;[^:]*)?):(.*)$/i.exec(l);
    const all = m[2].split(',');
    const keep = all.filter(v => !atOrAfter(v));
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
      if (rid && atOrAfter(rid.replace(/^RECURRENCE-ID(;[^:]*)?:/i, ''))) { i = j; continue; } // drop forward override
      out.push(...filterExdate(block));
      i = j;
      continue;
    }
    out.push(lines[i]);
    i++;
  }
  return fold(out);
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
