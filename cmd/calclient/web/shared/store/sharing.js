// sharing.js — calendar sharing, both sides: the owner's sharee roster
// ({cs}share POSTs, {cs}invite property) and the sharee's notification
// inbox ({cs}invite-notification items and the invite-reply POST).

import * as dav from '../dav.js';
import { state, emit, check } from './core.js';
import { loadCalendars } from './calendars.js';
import { deleteObject } from './objects.js';

const { NS, key } = dav;

// shareCalendar invites a sharee (by email) to the owner's calendar via a
// calendarserver {cs}share POST. readWrite=false grants read-only access. The
// sharee's own calendar home then lists the shared calendar (read-only marked).
export async function shareCalendar(href, { email, summary = '', readWrite = false }) {
  const body = dav.xmlDoc('cs:share', '', dav.xml`<cs:set>
    <d:href>mailto:${email}</d:href>
    <cs:summary>${summary}</cs:summary>${readWrite && dav.xml`<cs:read-write/>`}
  </cs:set>`);
  check(await dav.req('POST', href, {
    headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
  }), [200]);
}

// loadSharees reads the {cs}invite property of a calendar the caller owns and
// returns its roster: one entry per person the calendar is shared with, with
// their address, display name, access level, and invitation status.
export async function loadSharees(href) {
  const r = await dav.propfind(href, ['cs:invite'], '0');
  if (r.status !== 207) return [];
  const invite = dav.msResponses(r.xml)[0]?.props.get(key(NS.cs, 'invite'));
  if (!invite) return [];
  return [...invite.getElementsByTagNameNS(NS.cs, 'user')].map(u => {
    const addr = u.getElementsByTagNameNS(NS.d, 'href')[0]?.textContent ?? '';
    const access = u.getElementsByTagNameNS(NS.cs, 'access')[0];
    return {
      href: addr,
      email: addr.replace(/^mailto:/i, ''),
      commonName: u.getElementsByTagNameNS(NS.cs, 'common-name')[0]?.textContent ?? '',
      readWrite: !!access?.getElementsByTagNameNS(NS.cs, 'read-write').length,
      status: u.getElementsByTagNameNS(NS.cs, 'invite-accepted').length ? 'accepted'
        : u.getElementsByTagNameNS(NS.cs, 'invite-declined').length ? 'declined' : 'pending',
    };
  });
}

// unshareCalendar revokes a sharee's access. Removal is by the sharee's address
// (from loadSharees, or the address used to share).
export async function unshareCalendar(href, email) {
  const body = dav.xmlDoc('cs:share', '',
    dav.xml`<cs:remove><d:href>mailto:${email}</d:href></cs:remove>`);
  check(await dav.req('POST', href, {
    headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
  }), [200]);
}

// ---- share invitations (a sharee's notification inbox) -------------------

// loadShareNotifications reads the calendarserver notification collection
// (/calendars/{user}/notifications/) where a share invitation lands for the
// sharee, and parses each {cs}invite-notification into a card model.
export async function loadShareNotifications() {
  const base = state.calHome + 'notifications/';
  // The collection only exists when the backend supports notifications; a 404
  // there just means no inbox, so treat any non-207 as "nothing pending".
  const r = await dav.propfind(base, ['d:getetag'], '1');
  if (r.status !== 207) { state.shareNotifications = []; emit('sharenotifs'); return; }
  const hrefs = dav.msResponses(r.xml).map(x => x.href)
    .filter(h => h !== base && !h.endsWith('/'));
  const items = [];
  for (const href of hrefs) {
    try {
      const res = await dav.req('GET', href);
      const inv = res.xml && dav.child(res.xml.documentElement, NS.cs, 'invite-notification');
      if (!inv) continue;                                   // not a share invite
      const org = dav.child(inv, NS.cs, 'organizer');
      const access = dav.child(inv, NS.cs, 'access');
      items.push({
        href,                                               // for accept-then-dismiss / decline
        uid: dav.child(inv, NS.cs, 'uid')?.textContent ?? '',
        hostHref: dav.hrefText(dav.child(inv, NS.cs, 'hosturl')),
        summary: dav.child(inv, NS.cs, 'summary')?.textContent ?? '',
        readWrite: !!access?.getElementsByTagNameNS(NS.cs, 'read-write').length,
        organizer: dav.hrefText(org),
        organizerName: (org && dav.child(org, NS.cs, 'common-name')?.textContent) || '',
        status: dav.child(inv, NS.cs, 'invite-accepted') ? 'accepted'
          : dav.child(inv, NS.cs, 'invite-declined') ? 'declined' : 'noresponse',
      });
    } catch { /* skip unreadable items */ }
  }
  state.shareNotifications = items;
  emit('sharenotifs');
}

// replyShareInvite accepts or declines a share invitation. The reply is a
// {cs}invite-reply POST to the sharee's own calendar home; on success the
// notification is dismissed and the calendar list refreshed (an accepted share
// becomes writable/visible, a declined one drops away).
export async function replyShareInvite(notif, accepted) {
  const body = dav.xmlDoc('cs:invite-reply', '', dav.xml`
    <cs:hosturl><d:href>${notif.hostHref}</d:href></cs:hosturl>
    <cs:in-reply-to>${notif.uid}</cs:in-reply-to>
    ${accepted ? dav.xml`<cs:invite-accepted/>` : dav.xml`<cs:invite-declined/>`}`);
  check(await dav.req('POST', state.calHome, {
    headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
  }), [200]);
  await dismissShareNotification(notif);
  await loadCalendars();
}

// dismissShareNotification removes a notification from the inbox without
// otherwise responding to it.
export async function dismissShareNotification(notif) {
  await deleteObject(notif.href).catch(() => {});
  await loadShareNotifications();
}
