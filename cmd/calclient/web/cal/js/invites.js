// invites.js — the invitations slide-over: schedule-inbox items as cards
// with Accept / Maybe / Decline.

import * as store from '/shared/store.js';
import { $, h, esc, fmtRange, toast, showError } from './main.js';

export function toggleInvites(force) {
  const panel = $('#invites-panel');
  panel.hidden = force === undefined ? !panel.hidden : !force;
  if (!panel.hidden) {
    renderInvites();
    store.loadInvites().catch(() => {});
    store.loadShareNotifications().catch(() => {});
  }
}

// principalName turns a principal href (/principals/alice/) into a bare name
// for display when no common-name accompanies the share invitation.
function principalName(href) {
  return decodeURIComponent(String(href).replace(/\/+$/, '').split('/').pop() || '') || 'Someone';
}

// renderShareInvites lists calendar-share invitations as cards with
// Accept / Decline (or Dismiss once answered).
function renderShareInvites(list) {
  for (const n of store.state.shareNotifications) {
    const who = n.organizerName || principalName(n.organizer);
    const calName = principalName(n.hostHref);
    const pending = n.status === 'noresponse';
    const card = h(`<div class="invite">
      <div class="title">📅 ${esc(calName)}</div>
      <div class="meta">
        ${esc(who)} shared a calendar with you · ${n.readWrite ? 'can edit' : 'read-only'}
        ${n.summary ? '<br>' + esc(n.summary) : ''}
        ${n.status === 'accepted' ? '<br>Accepted' : n.status === 'declined' ? '<br>Declined' : ''}
      </div>
      <div class="actions">${pending ? `
        <button class="primary" data-share="accept">Accept</button>
        <button data-share="decline">Decline</button>` : `
        <button data-share-dismiss>Dismiss</button>`}
      </div>
    </div>`);
    for (const b of card.querySelectorAll('button[data-share]')) {
      b.addEventListener('click', async () => {
        for (const x of card.querySelectorAll('button')) x.disabled = true;
        try {
          await store.replyShareInvite(n, b.dataset.share === 'accept');
          toast(b.dataset.share === 'accept' ? 'Calendar added.' : 'Invitation declined.');
        } catch (e) { showError(e); for (const x of card.querySelectorAll('button')) x.disabled = false; }
      });
    }
    card.querySelector('button[data-share-dismiss]')?.addEventListener('click', async () => {
      try { await store.dismissShareNotification(n); } catch (e) { showError(e); }
    });
    list.append(card);
  }
}

export function renderInvites() {
  const list = $('#invites-list');
  list.innerHTML = '';
  renderShareInvites(list);
  if (!store.state.invites.length) {
    if (!store.state.shareNotifications.length) {
      list.append(h('<p class="empty">No pending invitations.</p>'));
    }
    return;
  }
  for (const inv of store.state.invites) {
    const isRequest = inv.method === 'REQUEST' && inv.uid;
    const card = h(`<div class="invite">
      <div class="title">${esc(inv.summary ?? '(no title)')}</div>
      <div class="meta">
        ${inv.start ? esc(fmtRange(inv)) + '<br>' : ''}
        ${inv.organizer ? 'From ' + esc(inv.organizer) : ''}
        ${inv.method !== 'REQUEST' ? ' · ' + esc(inv.method.toLowerCase()) : ''}
      </div>
      <div class="actions">${isRequest ? `
        <button class="primary" data-a="ACCEPTED">Accept</button>
        <button data-a="TENTATIVE">Maybe</button>
        <button data-a="DECLINED">Decline</button>` : `
        <button data-dismiss>Dismiss</button>`}
      </div>
    </div>`);
    for (const b of card.querySelectorAll('button[data-a]')) {
      b.addEventListener('click', async () => {
        b.disabled = true;
        try {
          await store.answerInvite(inv, b.dataset.a);
          toast(b.dataset.a === 'DECLINED' ? 'Declined.' :
            b.dataset.a === 'TENTATIVE' ? 'Marked as maybe.' : 'Added to your calendar.');
        } catch (e) { showError(e); b.disabled = false; }
      });
    }
    card.querySelector('button[data-dismiss]')?.addEventListener('click', async () => {
      try { await store.dismissInvite(inv); } catch (e) { showError(e); }
    });
    list.append(card);
  }
}
