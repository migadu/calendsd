// event-detail.js — read-only event popup shown when an event is clicked.
// Links (meeting URL, and any URL in the location/notes) are clickable here;
// Edit opens the full editor. This is why clicking an event no longer jumps
// straight into editing.

import * as store from '/shared/store.js';
import { $, h, esc, fmtRange, linkify, toast, showError } from './main.js';
import { openEventDialog } from './event-dialog.js';

const PARTSTAT_LABEL = {
  ACCEPTED: 'accepted', DECLINED: 'declined',
  TENTATIVE: 'maybe', 'NEEDS-ACTION': 'no reply',
};

export function openEventDetail(inst) {
  const cal = store.state.calendars.find(c => c.href === inst.calHref);
  const color = store.calColor(inst.calHref);
  const rows = [];
  const row = (label, valueHtml) => rows.push(
    `<div class="detail-row"><span class="dl">${esc(label)}</span><span class="dv">${valueHtml}</span></div>`);

  row('When', esc(fmtRange(inst)) + (inst.recurring ? ' <span class="type-tag">repeats</span>' : ''));
  row('Calendar', `<span class="cal-dot" style="background:${esc(color)}"></span>${esc(cal?.displayName || cal?.name || '')}`);
  if (inst.url) {
    row('Meeting', `<a class="join-btn" href="${esc(inst.url)}" target="_blank" rel="noreferrer noopener">Join</a>
      <a href="${esc(inst.url)}" target="_blank" rel="noreferrer noopener" class="join-url">${esc(inst.url)}</a>`);
  }
  if (inst.location) row('Location', linkify(inst.location));
  if (inst.description) row('Notes', linkify(inst.description).replace(/\n/g, '<br>'));
  if (inst.organizer) row('Organizer', esc(inst.organizer));
  if (inst.attendees?.length) {
    row('Guests', inst.attendees.map(a =>
      `<div>${esc(a.email)} <span class="type-tag">${esc(PARTSTAT_LABEL[a.partstat] || a.partstat.toLowerCase())}</span></div>`).join(''));
  }

  const dlg = h(`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="Event details">
    <div class="detail-head">
      <span class="cal-swatch" style="background:${esc(color)}" aria-hidden="true"></span>
      <h2 style="margin:0">${esc(inst.summary)}</h2>
    </div>
    <div class="detail-body">${rows.join('')}</div>
    <div class="actions">
      <button class="danger" id="ed-del">Delete…</button>
      <button id="ed-close">Close</button>
      <button class="primary" id="ed-edit">Edit</button>
    </div>
  </div></div>`);
  const close = () => dlg.remove();
  dlg.addEventListener('click', ev => { if (ev.target === dlg) close(); });
  dlg.addEventListener('keydown', ev => { if (ev.key === 'Escape') { ev.stopPropagation(); close(); } });
  $('#ed-close', dlg).addEventListener('click', close);
  $('#ed-edit', dlg).addEventListener('click', () => { close(); openEventDialog(inst); });
  $('#ed-del', dlg).addEventListener('click', async () => {
    // Recurring events have per-occurrence vs whole-series choices — defer to
    // the editor's Delete for those; delete a plain event directly.
    if (inst.recurring) { close(); openEventDialog(inst); return; }
    if (!confirm('Delete this event?')) return;
    try { await store.deleteObject(inst.href); close(); toast('Deleted.'); await store.refreshEvents(); }
    catch (e) { showError(e); }
  });
  $('#dialog-host').append(dlg);
  $('#ed-close', dlg).focus();
}
