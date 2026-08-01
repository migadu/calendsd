// invites.js — the invitations slide-over: schedule-inbox items as cards
// with Accept / Maybe / Decline, a per-card availability check and calendar
// picker, and first-class Cancelled / Reply cards.

import * as store from '/shared/store.js';
import { $, h, esc, fmtRange, fmtTime, toast, showError, showCalendarView } from './main.js';
import { setGridView } from './grid.js';
import { openEventDetail } from './event-detail.js';

const PARTSTAT_LABEL = {
  ACCEPTED: 'accepted', DECLINED: 'declined', TENTATIVE: 'might come',
};

let opener = null;
let trapWired = false;
// Memoized out-of-range availability, keyed by inbox href + time window — see
// fillConflicts. Bounded by the number of invitations, so never cleared.
const fbCache = new Map();

// The panel overlays content, so keep Tab cycling inside it while open.
// (Wired on first open, not at module load: this module and main.js import
// each other, so main's helpers aren't initialized yet at our top level.)
function wireFocusTrap(panel) {
  if (trapWired) return;
  trapWired = true;
  panel.addEventListener('keydown', e => {
    if (e.key !== 'Tab') return;
    const focusable = [...panel.querySelectorAll('button, select, a[href]')]
      .filter(x => !x.disabled && x.offsetParent !== null);
    if (!focusable.length) return;
    const first = focusable[0], last = focusable[focusable.length - 1];
    if (e.shiftKey && (document.activeElement === first || document.activeElement === panel)) {
      e.preventDefault(); last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault(); first.focus();
    }
  });
}

export function toggleInvites(force) {
  const panel = $('#invites-panel');
  const open = force === undefined ? panel.hidden : force;
  if (open === !panel.hidden) return;
  panel.hidden = !open;
  if (open) {
    wireFocusTrap(panel);
    opener = document.activeElement;
    renderInvites();
    store.loadInvites().catch(() => {});
    store.loadShareNotifications().catch(() => {});
    panel.focus();
  } else {
    if (opener?.isConnected) opener.focus();
    opener = null;
  }
}

// principalName turns a principal href (/principals/alice/) into a bare name
// for display when no common-name accompanies the share invitation.
function principalName(href) {
  return decodeURIComponent(String(href).replace(/\/+$/, '').split('/').pop() || '') || 'Someone';
}

function startOfDay(d) { return new Date(d.getFullYear(), d.getMonth(), d.getDate()); }

// fmtWhen renders an invite's time range with a relative day label when the
// event is imminent, and hints when the organizer wrote the times in another
// zone than the one they are displayed in.
function fmtWhen(inv) {
  if (!inv.start) return '';
  const days = Math.round((startOfDay(inv.start) - startOfDay(new Date())) / 864e5);
  const rel = days === 0 ? 'Today' : days === 1 ? 'Tomorrow' : '';
  let s;
  if (rel && !inv.allDay && inv.start.toDateString() === inv.end.toDateString()) {
    s = `${rel} ${fmtTime(inv.start)} – ${fmtTime(inv.end)}`;
  } else if (rel && inv.allDay) {
    s = `${rel} · all day`;
  } else {
    s = fmtRange(inv);
  }
  const shownIn = store.state.prefs.tz || Intl.DateTimeFormat().resolvedOptions().timeZone;
  if (inv.tzid && inv.tzid !== 'UTC' && inv.tzid !== shownIn) {
    s += ' <span class="tz-hint">shown in your time zone</span>';
  }
  return s;
}

// fillConflicts answers "am I free?" on a card. Inside the loaded range the
// check is local (and can name the clashing event, excluding this UID and
// declined copies); outside it falls back to a free-busy probe of our own
// availability through the outbox.
async function fillConflicts(box, inv) {
  if (!inv.start || !inv.end) { box.remove(); return; }
  try {
    const { range } = store.state;
    if (range && inv.start >= range.start && inv.end <= range.end) {
      const clashes = [];
      for (const list of store.state.instances.values()) {
        for (const i of list) {
          if (i.uid === inv.uid || i.myPartstat === 'DECLINED' || i.cancelled) continue;
          if (i.start < inv.end && i.end > inv.start) clashes.push(i);
        }
      }
      if (!clashes.length) { box.textContent = 'No conflicts'; box.classList.add('free'); return; }
      const names = clashes.slice(0, 2).map(c =>
        `${c.summary}${c.allDay ? '' : ' (' + fmtTime(c.start) + ')'}`).join(', ');
      box.textContent = 'Overlaps: ' + names + (clashes.length > 2 ? ` +${clashes.length - 2} more` : '');
      box.classList.add('busy');
      return;
    }
    // Outside the loaded range we can't compute conflicts locally, so probe our
    // own availability via the outbox. A principal with no mailto: address can't
    // be probed — say nothing rather than falsely claiming "No conflicts".
    const me = store.myEmail();
    if (!me) { box.remove(); return; }
    // Cache the verdict per invite+window: the panel re-renders on every
    // event-cache change (the 30s ctag poll), and without this each render would
    // re-POST a free-busy probe and flicker the line back to "Checking…".
    const key = `${inv.href}|${+inv.start}|${+inv.end}`;
    let busy = fbCache.get(key);
    if (busy === undefined) {
      const slots = (await store.freeBusy([me], inv.start, inv.end)).get(me) ?? [];
      busy = slots.some(b => b.start < inv.end && b.end > inv.start);
      fbCache.set(key, busy);
    }
    if (busy) {
      box.textContent = 'You have something at that time';
      box.classList.add('busy');
    } else {
      box.textContent = 'No conflicts';
      box.classList.add('free');
    }
  } catch { box.remove(); }
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

// requestCard renders an incoming (or re-sent) REQUEST: context to decide —
// when, where, who, availability — a calendar picker, and the three answers.
function requestCard(inv) {
  const writable = store.state.calendars.filter(c => c.supportsEvents && !c.readOnly);
  const preferred = writable.some(c => c.href === store.state.prefs.defaultCal)
    ? store.state.prefs.defaultCal : writable[0]?.href;
  // No picker when a copy is already stored (auto-delivered or a reschedule):
  // the answer updates that copy in place, so there is nothing to choose.
  const pick = !inv.existingHref && writable.length > 1 ? `
    <label class="cal-pick">Add to
      <select>${writable.map(c => `<option value="${esc(c.href)}"${c.href === preferred ? ' selected' : ''}>
        ${esc(c.displayName || c.name)}</option>`).join('')}</select>
    </label>` : '';
  const guests = (inv.attendees ?? []).length;
  const card = h(`<div class="invite">
    <div class="title">${inv.updated ? '<span class="type-tag">updated</span> ' : ''}${esc(inv.summary ?? '(no title)')}</div>
    <div class="meta">
      ${inv.start ? '<div class="when">' + fmtWhen(inv) + '</div>' : ''}
      ${inv.location ? '<div>📍 ' + esc(inv.location) + '</div>' : ''}
      <div>${inv.organizer ? 'From ' + esc(inv.organizer) : ''}${guests > 1 ? ` · ${guests} guests` : ''}</div>
      <div class="conflict">Checking your availability…</div>
    </div>
    ${pick}
    <div class="actions">
      <button class="primary" data-a="ACCEPTED">Accept</button>
      <button data-a="TENTATIVE">Maybe</button>
      <button data-a="DECLINED">Decline</button>
    </div>
  </div>`);
  fillConflicts(card.querySelector('.conflict'), inv);
  for (const b of card.querySelectorAll('button[data-a]')) {
    b.addEventListener('click', async () => {
      const cal = card.querySelector('select')?.value ?? preferred;
      for (const x of card.querySelectorAll('button, select')) x.disabled = true;
      card.remove(); // optimistic; a failure reloads the list and re-renders
      try {
        await store.answerInvite(inv, b.dataset.a, cal);
        toast(b.dataset.a === 'DECLINED' ? 'Declined.' :
          b.dataset.a === 'TENTATIVE' ? 'Marked as maybe.' :
          'Added to your calendar — you can change your reply on the event.');
      } catch (e) { showError(e); }
    });
  }
  return card;
}

// cancelCard renders a CANCEL: the event is dead, so the primary action is
// removing our stored copy, not just dismissing the notification.
function cancelCard(inv) {
  const card = h(`<div class="invite">
    <div class="title"><span class="type-tag cancelled">cancelled</span> ${esc(inv.summary ?? '(no title)')}</div>
    <div class="meta">
      ${inv.start ? '<div class="when">' + fmtWhen(inv) + '</div>' : ''}
      ${inv.organizer ? 'Cancelled by ' + esc(inv.organizer) : ''}
    </div>
    <div class="actions">
      <button class="primary" data-remove>Remove from calendar</button>
      <button data-dismiss>Dismiss</button>
    </div>
  </div>`);
  card.querySelector('[data-remove]').addEventListener('click', async () => {
    card.remove();
    try { await store.removeCancelled(inv); toast('Removed from your calendar.'); }
    catch (e) { showError(e); }
  });
  return card;
}

// replyCard renders a REPLY (we organize, someone answered) with the answer
// colored like the guest chips in the editor.
function replyCard(inv) {
  const a = (inv.attendees ?? [])[0];
  const verdict = a ? (PARTSTAT_LABEL[a.partstat] || a.partstat.toLowerCase()) : 'replied';
  const cls = a?.partstat === 'ACCEPTED' ? 'accepted' : a?.partstat === 'DECLINED' ? 'declined' : '';
  const card = h(`<div class="invite">
    <div class="title">${esc(inv.summary ?? '(no title)')}</div>
    <div class="meta">
      <span class="reply-status ${cls}">${esc(a?.email ?? 'Someone')} ${esc(verdict)}</span>
      ${inv.start ? '<div class="when">' + fmtWhen(inv) + '</div>' : ''}
    </div>
    <div class="actions"><button data-dismiss>Dismiss</button></div>
  </div>`);
  return card;
}

function otherCard(inv) {
  return h(`<div class="invite">
    <div class="title">${esc(inv.summary ?? '(no title)')}</div>
    <div class="meta">
      ${inv.start ? '<div class="when">' + fmtWhen(inv) + '</div>' : ''}
      ${inv.organizer ? 'From ' + esc(inv.organizer) : ''} · ${esc(inv.method.toLowerCase())}
    </div>
    <div class="actions"><button data-dismiss>Dismiss</button></div>
  </div>`);
}

// emptyState points at the next event still awaiting a reply (an invitation
// auto-delivered to a calendar never passes through the inbox).
function emptyState(list) {
  const now = new Date();
  const next = store.visibleInstances()
    .filter(i => i.myPartstat === 'NEEDS-ACTION' && !i.cancelled && i.end > now)[0];
  list.append(h('<p class="empty">No pending invitations.</p>'));
  if (next) {
    const go = h(`<button class="goto-event">Awaiting your reply: ${esc(next.summary)}</button>`);
    go.addEventListener('click', () => {
      toggleInvites(false);
      // The panel can be opened from any view; switch back to the calendar
      // before scrolling the grid to the event, or it stays hidden behind the
      // contacts/settings view.
      showCalendarView();
      setGridView('day', next.start);
      openEventDetail(next);
    });
    list.append(go);
  }
}

export function renderInvites() {
  const list = $('#invites-list');
  list.innerHTML = '';
  renderShareInvites(list);
  if (!store.state.invites.length) {
    if (!store.state.shareNotifications.length) {
      if (!store.state.invitesLoaded) list.append(h('<p class="empty">Loading…</p>'));
      else emptyState(list);
    }
    return;
  }
  for (const inv of store.state.invites) {
    const card =
      inv.method === 'REQUEST' && inv.uid ? requestCard(inv) :
      inv.method === 'CANCEL' && inv.uid ? cancelCard(inv) :
      inv.method === 'REPLY' ? replyCard(inv) : otherCard(inv);
    card.querySelector('button[data-dismiss]')?.addEventListener('click', async () => {
      card.remove();
      try { await store.dismissInvite(inv); } catch (e) { showError(e); }
    });
    list.append(card);
  }
}
