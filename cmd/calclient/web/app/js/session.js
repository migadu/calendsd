// session.js — the discovered principal context (home sets, scheduling
// URLs, user addresses) every panel reads.

import * as dav from '/shared/dav.js';
import { $ } from './ui.js';

const { NS, key } = dav;

export const state = {
  principal: '', displayName: '',
  calHome: '', abHome: '', inbox: '', outbox: '',
  addresses: [],
};

// discover walks current-user-principal from the root, then reads the home
// sets, scheduling URLs and addresses off the principal.
export async function discover() {
  const r1 = await dav.propfind('/', ['d:current-user-principal']);
  if (r1.status === 401) throw new Error('unauthorized');
  const ms1 = dav.msResponses(r1.xml);
  state.principal = dav.hrefText(ms1[0]?.props.get(key(NS.d, 'current-user-principal')));
  if (!state.principal) throw new Error('no current-user-principal');

  const r2 = await dav.propfind(state.principal, [
    'd:displayname', 'c:calendar-home-set', 'card:addressbook-home-set',
    'c:schedule-inbox-URL', 'c:schedule-outbox-URL', 'c:calendar-user-address-set',
  ]);
  const p = dav.msResponses(r2.xml)[0]?.props ?? new Map();
  state.displayName = p.get(key(NS.d, 'displayname'))?.textContent ?? '';
  state.calHome = dav.hrefText(p.get(key(NS.c, 'calendar-home-set')));
  state.abHome = dav.hrefText(p.get(key(NS.card, 'addressbook-home-set')));
  state.inbox = dav.hrefText(p.get(key(NS.c, 'schedule-inbox-URL')));
  state.outbox = dav.hrefText(p.get(key(NS.c, 'schedule-outbox-URL')));
  state.addresses = dav.hrefTexts(p.get(key(NS.c, 'calendar-user-address-set')));
  $('#session-status').textContent =
    `${dav.session.user} — ${state.principal}` + (state.displayName ? ` (${state.displayName})` : '');
}

export function primaryEmail() {
  const mailto = state.addresses.find(a => a.startsWith('mailto:'));
  return mailto ? mailto.slice(7) : (state.addresses[0] || '');
}
