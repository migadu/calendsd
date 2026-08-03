// settings.js — preferences, import and export.

import * as store from '/shared/store.js';
import * as pushSync from './push-sync.js';
import { $, html, frag } from '/shared/dom.js';
import { toast, showError } from './ui.js';
import { contactsDedupSweep } from './contacts.js';

export function renderSettings(main) {
  main.innerHTML = '';
  const p = store.state.prefs;
  const el = html`<div class="settings">
    <section>
      <h2>Display</h2>
      <label>Week starts on
        <select id="st-weekstart">
          <option value="1" ${p.weekStart === 1 ? 'selected' : ''}>Monday</option>
          <option value="0" ${p.weekStart === 0 ? 'selected' : ''}>Sunday</option>
        </select></label>
      <label>Time format
        <select id="st-hour12">
          <option value="0" ${!p.hour12 ? 'selected' : ''}>24-hour</option>
          <option value="1" ${p.hour12 ? 'selected' : ''}>12-hour</option>
        </select></label>
      <label>Time zone
        <select id="st-tz"></select></label>
      <label>Default view
        <select id="st-defview">
          <option value="month" ${p.defaultView === 'month' ? 'selected' : ''}>Month</option>
          <option value="week" ${p.defaultView === 'week' ? 'selected' : ''}>Week</option>
          <option value="agenda" ${p.defaultView === 'agenda' ? 'selected' : ''}>Agenda</option>
        </select></label>
      <label>Default event length
        <select id="st-defdur">
          <option value="15" ${p.defaultDuration === 15 ? 'selected' : ''}>15 minutes</option>
          <option value="30" ${p.defaultDuration === 30 ? 'selected' : ''}>30 minutes</option>
          <option value="60" ${p.defaultDuration === 60 ? 'selected' : ''}>1 hour</option>
          <option value="90" ${p.defaultDuration === 90 ? 'selected' : ''}>1.5 hours</option>
          <option value="120" ${p.defaultDuration === 120 ? 'selected' : ''}>2 hours</option>
        </select></label>
      <label>Default calendar for new events and invitations
        <select id="st-defcal"></select></label>
      <p class="note">Times are stored in UTC and shown in the time zone selected above.</p>
    </section>
    <section>
      <h2>Import</h2>
      <label>Add events or contacts from a file (.ics or .vcf)
        <input id="st-import" type="file" accept=".ics,.vcf,text/calendar,text/vcard"></label>
      <progress id="st-import-bar" class="import-bar" value="0" max="1" hidden></progress>
      <div class="import-status">
        <p class="note" id="st-import-note"></p>
        <button type="button" id="st-import-cancel" class="ghost" hidden>Cancel</button>
      </div>
    </section>
    <section>
      <h2>Contacts</h2>
      <p><button id="st-dedup">Find duplicate contacts…</button></p>
      <p class="note">Scans each address book for cards that share an email, phone, or name, and lets you review and merge them.</p>
    </section>
    <section>
      <h2>Export</h2>
      <div id="st-export"></div>
    </section>
    <section>
      <h2>Notifications</h2>
      <label class="row"><input type="checkbox" id="st-push" ${pushSync.pushEnabled() ? 'checked' : ''} ${pushSync.pushSupported() ? '' : 'disabled'}>
        Instant updates (push)</label>
      <p class="note">${pushSync.pushSupported()
        ? 'Get changes within seconds via WebDAV-Push instead of waiting for the periodic refresh. Your browser will ask permission to show notifications.'
        : 'This browser cannot receive push (it needs a secure context — https, or localhost during development).'}</p>
    </section>
    <section>
      <h2>Account</h2>
      <p>Signed in as <strong>${store.state.displayName || store.state.user}</strong>
        ${store.myEmail() && frag`(${store.myEmail()})`}</p>
      <p class="note">${p.staySignedIn
        ? 'You stay signed in on this device until you sign out.'
        : 'You are signed in for this browser session only.'}</p>
    </section>
  </div>`;
  main.append(el);

  // Only calendars that can hold new events/invitations — a read-only share or
  // a task/journal collection can't be the default target.
  const defSel = $('#st-defcal', el);
  for (const c of store.eventCalendars()) {
    defSel.append(html`<option value="${c.href}" ${c.href === p.defaultCal ? 'selected' : ''}>${c.displayName || c.name}</option>`);
  }

  // Time zone list: the browser's own IANA set when available, otherwise a
  // short curated fallback. First entry follows the device zone.
  const tzSel = $('#st-tz', el);
  const device = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  // Current UTC offset of the device zone, e.g. "UTC+02:00" — shown so the
  // label is transparent when the browser resolves the system zone to an
  // unexpected IANA name (a machine set to UTC/GMT resolves to Reykjavik).
  const offMin = -new Date().getTimezoneOffset();
  const off = `UTC${offMin < 0 ? '-' : '+'}${String(Math.floor(Math.abs(offMin) / 60)).padStart(2, '0')}:${String(Math.abs(offMin) % 60).padStart(2, '0')}`;
  let zones;
  try { zones = Intl.supportedValuesOf('timeZone'); }
  catch { zones = ['UTC', 'Europe/Zurich', 'Europe/London', 'America/New_York', 'America/Los_Angeles', 'Asia/Tokyo', device]; }
  tzSel.append(html`<option value="" ${!p.tz ? 'selected' : ''}>Device default — ${device.replace(/_/g, ' ')} (${off})</option>`);
  for (const z of [...new Set(zones)].sort()) {
    tzSel.append(html`<option value="${z}" ${p.tz === z ? 'selected' : ''}>${z.replace(/_/g, ' ')}</option>`);
  }

  $('#st-weekstart', el).addEventListener('change', ev => store.savePrefs({ weekStart: +ev.target.value }));
  $('#st-hour12', el).addEventListener('change', ev => store.savePrefs({ hour12: ev.target.value === '1' }));
  $('#st-defview', el).addEventListener('change', ev => store.savePrefs({ defaultView: ev.target.value }));
  $('#st-defdur', el).addEventListener('change', ev => store.savePrefs({ defaultDuration: +ev.target.value }));
  tzSel.addEventListener('change', ev => store.savePrefs({ tz: ev.target.value }));
  defSel.addEventListener('change', ev => store.savePrefs({ defaultCal: ev.target.value }));

  $('#st-push', el)?.addEventListener('change', async ev => {
    try {
      await pushSync.setPushEnabled(ev.target.checked);
      toast(ev.target.checked ? 'Instant updates on.' : 'Instant updates off.');
    } catch (e) {
      ev.target.checked = pushSync.pushEnabled();
      showError(e);
    }
  });

  $('#st-dedup', el).addEventListener('click', () => contactsDedupSweep());

  const exp = $('#st-export', el);
  for (const c of store.state.calendars) {
    exp.append(html`<label><a href="${c.href}?export" download="${c.name}.ics">Download “${c.displayName || c.name}” as .ics</a></label>`);
  }
  for (const b of store.state.books) {
    exp.append(html`<label><a href="${b.href}?export" download="contacts.vcf">Download “${b.displayName}” as .vcf</a></label>`);
  }

  // Reflect any import that's already running/finished (e.g. started before we
  // navigated away and back), then wire the file picker and Cancel button.
  updateImportProgress(el);
  $('#st-import', el).addEventListener('change', async ev => {
    const file = ev.target.files[0];
    if (!file) return;
    ev.target.value = ''; // allow re-picking the same file after this run
    try {
      const job = await store.startImport(file);
      if (job.status === 'done' || job.status === 'cancelled') toast(`Imported ${job.added} item(s).`);
    } catch (e) {
      showError(e);
    }
  });
  $('#st-import-cancel', el).addEventListener('click', () => store.cancelImport());
  $('#st-import', el).disabled = store.importRunning();
}

// importStatusText renders the current job as a one-line status. The running
// line covers the pre-parse "Reading…" phase (total still 0) and the per-item
// progress; terminal states summarize what was written.
function importStatusText(job) {
  if (!job) return '';
  if (job.status === 'running') return job.total ? `Importing… ${job.done}/${job.total}` : 'Reading…';
  if (job.status === 'empty') return 'Nothing to import in that file.';
  if (job.status === 'error') return job.error || 'Import failed.';
  const verb = job.status === 'cancelled' ? 'Cancelled — imported' : 'Imported';
  const parts = [`${verb} ${job.added}`];
  if (job.skipped) parts.push(`skipped ${job.skipped} already present`);
  if (job.failed) parts.push(`${job.failed} failed`);
  return parts.join(', ') + '.';
}

// updateImportProgress paints the import section from store.state.importJob. It
// queries the live DOM (root defaults to the document) so it always targets the
// currently-mounted Settings panel — the store-held job is the source of truth,
// so leaving and returning to Settings re-attaches to a still-running import.
export function updateImportProgress(root = document) {
  const bar = $('#st-import-bar', root);
  if (!bar) return; // Settings not currently mounted
  const note = $('#st-import-note', root);
  const cancel = $('#st-import-cancel', root);
  const input = $('#st-import', root);
  const job = store.state.importJob;
  const running = job?.status === 'running';

  note.textContent = importStatusText(job);
  cancel.hidden = !running;
  input.disabled = running;
  if (running && job.total) { bar.max = job.total; bar.value = job.done; bar.hidden = false; }
  else if (running) { bar.removeAttribute('value'); bar.hidden = false; } // indeterminate while reading
  else bar.hidden = true;
}
