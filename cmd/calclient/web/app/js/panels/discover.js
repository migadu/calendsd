// discover.js — principal discovery and the /.well-known/caldav bootstrap.

import * as dav from '/shared/dav.js';
import { $, el, run, setOut, statusMsg } from '../ui.js';
import { state, discover } from '../session.js';

const { esc } = dav;

export default {
  id: 'discover', title: 'Discover',
  render(main) {
    main.append(el(`<div>
      <h2>Discovery</h2>
      <p class="hint">current-user-principal from the root, then home sets,
      scheduling URLs and user addresses from the principal; plus the
      /.well-known/caldav bootstrap redirect (server replies 301 → /).</p>
      <fieldset><legend>Run</legend>
        <button class="primary" id="disc-run">PROPFIND / + principal</button>
        <button id="disc-wk">GET /.well-known/caldav</button>
        <div class="out" id="disc-out"></div>
      </fieldset>
    </div>`));
    run($('#disc-run', main), async () => {
      await discover();
      setOut($('#disc-out', main), `<table class="list">
        <tr><th>current-user-principal</th><td class="mono">${esc(state.principal)}</td></tr>
        <tr><th>displayname</th><td>${esc(state.displayName)}</td></tr>
        <tr><th>calendar-home-set</th><td class="mono">${esc(state.calHome)}</td></tr>
        <tr><th>addressbook-home-set</th><td class="mono">${esc(state.abHome)}</td></tr>
        <tr><th>schedule-inbox-URL</th><td class="mono">${esc(state.inbox)}</td></tr>
        <tr><th>schedule-outbox-URL</th><td class="mono">${esc(state.outbox)}</td></tr>
        <tr><th>calendar-user-address-set</th><td class="mono">${state.addresses.map(esc).join('<br>')}</td></tr>
      </table>`);
    });
    run($('#disc-wk', main), async () => {
      const r = await dav.req('GET', '/.well-known/caldav');
      setOut($('#disc-out', main), statusMsg(r,
        r.redirected ? `redirect followed to ${r.url.replace(location.origin, '')}` : ''));
    });
  },
};
