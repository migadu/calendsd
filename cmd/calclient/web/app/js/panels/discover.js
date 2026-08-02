// discover.js — principal discovery and the /.well-known/caldav bootstrap.

import * as dav from '/shared/dav.js';
import { $, html, frag, brJoin, run, setOut, statusMsg } from '../ui.js';
import { state, discover } from '../session.js';

export default {
  id: 'discover', title: 'Discover',
  render(main) {
    main.append(html`<div>
      <h2>Discovery</h2>
      <p class="hint">current-user-principal from the root, then home sets,
      scheduling URLs and user addresses from the principal; plus the
      /.well-known/caldav bootstrap redirect (server replies 301 → /).</p>
      <fieldset><legend>Run</legend>
        <button class="primary" id="disc-run">PROPFIND / + principal</button>
        <button id="disc-wk">GET /.well-known/caldav</button>
        <div class="out" id="disc-out"></div>
      </fieldset>
    </div>`);
    run($('#disc-run', main), async () => {
      await discover();
      setOut($('#disc-out', main), frag`<table class="list">
        <tr><th>current-user-principal</th><td class="mono">${state.principal}</td></tr>
        <tr><th>displayname</th><td>${state.displayName}</td></tr>
        <tr><th>calendar-home-set</th><td class="mono">${state.calHome}</td></tr>
        <tr><th>addressbook-home-set</th><td class="mono">${state.abHome}</td></tr>
        <tr><th>schedule-inbox-URL</th><td class="mono">${state.inbox}</td></tr>
        <tr><th>schedule-outbox-URL</th><td class="mono">${state.outbox}</td></tr>
        <tr><th>calendar-user-address-set</th><td class="mono">${brJoin(state.addresses)}</td></tr>
      </table>`);
    });
    run($('#disc-wk', main), async () => {
      const r = await dav.req('GET', '/.well-known/caldav');
      setOut($('#disc-out', main), statusMsg(r,
        r.redirected ? `redirect followed to ${r.url.replace(location.origin, '')}` : ''));
    });
  },
};
