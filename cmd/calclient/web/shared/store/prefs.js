// prefs.js — server-side preference sync. Preferences are cached in
// localStorage (see core.js) so the app works instantly and offline, and are
// ALSO mirrored to the server as a single JSON dead property on the user's
// calendar-home collection, so the same account carries its settings across
// browsers and devices.
//
// The calendar home is used (not the principal) because it is unambiguously
// owner-writable: CalendarHome grants {DAV:}write, which aggregates
// write-properties, so a PROPPATCH of a dead property there is permitted. The
// property lives in calclient's private namespace, so other DAV clients (and
// sabre) round-trip it untouched.

import * as dav from '../dav.js';
import { state, bus, savePrefs } from './core.js';

const { NS, key } = dav;

// The prefixed property name for PROPPATCH/PROPFIND and the parsed name for
// reading it back out of a multistatus.
const PROP_PREFIXED = 'x:prefs';
const PROP_KEY = key(NS.x, 'prefs');

// Only account-global settings are synced. Device-specific ones (staying
// signed in) stay local — they describe this browser, not the account.
const SYNCED = ['weekStart', 'hour12', 'tz', 'defaultCal', 'defaultView', 'defaultDuration'];

// The JSON last read from / written to the server, so an echo (loading server
// prefs re-emits 'prefs') and no-op saves don't trigger a redundant PROPPATCH.
let lastSynced = null;
let pushTimer = null;

// syncedBlob serializes just the account-global subset of the current prefs.
function syncedBlob() {
  const o = {};
  for (const k of SYNCED) o[k] = state.prefs[k];
  return JSON.stringify(o);
}

// loadServerPrefs pulls the stored prefs blob and merges it over the local
// cache. Best-effort: on any failure (offline, malformed) the local cache
// stands. If the account has no prefs stored yet, it is seeded from this
// device's current settings so sync is live from the first login, not only
// after the next toggle. Call after discovery has set state.calHome.
export async function loadServerPrefs() {
  if (!state.calHome) return;
  let raw;
  try {
    const r = await dav.propfind(state.calHome, [PROP_PREFIXED]);
    raw = dav.msResponses(r.xml)[0]?.props.get(PROP_KEY)?.textContent?.trim();
  } catch {
    lastSynced = syncedBlob(); // offline: don't clobber, don't re-seed
    return;
  }
  if (raw) {
    try { savePrefs(JSON.parse(raw)); } // merge, cache locally, emit('prefs')
    catch { /* malformed blob: keep the local cache */ }
    lastSynced = syncedBlob();
    return;
  }
  lastSynced = null;        // nothing stored yet — seed from this device
  await pushServerPrefs();
}

// pushServerPrefs uploads the current account-global prefs, unless they already
// match what the server has. Best-effort; a failure leaves the local cache
// authoritative and the next change retries.
async function pushServerPrefs() {
  if (!state.calHome) return;
  const json = syncedBlob();
  if (json === lastSynced) return;
  try {
    await dav.proppatch(state.calHome, [{ name: PROP_PREFIXED, value: json }]);
    lastSynced = json;
  } catch { /* retry on the next change */ }
}

// wirePrefsSync mirrors every local pref change up to the server (debounced so a
// flurry of toggles is one write). Idempotent; safe to call on each bootstrap.
let wired = false;
export function wirePrefsSync() {
  if (wired) return;
  wired = true;
  bus.addEventListener('prefs', () => {
    clearTimeout(pushTimer);
    pushTimer = setTimeout(pushServerPrefs, 800);
  });
}

// resetPrefsSync forgets the server baseline on logout, so a different account
// signing in next doesn't suppress its first upload.
export function resetPrefsSync() { lastSynced = null; }
