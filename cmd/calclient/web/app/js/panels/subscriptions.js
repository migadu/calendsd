// subscriptions.js — webcal subscriptions ({cs}subscribed + {cs}source).

import * as dav from '/shared/dav.js';
import { $, el, run, setOut, statusMsg } from '../ui.js';
import { state } from '../session.js';
import { homeChildren } from '../collections.js';

const { NS, esc } = dav;

export default {
  id: 'subscriptions', title: 'Subscriptions',
  render(main) {
    main.append(el(`<div>
      <h2>Webcal subscriptions</h2>
      <p class="hint">The Postgres storage backend does not store subscriptions
      (sabre PDO parity): expect a 403 "backend does not support subscriptions"
      in the drawer — the request itself is the demonstration.</p>
      <fieldset><legend>Create (MKCALENDAR with {cs}subscribed + {cs}source)</legend>
        <label class="fld">uri name<input id="su-name" value="holidays"></label>
        <label class="fld">displayname<input id="su-dn" value="Holidays feed"></label>
        <label class="fld">source URL<input id="su-src" value="webcal://example.org/holidays.ics" size="36"></label>
        <button class="primary" id="su-create">MKCALENDAR</button>
        <div class="out" id="su-create-out"></div>
      </fieldset>
      <fieldset><legend>List</legend>
        <button id="su-list">List subscriptions</button>
        <div class="out" id="su-out"></div>
      </fieldset>
    </div>`));

    run($('#su-create', main), async () => {
      // xmlns declared inline: the server re-parses the resourcetype/source
      // fragments standalone, so prefixes bound on the root would be lost.
      const body = dav.xmlDoc('c:mkcalendar', '', `<d:set><d:prop>
        <d:resourcetype><d:collection/><subscribed xmlns="${NS.cs}"/></d:resourcetype>
        <d:displayname>${esc($('#su-dn', main).value)}</d:displayname>
        <cs:source><href xmlns="${NS.d}">${esc($('#su-src', main).value.trim())}</href></cs:source>
      </d:prop></d:set>`);
      const r = await dav.req('MKCALENDAR', state.calHome + encodeURIComponent($('#su-name', main).value) + '/', {
        headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
      });
      setOut($('#su-create-out', main), statusMsg(r, r.status === 201 ? 'subscription created' : ''));
    });

    const renderSubs = async () => {
      const subs = (await homeChildren()).filter(c => c.isSubscription);
      const rows = subs.map(s => `<tr><td class="mono">${esc(s.name)}</td>
        <td>${esc(s.displayName)}</td><td class="mono">${esc(s.source)}</td>
        <td><button data-del="${esc(s.href)}">DELETE</button></td></tr>`).join('');
      setOut($('#su-out', main), `<table class="list">
        <tr><th>uri</th><th>displayname</th><th>source</th><th></th></tr>
        ${rows || '<tr><td colspan="4">(none)</td></tr>'}</table>`);
      for (const b of $('#su-out', main).querySelectorAll('button[data-del]')) run(b, async () => {
        if (!confirm(`DELETE ${b.dataset.del}?`)) return;
        await dav.req('DELETE', b.dataset.del);
        await renderSubs();
      });
    };
    run($('#su-list', main), renderSubs);
  },
};
