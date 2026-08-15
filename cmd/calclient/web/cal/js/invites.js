// invites.js — the invitations slide-over: schedule-inbox items as cards
// with Accept / Maybe / Decline, a per-card availability check and calendar
// picker, and first-class Cancelled / Reply cards.

import * as store from '/shared/store.js';
import { lastSegment } from '/shared/dav.js';
import { $, html, frag } from '/shared/dom.js';
import { icon } from '/shared/icons.js';
import { trapTabIn } from '/shared/dialogs.js';
import { DAY, startOfDay, sameDay } from '/shared/time.js';
import { fmtRange, fmtTime, toast, showError } from './ui.js';
import * as router from './router.js';

const PARTSTAT_LABEL = {
  ACCEPTED: 'accepted', DECLINED: 'declined', TENTATIVE: 'might come',
};

let opener = null;
let trapWired = false;
// Memoized out-of-range availability, keyed by inbox href + time window — see
// fillConflicts. Bounded by the number of invitations, so never cleared.
const fbCache = new Map();

// The panel overlays content, so keep Tab cycling inside it while open — the
// same trap every dialog gets (shared/dialogs.js), wired on first open rather
// than at module load: this module and main.js import each other, so main's
// helpers aren't initialized yet at our top level.
function wireFocusTrap(panel) {
  if (trapWired) return;
  trapWired = true;
  trapTabIn(panel);
}

export function toggleInvites(force) {
  const panel = $('#invites-panel');
  const open = force === undefined ? panel.hidden : force;
  if (open === !panel.hidden) return;
  panel.hidden = !open;
  const bell = $('#bell');
  bell.setAttribute('aria-expanded', String(open));
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
const principalName = href => lastSegment(href) || 'Someone';

// fmtWhen renders an invite's time range with a relative day label when the
// event is imminent, and hints when the organizer wrote the times in another
// zone than the one they are displayed in.
function fmtWhen(inv) {
  if (!inv.start) return '';
  const days = Math.round((startOfDay(inv.start) - startOfDay(new Date())) / DAY);
  const rel = days === 0 ? 'Today' : days === 1 ? 'Tomorrow' : '';
  let s;
  if (rel && !inv.allDay && sameDay(inv.start, inv.end)) {
    s = `${rel} ${fmtTime(inv.start)} – ${fmtTime(inv.end)}`;
  } else if (rel && inv.allDay) {
    s = `${rel} · all day`;
  } else {
    s = fmtRange(inv);
  }
  const shownIn = store.state.prefs.tz || Intl.DateTimeFormat().resolvedOptions().timeZone;
  const foreignTz = inv.tzid && inv.tzid !== 'UTC' && inv.tzid !== shownIn;
  return frag`${s}${foreignTz && frag` <span class="tz-hint">shown in your time zone</span>`}`;
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
    const card = html`<div class="invite">
      <div class="title">${icon('calendar', 15)} ${calName}</div>
      <div class="meta">
        ${who} shared a calendar with you · ${n.readWrite ? 'can edit' : 'read-only'}
        ${n.summary && frag`<br>${n.summary}`}
        ${n.status === 'accepted' ? frag`<br>Accepted` : n.status === 'declined' ? frag`<br>Declined` : ''}
      </div>
      <div class="actions">${pending ? frag`
        <button class="primary" data-share="accept">Accept</button>
        <button data-share="decline">Decline</button>` : frag`
        <button data-share-dismiss>Dismiss</button>`}
      </div>
    </div>`;
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
  const pick = !inv.existingHref && writable.length > 1 ? frag`
    <label class="cal-pick">Add to
      <select>${writable.map(c => frag`<option value="${c.href}" ${c.href === preferred ? 'selected' : ''}>
        ${c.displayName || c.name}</option>`)}</select>
    </label>` : '';
  const guests = (inv.attendees ?? []).length;
  const card = html`<div class="invite">
    <div class="title">${inv.updated && frag`<span class="type-tag">updated</span> `}${inv.summary ?? '(no title)'}</div>
    <div class="meta">
      ${inv.start && frag`<div class="when">${fmtWhen(inv)}</div>`}
      ${inv.location && frag`<div>${icon('pin', 13)} ${inv.location}</div>`}
      <div>${inv.organizer && frag`From ${inv.organizer}`}${guests > 1 ? ` · ${guests} guests` : ''}</div>
      <div class="conflict">Checking your availability…</div>
    </div>
    ${pick}
    <div class="actions">
      <button class="primary" data-a="ACCEPTED">Accept</button>
      <button data-a="TENTATIVE">Maybe</button>
      <button data-a="DECLINED">Decline</button>
    </div>
  </div>`;
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
  const card = html`<div class="invite">
    <div class="title"><span class="type-tag cancelled">cancelled</span> ${inv.summary ?? '(no title)'}</div>
    <div class="meta">
      ${inv.start && frag`<div class="when">${fmtWhen(inv)}</div>`}
      ${inv.organizer && frag`Cancelled by ${inv.organizer}`}
    </div>
    <div class="actions">
      <button class="primary" data-remove>Remove from calendar</button>
      <button data-dismiss>Dismiss</button>
    </div>
  </div>`;
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
  return html`<div class="invite">
    <div class="title">${inv.summary ?? '(no title)'}</div>
    <div class="meta">
      <span class="reply-status ${cls}">${a?.email ?? 'Someone'} ${verdict}</span>
      ${inv.start && frag`<div class="when">${fmtWhen(inv)}</div>`}
    </div>
    <div class="actions"><button data-dismiss>Dismiss</button></div>
  </div>`;
}

function otherCard(inv) {
  return html`<div class="invite">
    <div class="title">${inv.summary ?? '(no title)'}</div>
    <div class="meta">
      ${inv.start && frag`<div class="when">${fmtWhen(inv)}</div>`}
      ${inv.organizer && frag`From ${inv.organizer}`} · ${inv.method.toLowerCase()}
    </div>
    <div class="actions"><button data-dismiss>Dismiss</button></div>
  </div>`;
}

// emptyState points at the next event still awaiting a reply (an invitation
// auto-delivered to a calendar never passes through the inbox).
function emptyState(list) {
  const now = new Date();
  const next = store.visibleInstances()
    .filter(i => i.myPartstat === 'NEEDS-ACTION' && !i.cancelled && i.end > now)[0];
  list.append(html`<p class="empty">No pending invitations.</p>`);
  if (next) {
    const go = html`<button class="goto-event">Awaiting your reply: ${next.summary}</button>`;
    go.addEventListener('click', () => {
      toggleInvites(false);
      // Point the calendar at the event's day (behind the overlay) first, so
      // closing the event lands there rather than on wherever the grid was; then
      // open the event itself. Works from whichever section the panel was in.
      router.navigate(router.calendarPath({ view: 'day', date: next.start }), { replace: true });
      router.navigate(router.eventPath(next.uid, { occ: next.recurring ? next.start : null }));
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
      if (!store.state.invitesLoaded) list.append(html`<p class="empty">Loading…</p>`);
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
