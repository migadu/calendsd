// import.js — importing events and contacts from an uploaded .ics/.vcf file, as
// a single cancelable background job whose progress lives in the store
// (state.importJob). Because the job isn't tied to the Settings DOM, it keeps
// running when you navigate away, and the view can re-attach to its live
// progress when you come back. Each step emits 'import' so the view updates.

import * as ical from '../ical.js';
import { state, emit } from './core.js';
import { putObject } from './objects.js';
import { eventHref, refreshEvents } from './events.js';
import { loadContacts, putContact, normalizeUID } from './contacts.js';

// The AbortController for the running job, so cancelImport can stop the loop
// between items. Module-level (not in state) since it isn't serializable.
let controller = null;

export function importRunning() {
  return state.importJob?.status === 'running';
}

// cancelImport stops the in-flight job after the current item finishes; already
// written items stay. A no-op when nothing is running.
export function cancelImport() {
  if (controller) controller.abort();
}

// startImport parses the file and writes its events/contacts, updating
// state.importJob as it goes. Resolves when done (or cancelled/failed); the
// caller reads state.importJob for the outcome. Rejects only on a parse/read
// error, with state.importJob.status === 'error'.
export async function startImport(file) {
  if (importRunning()) throw new Error('An import is already running.');
  controller = new AbortController();
  const { signal } = controller;
  const job = state.importJob = {
    status: 'running', name: file.name,
    done: 0, total: 0, added: 0, skipped: 0, failed: 0, error: '',
  };
  const tick = () => { job.done++; emit('import'); };
  emit('import');

  try {
    const text = await file.text();
    const tree = ical.parse(text);
    const events = ical.components(tree, 'VEVENT');
    const cards = ical.components(tree, 'VCARD');
    // Events grouped by UID (a recurring event and its overrides share one file).
    const byUid = new Map();
    for (const ev of events) {
      const uid = ical.propValue(ev, 'UID') || crypto.randomUUID();
      if (!byUid.has(uid)) byUid.set(uid, []);
      byUid.get(uid).push(ev);
    }
    job.total = byUid.size + cards.length;
    if (!job.total) { job.status = 'empty'; emit('import'); return job; }
    emit('import');

    // Know what's already here so re-importing the same file skips existing
    // contacts (matched by UID) instead of failing on the create-only PUT.
    await loadContacts();
    const existing = new Set(state.contacts.map(c => normalizeUID(c.uid)));

    for (const [uid, group] of byUid) {
      if (signal.aborted) break;
      try {
        const lines = ['BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//calweb import//EN'];
        for (const ev of group) {
          lines.push('BEGIN:VEVENT');
          for (const prop of ev.props) {
            const params = Object.entries(prop.params).map(([k, v]) => `;${k}=${v}`).join('');
            lines.push(ical.foldLong(`${prop.name}${params}:${prop.value}`));
          }
          lines.push('END:VEVENT');
        }
        lines.push('END:VCALENDAR');
        await putObject(eventHref(state.prefs.defaultCal, uid), lines.join('\r\n') + '\r\n', undefined);
        job.added++;
      } catch { job.failed++; }
      tick();
    }
    for (const card of cards) {
      if (signal.aborted) break;
      const uid = ical.propValue(card, 'UID') || crypto.randomUUID();
      if (existing.has(normalizeUID(uid))) { job.skipped++; tick(); continue; }
      try {
        const fn = ical.icalUnescape(ical.propValue(card, 'FN')) || 'Imported contact';
        await putContact({
          uid, fn,
          emails: card.props.filter(x => x.name === 'EMAIL').map(p => ({ value: p.value, type: '' })),
          tels: card.props.filter(x => x.name === 'TEL').map(p => ({ value: p.value, type: '' })),
          org: ical.icalUnescape(ical.propValue(card, 'ORG')),
          preserve: card.props.filter(p => !ical.MANAGED_VCARD.has(p.name)).map(ical.vcardLine),
        });
        existing.add(normalizeUID(uid));
        job.added++;
      } catch { job.failed++; }
      tick();
    }

    job.status = signal.aborted ? 'cancelled' : 'done';
    // One refresh reflects everything written, instead of one per item.
    await Promise.all([refreshEvents(), loadContacts()]);
    emit('import');
    return job;
  } catch (e) {
    job.status = 'error';
    job.error = e.message || String(e);
    emit('import');
    throw e;
  } finally {
    controller = null;
  }
}
