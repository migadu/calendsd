// session.js — login/logout/restore, principal discovery, and the
// ctag-poll loop. This is the store's top layer: it orchestrates the
// per-area loaders (calendars, invites, sharing, contacts) on bootstrap.

import * as dav from '../dav.js';
import { state, emit, savePrefs, check, DavError } from './core.js';
import { loadCalendars } from './calendars.js';
import { loadInvites } from './invites.js';
import { loadShareNotifications } from './sharing.js';
import { loadContacts } from './contacts.js';
import { loadServerPrefs, wirePrefsSync, resetPrefsSync } from './prefs.js';

const { NS, key } = dav;

export function haveSession() { return !!dav.session.auth; }

export async function login(user, pass, stay) {
  dav.session.login(user, pass);
  try {
    await discover();
  } catch (e) {
    dav.session.logout();
    throw e;
  }
  if (stay) {
    localStorage.setItem('calweb.user', sessionStorage.getItem('calclient.user'));
    localStorage.setItem('calweb.auth', sessionStorage.getItem('calclient.auth'));
  }
  savePrefs({ staySignedIn: !!stay });
  await bootstrap();
}

export function logout() {
  dav.session.logout();
  localStorage.removeItem('calweb.user');
  localStorage.removeItem('calweb.auth');
  resetPrefsSync();
  stopPolling();
  emit('session');
}

// restore restores a saved session (sessionStorage first, then "stay signed
// in" localStorage) and bootstraps. Returns false if there is none / stale.
export async function restore() {
  if (!dav.session.auth && localStorage.getItem('calweb.auth')) {
    sessionStorage.setItem('calclient.user', localStorage.getItem('calweb.user'));
    sessionStorage.setItem('calclient.auth', localStorage.getItem('calweb.auth'));
  }
  if (!dav.session.auth) return false;
  try {
    await discover();
    await bootstrap();
    return true;
  } catch {
    dav.session.logout();
    return false;
  }
}

async function discover() {
  const r1 = check(await dav.propfind('/', ['d:current-user-principal']), [207]);
  state.principal = dav.hrefText(dav.msResponses(r1.xml)[0]?.props.get(key(NS.d, 'current-user-principal')));
  if (!state.principal) throw new DavError({ status: 401 });
  const r2 = check(await dav.propfind(state.principal, [
    'd:displayname', 'c:calendar-home-set', 'card:addressbook-home-set',
    'c:schedule-inbox-URL', 'c:schedule-outbox-URL', 'c:calendar-user-address-set',
  ]), [207]);
  const p = dav.msResponses(r2.xml)[0]?.props ?? new Map();
  state.user = dav.session.user;
  state.displayName = p.get(key(NS.d, 'displayname'))?.textContent ?? dav.session.user;
  state.calHome = dav.hrefText(p.get(key(NS.c, 'calendar-home-set')));
  state.abHome = dav.hrefText(p.get(key(NS.card, 'addressbook-home-set')));
  state.inbox = dav.hrefText(p.get(key(NS.c, 'schedule-inbox-URL')));
  state.outbox = dav.hrefText(p.get(key(NS.c, 'schedule-outbox-URL')));
  state.addresses = dav.hrefTexts(p.get(key(NS.c, 'calendar-user-address-set')));
  emit('session');
}

async function bootstrap() {
  // Mirror pref changes to the server from here on, then pull the account's
  // stored prefs over the local cache before the first render so this device
  // adopts the settings last saved on any device.
  wirePrefsSync();
  await loadServerPrefs();
  await loadCalendars();
  loadInvites().catch(() => {});
  loadShareNotifications().catch(() => {});
  loadContacts().catch(() => {});
  startPolling();
}

// ---- polling -------------------------------------------------------------

let pollTimer = null;
export function startPolling() {
  stopPolling();
  pollTimer = setInterval(() => {
    loadCalendars().catch(() => {});
    loadInvites().catch(() => {});
    loadShareNotifications().catch(() => {});
  }, 30_000);
}
export function stopPolling() {
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = null;
}
