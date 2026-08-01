// invites.js — iTIP invitations: the schedule inbox listing and the
// accept/tentative/decline answers that make the server deliver a REPLY to
// the organizer. CANCELs get a remove-from-calendar action; re-sent REQUESTs
// for the same UID are collapsed to the newest (by SEQUENCE) and flagged as
// updates of an event already on the calendar.

import * as dav from '../dav.js';
import * as ical from '../ical.js';
import { state, emit, myEmail } from './core.js';
import { getObject, putObject, deleteObject } from './objects.js';
import { instanceFrom, refreshEvents, eventHref } from './events.js';

// copiesOf lists the hrefs of every loaded instance carrying a UID — the
// server auto-delivers invite copies onto the attendee's default calendar,
// so an inbox REQUEST's event may already be stored (answer it in place,
// label it "Updated") and a CANCEL may have several copies to remove.
function copiesOf(uid) {
  const out = [];
  for (const list of state.instances.values()) {
    for (const i of list) if (i.uid === uid && !out.includes(i.href)) out.push(i.href);
  }
  return out;
}

export async function loadInvites() {
  const r = await dav.propfind(state.inbox, ['d:getetag'], '1');
  if (r.status !== 207) { state.invitesLoaded = true; emit('invites'); return; }
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
      const dtstart = ev && ical.prop(ev, 'DTSTART');
      items.push({
        href, etag, text, method,
        seq: ev ? +ical.propValue(ev, 'SEQUENCE') || 0 : 0,
        // The zone the organizer wrote the times in; the panel hints when it
        // differs from the zone the times are displayed in.
        tzid: dtstart ? (dtstart.params.TZID || (dtstart.value.endsWith('Z') ? 'UTC' : '')) : '',
        ...(inst ?? { summary: '(scheduling message)' }),
      });
    } catch { /* skip unreadable items */ }
  }

  // Collapse re-sent REQUESTs: one card per UID, newest SEQUENCE wins; the
  // superseded inbox copies are deleted together with the answered one. A
  // CANCEL for the same UID moots any pending REQUEST entirely — only the
  // cancellation card remains, and acting on it cleans up both.
  const byUid = new Map(), out = [];
  for (const it of items) {
    if (it.method !== 'REQUEST' || !it.uid) { out.push(it); continue; }
    const prev = byUid.get(it.uid);
    if (!prev) { byUid.set(it.uid, it); continue; }
    const [win, lose] = it.seq >= prev.seq ? [it, prev] : [prev, it];
    win.superseded = [...(win.superseded ?? []), ...(lose.superseded ?? []), lose.href];
    byUid.set(it.uid, win);
  }
  for (const it of [...out]) { // copy: stale CANCELs are spliced out of `out`
    if (it.method !== 'CANCEL' || !it.uid) continue;
    const req = byUid.get(it.uid);
    if (!req) continue;
    if (it.seq >= req.seq) {
      it.superseded = [...(it.superseded ?? []), ...(req.superseded ?? []), req.href];
      byUid.delete(it.uid);
    } else {
      // A re-invitation after a cancellation: the newer REQUEST wins and the
      // stale CANCEL is cleaned up alongside it.
      req.superseded = [...(req.superseded ?? []), ...(it.superseded ?? []), it.href];
      out.splice(out.indexOf(it), 1);
    }
  }
  for (const it of byUid.values()) {
    it.existingHref = copiesOf(it.uid)[0];
    it.updated = !!it.existingHref || (it.superseded ?? []).length > 0;
    out.push(it);
  }
  out.sort((a, b) => {
    // Two startless items must compare equal — Infinity - Infinity is NaN,
    // which makes the sort order undefined (cards jump between renders).
    const as = a.start ? +a.start : Infinity, bs = b.start ? +b.start : Infinity;
    return as === bs ? 0 : as - bs;
  });
  state.invites = out;
  state.invitesLoaded = true;
  emit('invites');
}

// dropLocal removes an inbox item from the rendered list immediately — the
// optimistic half of every answer; a failed request reloads to reconcile.
function dropLocal(invite) {
  state.invites = state.invites.filter(i => i.href !== invite.href);
  emit('invites');
}

function writableCalendars() {
  return state.calendars.filter(c => c.supportsEvents && !c.readOnly);
}

// answerInvite: ACCEPTED | TENTATIVE | DECLINED, stored onto calHref (the
// card's calendar picker; defaults to the preferred calendar). Storing our
// copy with the new PARTSTAT makes the server deliver the iTIP REPLY to the
// organizer; then the inbox item (and any superseded copies) are removed.
export async function answerInvite(invite, partstat, calHref = state.prefs.defaultCal) {
  dropLocal(invite);
  try {
    // The server may have auto-delivered a copy of this event onto one of our
    // calendars already; answering must rewrite that copy in place — storing a
    // second one under the picked calendar would duplicate the event.
    const existing = copiesOf(invite.uid)[0] ?? invite.existingHref;
    if (existing) {
      const { text, etag } = await getObject(existing);
      await putObject(existing, ical.setPartstat(text, myEmail(), partstat), etag);
    } else {
      const writable = writableCalendars();
      const target = writable.some(c => c.href === calHref) ? calHref : writable[0]?.href;
      if (!target) throw new Error('No writable calendar to add this event to.');
      const text = ical.setPartstat(invite.text, myEmail(), partstat);
      // Unconditional PUT (no If-Match / If-None-Match) is an intentional upsert:
      // the href is derived from the invite UID and lives in the user's own
      // calendar, so the only object it can hit is this same event — and a
      // re-sent REQUEST (a reschedule) must be allowed to update the stored copy,
      // which If-None-Match:* would reject.
      await putObject(eventHref(target, invite.uid), text, undefined);
    }
    for (const href of [invite.href, ...(invite.superseded ?? [])]) {
      await deleteObject(href).catch(() => {});
    }
  } catch (e) {
    await loadInvites().catch(() => {});
    throw e;
  }
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

// removeCancelled acts on a CANCEL: delete every stored copy of the event
// (the server's auto-delivered copy and an accepted one can coexist on
// different calendars; for copies outside the loaded range the fallback derives
// the UID href on every writable calendar), then the inbox item.
export async function removeCancelled(invite) {
  dropLocal(invite);
  try {
    let targets = copiesOf(invite.uid);
    if (!targets.length && invite.uid) {
      // Copies outside the loaded range aren't in the instance cache. An invite
      // may have been accepted onto any writable calendar, not just the default,
      // so derive the UID href on each and let the 404s for calendars without a
      // copy fall away.
      targets = writableCalendars().map(c => eventHref(c.href, invite.uid));
    }
    for (const t of targets) await deleteObject(t).catch(() => {});
    for (const href of [invite.href, ...(invite.superseded ?? [])]) {
      await deleteObject(href).catch(() => {});
    }
  } catch (e) {
    await loadInvites().catch(() => {});
    throw e;
  }
  await Promise.all([loadInvites(), refreshEvents()]);
}

export async function dismissInvite(invite) {
  dropLocal(invite);
  for (const href of [invite.href, ...(invite.superseded ?? [])]) {
    await deleteObject(href).catch(() => {});
  }
  await loadInvites();
}
