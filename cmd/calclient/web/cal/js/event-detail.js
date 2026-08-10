// event-detail.js — read-only event popup shown when an event is clicked.
// Links (meeting URL, and any URL in the location/notes) are clickable here;
// Edit opens the full editor. This is why clicking an event no longer jumps
// straight into editing.

import * as store from '/shared/store.js';
import { $, html, frag } from '/shared/dom.js';
import { fmtRange, linkify, toast, showError, scheduleStatusTag } from './ui.js';
import { openEventDialog } from './event-dialog.js';
import { confirmDialog, choiceDialog, openOverlay } from '/shared/dialogs.js';

const PARTSTAT_LABEL = {
  ACCEPTED: 'accepted', DECLINED: 'declined',
  TENTATIVE: 'maybe', 'NEEDS-ACTION': 'no reply',
};

export function openEventDetail(inst) {
  const cal = store.state.calendars.find(c => c.href === inst.calHref);
  const color = store.calColor(inst.calHref);
  const me = store.myEmail().toLowerCase();
  const rows = [];
  const row = (label, value) => rows.push(
    frag`<div class="detail-row"><span class="dl">${label}</span><span class="dv">${value}</span></div>`);

  // Delivery status is the organizer's concern: the server writes SCHEDULE-STATUS
  // onto the organizer's stored copy only (an event with attendees but no
  // ORGANIZER is one we created too), so only show delivery pills there.
  const iAmOrganizer = !inst.organizer || inst.organizer.toLowerCase() === me;
  const failedSends = iAmOrganizer
    ? (inst.attendees ?? []).filter(a => /^[35]\./.test(a.scheduleStatus))
    : [];

  row('When', frag`${fmtRange(inst)}${inst.recurring && frag` <span class="type-tag">repeats</span>`}`);
  row('Calendar', frag`<span class="cal-dot" style="background:${color}"></span>${cal?.displayName || cal?.name || ''}`);
  if (inst.url) {
    row('Meeting', frag`<a class="join-btn" href="${inst.url}" target="_blank" rel="noreferrer noopener">Join</a>
      <a href="${inst.url}" target="_blank" rel="noreferrer noopener" class="join-url">${inst.url}</a>`);
  }
  if (inst.location) row('Location', linkify(inst.location));
  if (inst.description) row('Notes', linkify(inst.description));
  if (inst.organizer) row('Organizer', inst.organizer);
  if (inst.attendees?.length) {
    row('Guests', inst.attendees.map(a =>
      frag`<div>${a.email} <span class="type-tag">${PARTSTAT_LABEL[a.partstat] || a.partstat.toLowerCase()}</span>${iAmOrganizer ? scheduleStatusTag(a.scheduleStatus) : ''}</div>`));
  }

  // An invitation (someone else organizes, we are on the attendee list) gets
  // Accept / Maybe / Decline here, so answering — or changing an earlier
  // answer — doesn't require the invitations panel.
  const isInvite = !!inst.myPartstat && !!inst.organizer && inst.organizer.toLowerCase() !== me;
  const rsvpBtn = (ps, label) =>
    frag`<button data-ps="${ps}" class="${inst.myPartstat === ps ? 'active' : ''}">${label}</button>`;

  const dlg = html`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="Event details">
    <button class="dialog-close" id="ed-x" aria-label="Close">×</button>
    <div class="detail-head">
      <span class="cal-swatch" style="background:${color}" aria-hidden="true"></span>
      <h2 style="margin:0">${inst.summary}</h2>
    </div>
    ${failedSends.length ? frag`<p class="detail-warn">⚠ ${failedSends.length === 1
      ? `An invitation to ${failedSends[0].email} couldn’t be delivered`
      : `${failedSends.length} invitations couldn’t be delivered`} — see the guest list below.</p>` : ''}
    <div class="detail-body">${rows}</div>
    ${isInvite && frag`<div class="rsvp"><span class="dl">Going?</span>
      ${rsvpBtn('ACCEPTED', 'Accept')}${rsvpBtn('TENTATIVE', 'Maybe')}${rsvpBtn('DECLINED', 'Decline')}
    </div>`}
    <div class="actions">
      <button class="danger" id="ed-del">Delete…</button>
      <button id="ed-edit">Edit</button>
      <button class="primary" id="ed-close">Done</button>
    </div>
  </div></div>`;
  const close = () => dlg.remove();
  openOverlay(dlg, close);
  for (const b of dlg.querySelectorAll('.rsvp button[data-ps]')) {
    b.addEventListener('click', async () => {
      if (b.dataset.ps === inst.myPartstat) return;
      for (const x of dlg.querySelectorAll('.rsvp button')) x.disabled = true;
      try {
        await store.answerEvent(inst, b.dataset.ps);
        close();
        toast(b.dataset.ps === 'DECLINED' ? 'Declined.' :
          b.dataset.ps === 'TENTATIVE' ? 'Marked as maybe.' : 'Accepted.');
      } catch (e) {
        showError(e);
        for (const x of dlg.querySelectorAll('.rsvp button')) x.disabled = false;
      }
    });
  }
  $('#ed-x', dlg).addEventListener('click', close);
  $('#ed-close', dlg).addEventListener('click', close);
  $('#ed-edit', dlg).addEventListener('click', () => { close(); openEventDialog(inst); });
  $('#ed-del', dlg).addEventListener('click', async () => {
    // A read-only share can't be deleted from — send it to the editor, which
    // says so and disables its write actions, rather than let the write 403.
    if (store.isReadOnly(inst.calHref)) { close(); openEventDialog(inst); return; }
    // Recurring events offer a per-occurrence vs whole-series choice right here;
    // a plain event just confirms. `scope` feeds store.deleteEvent either way.
    let scope = 'all';
    if (inst.recurring) {
      scope = await choiceDialog('This event repeats. What would you like to delete?', [
        { label: 'This event', value: 'one', danger: true },
        { label: 'All events', value: 'all', danger: true },
      ]);
      if (!scope) return;
    } else if (!await confirmDialog('Delete this event?', { confirmLabel: 'Delete', danger: true })) {
      return;
    }
    try { await store.deleteEvent(inst, scope); close(); toast('Deleted.'); await store.refreshEvents(); }
    catch (e) { showError(e); }
  });
  $('#ed-close', dlg).focus();
}
