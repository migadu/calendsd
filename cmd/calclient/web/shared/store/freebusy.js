// freebusy.js — availability probing through the schedule outbox.

import * as dav from '../dav.js';
import * as ical from '../ical.js';
import { state, check, myEmail } from './core.js';
import { parseICalDate } from './events.js';

const { NS } = dav;

// freeBusy probes availability via the outbox; returns Map(email -> [{start,end}]).
export async function freeBusy(emails, start, end) {
  const body = ical.buildFreeBusyRequest({
    organizer: myEmail(), attendees: emails,
    start: start.toISOString(), end: end.toISOString(),
  });
  const r = check(await dav.req('POST', state.outbox, {
    headers: { 'Content-Type': 'text/calendar; charset=utf-8' }, body,
  }), [200]);
  const out = new Map(emails.map(e => [e, []]));
  if (!r.xml) return out;
  for (const resp of [...r.xml.getElementsByTagNameNS(NS.c, 'response')]) {
    const who = resp.getElementsByTagNameNS(NS.d, 'href')[0]?.textContent
      ?.replace(/^mailto:/i, '').toLowerCase();
    const data = resp.getElementsByTagNameNS(NS.c, 'calendar-data')[0]?.textContent ?? '';
    const busy = [];
    for (const line of ical.unfold(data)) {
      const m = /^FREEBUSY[^:]*:(.*)$/i.exec(line);
      if (!m) continue;
      for (const period of m[1].split(',')) {
        const [ps, pe] = period.split('/');
        const s = parseICalDate(ps, {});
        if (!s) continue;
        // A period end is either an explicit datetime or a DURATION
        // (RFC 5545 §3.3.9); a dropped duration would falsely read as "free".
        let end = null;
        if (/^[+-]?P/i.test(pe || '')) {
          const dur = ical.parseDuration(pe);
          if (dur != null) end = new Date(+s.date + dur);
        } else {
          const e = parseICalDate(pe, {});
          if (e) end = e.date;
        }
        if (end) busy.push({ start: s.date, end });
      }
    }
    const match = emails.find(e => e.toLowerCase() === who);
    if (match) out.set(match, busy);
  }
  return out;
}
