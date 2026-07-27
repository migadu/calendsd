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
  }
}

export function renderInvites() {
  const list = $('#invites-list');
  list.innerHTML = '';
  if (!store.state.invites.length) {
    list.append(h('<p class="empty">No pending invitations.</p>'));
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
