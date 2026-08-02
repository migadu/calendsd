// push-sync.js — WebDAV-Push wiring for the calweb app. When the user turns on
// "Instant updates", the browser subscribes to Web Push for each of their
// event calendars; an incoming push then refreshes the calendars and events
// right away instead of waiting for the 30-second poll.

import * as store from '/shared/store.js';
import * as push from '/shared/push.js';
import { toast } from './ui.js';
import { flashInstances } from './grid.js';

const PREF_KEY = 'calclient.push.enabled';
const SW_PATH = '/cal/sw.js';
const SW_SCOPE = '/cal/';

export function pushSupported() { return push.supported(); }
export function pushEnabled() { return localStorage.getItem(PREF_KEY) === '1'; }

// snapshotEtags records the ETag of every visible occurrence, so that after a
// push-driven refresh we can tell which events actually changed.
function snapshotEtags() {
  const m = new Map();
  for (const i of store.visibleInstances()) m.set(store.instanceKey(i), i.etag);
  return m;
}

// flashChanged highlights the events whose ETag changed (or that appeared) since
// the given snapshot, so a background push produces a visible cue instead of a
// silent redraw. When the change is off-screen there is nothing to flash, so a
// toast confirms something updated.
function flashChanged(before) {
  const changed = [];
  for (const i of store.visibleInstances()) {
    const k = store.instanceKey(i);
    if (before.get(k) !== i.etag) changed.push(k);
  }
  if (!changed.length) return;
  if (flashInstances(changed) === 0) toast('Calendar updated.');
}

// A push means "something changed"; debounce so a burst of notifications (a
// bulk change across calendars) triggers a single refresh. After the refresh
// redraws the grid, flash whatever changed so the update is visible.
let refreshTimer = null;
function scheduleRefresh() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(async () => {
    store.loadCalendars().catch(() => {});
    store.loadInvites().catch(() => {});
    const before = snapshotEtags();
    try {
      await store.refreshEvents();
      flashChanged(before);
    } catch { /* transient sync error: leave the current view untouched */ }
  }, 400);
}

let wired = false;
export function wirePushRefresh() {
  if (wired || !push.supported()) return;
  wired = true;
  push.onMessage(() => scheduleRefresh());
}

// syncSubscriptions reconciles the browser's registrations with the pref and
// the current set of event calendars. Idempotent: it only registers what is
// missing, so it is cheap to call on every calendar refresh.
export async function syncSubscriptions() {
  if (!push.supported()) return;
  if (pushEnabled()) {
    for (const cal of store.eventCalendars()) {
      if (!push.isRegistered(cal.href)) {
        try { await push.registerCollection(cal.href, SW_PATH, SW_SCOPE); }
        catch (e) { console.warn('push registration failed for', cal.href, e); }
      }
    }
  } else {
    for (const path of push.registeredPaths()) {
      await push.unregisterCollection(path).catch(() => {});
    }
  }
}

// setPushEnabled flips the pref and applies it (subscribing or tearing down).
export async function setPushEnabled(on) {
  localStorage.setItem(PREF_KEY, on ? '1' : '0');
  await syncSubscriptions();
}
