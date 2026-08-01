// invites.js — iTIP invitations: the schedule inbox listing and the
// accept/tentative/decline answers that make the server deliver a REPLY to
// the organizer.

import * as dav from '../dav.js';
import * as ical from '../ical.js';
import { state, emit, myEmail } from './core.js';
import { getObject, putObject, deleteObject } from './objects.js';
import { instanceFrom, refreshEvents, eventHref } from './events.js';

export async function loadInvites() {
  const r = await dav.propfind(state.inbox, ['d:getetag'], '1');
  if (r.status !== 207) return;
  const hrefs = dav.msResponses(r.xml).map(x => x.href)
    .filter(h => h !== state.inbox && !h.endsWith('/'));
  const items = [];
  for (const href of hrefs) {
    try {
      const { text, etag } = await getObject(href);
      const tree = ical.parse(text);
      const ev = ical.components(tree, 'VEVENT')[0];
      const method = (tree.children[0] ? ical.propValue(tree.children[0], 'METHOD') : '') || 'REQUEST';
      const inst = ev ? instanceFrom(ev, href, '', etag) : null;
      items.push({ href, etag, text, method, ...(inst ?? { summary: '(scheduling message)' }) });
    } catch { /* skip unreadable items */ }
  }
  state.invites = items;
  emit('invites');
}

// answerInvite: ACCEPTED | TENTATIVE | DECLINED. Storing our copy with the
// new PARTSTAT makes the server deliver the iTIP REPLY to the organizer;
// then the inbox item is removed.
export async function answerInvite(invite, partstat) {
  const text = ical.setPartstat(invite.text, myEmail(), partstat);
  // Unconditional PUT (no If-Match / If-None-Match) is an intentional upsert:
  // the href is derived from the invite UID and lives in the user's own default
  // calendar, so the only object it can hit is this same event — and a
  // re-sent REQUEST (a reschedule) must be allowed to update the stored copy,
  // which If-None-Match:* would reject.
  await putObject(eventHref(state.prefs.defaultCal, invite.uid), text, undefined);
  await deleteObject(invite.href).catch(() => {});
  await Promise.all([loadInvites(), refreshEvents()]);
}

// answerEvent updates our PARTSTAT on an event already stored on the
// calendar (e.g. an invitation auto-delivered by the server, or changing an
// earlier answer). As with answerInvite, rewriting the stored copy makes the
// server deliver the iTIP REPLY to the organizer.
export async function answerEvent(inst, partstat) {
  const { text, etag } = await getObject(inst.href);
  await putObject(inst.href, ical.setPartstat(text, myEmail(), partstat), etag);
  await refreshEvents();
}

export async function dismissInvite(invite) {
  await deleteObject(invite.href).catch(() => {});
  await loadInvites();
}
