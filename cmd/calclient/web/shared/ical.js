// ical.js — just enough iCalendar/vCard to round-trip the fields the UI
// edits. The server does the real validation; malformed bodies surface as
// 4xx in the raw drawer, which is itself a useful demonstration.

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
  organizer, attendees = [], description, location, url, recurrenceId, confidential }) {
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
    l.push(`ATTENDEE;PARTSTAT=${partstat};RSVP=TRUE:mailto:${at.email}`);
  }
  return l;
}

// buildEvent renders a single-VEVENT VCALENDAR.
export function buildEvent(fields) {
  return ['BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//calclient//EN',
    'BEGIN:VEVENT', ...eventLines(fields), 'END:VEVENT', 'END:VCALENDAR']
    .join('\r\n') + '\r\n';
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

// parseDuration converts an iCal DURATION (RFC 5545 §3.3.6) to milliseconds,
// or null if it can't be parsed.
export function parseDuration(v) {
  const m = /^([+-]?)P(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$/i.exec(String(v).trim());
  if (!m) return null;
  const [w, d, h, mi, s] = [m[2], m[3], m[4], m[5], m[6]].map(x => +(x || 0));
  const secs = (((w * 7 + d) * 24 + h) * 60 + mi) * 60 + s;
  return (m[1] === '-' ? -1 : 1) * secs * 1000;
}

// masterEventRange finds [begin, end] line indexes (inclusive) of the first
// VEVENT without a RECURRENCE-ID (the recurrence master / the plain event).
function masterEventRange(lines) {
  let begin = -1;
  for (let i = 0; i < lines.length; i++) {
    if (/^BEGIN:VEVENT/i.test(lines[i])) begin = i;
    else if (/^END:VEVENT/i.test(lines[i]) && begin >= 0) {
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

// addExdate excludes one occurrence (dtValue in the same form as DTSTART)
// from the master VEVENT.
export function addExdate(text, dtValue, allDay) {
  const lines = unfold(text);
  const range = masterEventRange(lines);
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
// master VEVENT while leaving everything else — nested VALARMs, sibling
// override VEVENTs, EXDATEs, and properties this client doesn't model —
// untouched. `props` maps an upper-case NAME to the full iCal line to emit
// (params included), an array of lines (for multi-valued props like ATTENDEE),
// or null to delete the property. Absent props are appended before END:VEVENT.
export function setMasterProps(text, props) {
  const lines = unfold(text);
  const range = masterEventRange(lines);
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
      if (--depth === 0) { // closing the master VEVENT: flush new properties
        for (const [name, val] of want) if (!seen.has(name)) push(body, val);
      }
      body.push(line);
      continue;
    }
    // Only rewrite direct VEVENT properties (depth 1); depth 2 is inside a
    // nested VALARM, whose DESCRIPTION/etc must stay untouched.
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

// shiftDate rewrites a DTSTART/DTEND line shifted by deltaMs, or returns null
// for forms we can't shift safely (TZID or floating local time) so the caller
// leaves the stored value unchanged rather than corrupting it.
export function shiftDate(name, prop, deltaMs) {
  const { value, params = {} } = prop;
  if (params.TZID) return null;
  if (params.VALUE === 'DATE' || /^\d{8}$/.test(value)) {
    const days = Math.round(deltaMs / 864e5);
    const d = new Date(+value.slice(0, 4), +value.slice(4, 6) - 1, +value.slice(6, 8) + days);
    const p = x => String(x).padStart(2, '0');
    return `${name};VALUE=DATE:${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}`;
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
  const params = Object.entries(prop.params).map(([k, v]) => `;${k}=${v}`).join('');
  return foldLong(`${prop.name}${params}:${prop.value}`);
}

// MANAGED_VCARD: properties the editor owns; everything else is preserved.
export const MANAGED_VCARD = new Set(
  ['BEGIN', 'END', 'VERSION', 'UID', 'PRODID', 'FN', 'N', 'ORG', 'TITLE',
    'EMAIL', 'TEL', 'ADR', 'URL', 'BDAY', 'NOTE']);

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
