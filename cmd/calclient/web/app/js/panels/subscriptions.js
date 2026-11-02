// subscriptions.js — webcal subscriptions ({cs}subscribed + {cs}source).

import * as dav from '/shared/dav.js';
import { $, html, frag, run, setOut, statusMsg, rowsOr } from '../ui.js';
import { confirmDialog } from '/shared/dialogs.js';
import { state } from '../session.js';
import { homeChildren } from '../collections.js';

const { NS, xml } = dav;

export default {
  id: 'subscriptions', title: 'Subscriptions',
  render(main) {
    main.append(html`<div>
      <h2>Webcal subscriptions</h2>
      <p class="hint">The Postgres storage backend does not store subscriptions:
      expect a 403 "backend does not support subscriptions"
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
    </div>`);

    run($('#su-create', main), async () => {
      // xmlns declared inline: the server re-parses the resourcetype/source
      // fragments standalone, so prefixes bound on the root would be lost.
      const body = dav.xmlDoc('c:mkcalendar', '', xml`<d:set><d:prop>
        <d:resourcetype><d:collection/><subscribed xmlns="${NS.cs}"/></d:resourcetype>
        <d:displayname>${$('#su-dn', main).value}</d:displayname>
        <cs:source><href xmlns="${NS.d}">${$('#su-src', main).value.trim()}</href></cs:source>
      </d:prop></d:set>`);
      const r = await dav.req('MKCALENDAR', state.calHome + encodeURIComponent($('#su-name', main).value) + '/', {
        headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
      });
      setOut($('#su-create-out', main), statusMsg(r, r.status === 201 ? 'subscription created' : ''));
    });

    const renderSubs = async () => {
      const subs = (await homeChildren()).filter(c => c.isSubscription);
      const rows = subs.map(s => frag`<tr><td class="mono">${s.name}</td>
        <td>${s.displayName}</td><td class="mono">${s.source}</td>
        <td><button data-del="${s.href}">DELETE</button></td></tr>`);
      setOut($('#su-out', main), frag`<table class="list">
        <tr><th>uri</th><th>displayname</th><th>source</th><th></th></tr>
        ${rowsOr(rows, 4, '(none)')}</table>`);
      for (const b of $('#su-out', main).querySelectorAll('button[data-del]')) run(b, async () => {
        if (!await confirmDialog(`DELETE ${b.dataset.del}?`, { confirmLabel: 'Delete', danger: true })) return;
        await dav.req('DELETE', b.dataset.del);
        await renderSubs();
      });
    };
    run($('#su-list', main), renderSubs);
  },
};
