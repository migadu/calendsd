// settings.js — preferences, import and export.

import * as store from '/shared/store.js';
import * as ical from '/shared/ical.js';
import * as pushSync from './push-sync.js';
import { $, h, esc, toast, showError } from './main.js';

export function renderSettings(main) {
  main.innerHTML = '';
  const p = store.state.prefs;
  const el = h(`<div class="settings">
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
      <p class="note" id="st-import-note"></p>
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
      <p>Signed in as <strong>${esc(store.state.displayName || store.state.user)}</strong>
        ${store.myEmail() ? '(' + esc(store.myEmail()) + ')' : ''}</p>
      <p class="note">${p.staySignedIn
        ? 'You stay signed in on this device until you sign out.'
        : 'You are signed in for this browser session only.'}</p>
    </section>
  </div>`);
  main.append(el);

  // Only calendars that can hold new events/invitations — a read-only share or
  // a task/journal collection can't be the default target.
  const defSel = $('#st-defcal', el);
  for (const c of store.eventCalendars()) {
    defSel.append(h(`<option value="${esc(c.href)}" ${c.href === p.defaultCal ? 'selected' : ''}>${esc(c.displayName || c.name)}</option>`));
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
  tzSel.append(h(`<option value="" ${!p.tz ? 'selected' : ''}>Device default — ${esc(device.replace(/_/g, ' '))} (${esc(off)})</option>`));
  for (const z of [...new Set(zones)].sort()) {
    tzSel.append(h(`<option value="${esc(z)}" ${p.tz === z ? 'selected' : ''}>${esc(z.replace(/_/g, ' '))}</option>`));
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

  const exp = $('#st-export', el);
  for (const c of store.state.calendars) {
    exp.append(h(`<label><a href="${esc(c.href)}?export" download="${esc(c.name)}.ics">Download “${esc(c.displayName || c.name)}” as .ics</a></label>`));
  }
  for (const b of store.state.books) {
    exp.append(h(`<label><a href="${esc(b.href)}?export" download="contacts.vcf">Download “${esc(b.displayName)}” as .vcf</a></label>`));
  }

  $('#st-import', el).addEventListener('change', async ev => {
    const file = ev.target.files[0];
    if (!file) return;
    const note = $('#st-import-note', el);
    note.textContent = 'Importing…';
    try {
      const text = await file.text();
      const tree = ical.parse(text);
      const events = ical.components(tree, 'VEVENT');
      const cards = ical.components(tree, 'VCARD');
      let done = 0;
      // events grouped by UID (a recurring event and its overrides share one file)
      const byUid = new Map();
      for (const ev2 of events) {
        const uid = ical.propValue(ev2, 'UID') || crypto.randomUUID();
        if (!byUid.has(uid)) byUid.set(uid, []);
        byUid.get(uid).push(ev2);
      }
      for (const [uid, group] of byUid) {
        const lines = ['BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//calweb import//EN'];
        for (const ev2 of group) {
          lines.push('BEGIN:VEVENT');
          for (const prop of ev2.props) {
            const params = Object.entries(prop.params).map(([k, v]) => `;${k}=${v}`).join('');
            lines.push(ical.foldLong(`${prop.name}${params}:${prop.value}`));
          }
          lines.push('END:VEVENT');
        }
        lines.push('END:VCALENDAR');
        await store.putObject(store.eventHref(store.state.prefs.defaultCal, uid),
          lines.join('\r\n') + '\r\n', undefined);
        done++;
      }
      for (const card of cards) {
        const uid = ical.propValue(card, 'UID') || crypto.randomUUID();
        const fn = ical.icalUnescape(ical.propValue(card, 'FN')) || 'Imported contact';
        await store.saveContact({
          uid, fn,
          emails: card.props.filter(x => x.name === 'EMAIL').map(p => ({ value: p.value, type: '' })),
          tels: card.props.filter(x => x.name === 'TEL').map(p => ({ value: p.value, type: '' })),
          org: ical.icalUnescape(ical.propValue(card, 'ORG')),
          preserve: card.props.filter(p => !ical.MANAGED_VCARD.has(p.name)).map(ical.vcardLine),
        });
        done++;
      }
      note.textContent = done ? `Imported ${done} item(s).` : 'Nothing to import in that file.';
      toast(`Imported ${done} item(s).`);
      await Promise.all([store.refreshEvents(), store.loadContacts()]);
    } catch (e) {
      note.textContent = '';
      showError(e);
    } finally {
      ev.target.value = '';
    }
  });
}
