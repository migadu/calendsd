// sharing.js — calendar sharing: {cs}share POSTs, the notifications
// collection, and the sharee's invite-reply.

import * as dav from '/shared/dav.js';
import { $, el, run, setOut, statusMsg, preXML } from '../ui.js';
import { state } from '../session.js';
import { calSelect, wireCalSelect, listObjects } from '../collections.js';

const { NS, esc } = dav;

export default {
  id: 'sharing', title: 'Sharing',
  render(main) {
    main.append(el(`<div>
      <h2>Calendar sharing</h2>
      <p class="hint">The Postgres storage backend does not implement sharing,
      notifications, or invite replies (sabre PDO parity) — these requests
      demonstrate the protocol and surface the server's honest 403/501/404 in
      the drawer. Mock-backend test servers accept them.</p>
      <fieldset><legend>Share / unshare (POST {cs}share to the calendar)</legend>
        ${calSelect}
        <label class="fld">sharee href<input id="sh-href" value="mailto:bob@example.org" size="30"></label>
        <label class="fld">summary<input id="sh-sum" value="shared from calclient"></label>
        <label class="chk"><input type="checkbox" id="sh-rw" checked>read-write</label>
        <button class="primary" id="sh-share">Share (set)</button>
        <button id="sh-unshare">Unshare (remove)</button>
        <div class="out" id="sh-out"></div>
      </fieldset>
      <fieldset><legend>Notifications collection</legend>
        <button id="sh-notif">List /calendars/{u}/notifications/</button>
        <div class="out" id="sh-notif-out"></div>
      </fieldset>
      <fieldset><legend>Invite reply (POST {cs}invite-reply to the calendar home — log in as the sharee)</legend>
        <label class="fld">hosturl (shared calendar href)<input id="sh-host" size="40"></label>
        <label class="fld">in-reply-to (uid from the notification)<input id="sh-irt" size="30"></label>
        <label class="chk"><input type="checkbox" id="sh-accept" checked>accept</label>
        <button id="sh-reply">POST invite-reply</button>
        <div class="out" id="sh-reply-out"></div>
      </fieldset>
    </div>`));
    const sel = wireCalSelect(main);

    run($('#sh-share', main), async () => {
      const rw = $('#sh-rw', main).checked ? '<cs:read-write/>' : '';
      const body = dav.xmlDoc('cs:share', '', `<cs:set>
        <d:href>${esc($('#sh-href', main).value.trim())}</d:href>
        <cs:summary>${esc($('#sh-sum', main).value)}</cs:summary>${rw}
      </cs:set>`);
      const r = await dav.req('POST', sel.value, {
        headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
      });
      setOut($('#sh-out', main), statusMsg(r, r.ok ? 'sharee invited — a notification lands for them' : ''));
    });

    run($('#sh-unshare', main), async () => {
      const body = dav.xmlDoc('cs:share', '',
        `<cs:remove><d:href>${esc($('#sh-href', main).value.trim())}</d:href></cs:remove>`);
      const r = await dav.req('POST', sel.value, {
        headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
      });
      setOut($('#sh-out', main), statusMsg(r, r.ok ? 'access removed' : ''));
    });

    run($('#sh-notif', main), async () => {
      const base = state.calHome + 'notifications/';
      const objs = await listObjects(base);
      const rows = objs.map(o => `<tr><td class="mono">${esc(o.name)}</td>
        <td><button data-get="${esc(o.href)}">GET</button></td></tr>`).join('');
      setOut($('#sh-notif-out', main),
        `<table class="list"><tr><th>notification</th><th></th></tr>${rows || '<tr><td colspan="2">(empty)</td></tr>'}</table><div class="out"></div>`);
      for (const b of $('#sh-notif-out', main).querySelectorAll('button[data-get]')) run(b, async () => {
        const r = await dav.req('GET', b.dataset.get);
        setOut($('#sh-notif-out .out', main), preXML(r.text));
        const uid = r.xml?.getElementsByTagNameNS(NS.cs, 'uid')[0]?.textContent;
        const host = r.xml ? dav.hrefText(r.xml.getElementsByTagNameNS(NS.cs, 'hosturl')[0]) : '';
        if (uid) $('#sh-irt', main).value = uid;
        if (host) $('#sh-host', main).value = host;
      });
    });

    run($('#sh-reply', main), async () => {
      const verdict = $('#sh-accept', main).checked ? '<cs:invite-accepted/>' : '<cs:invite-declined/>';
      const body = dav.xmlDoc('cs:invite-reply', '', `
        <cs:hosturl><d:href>${esc($('#sh-host', main).value.trim())}</d:href></cs:hosturl>
        <cs:in-reply-to>${esc($('#sh-irt', main).value.trim())}</cs:in-reply-to>${verdict}`);
      const r = await dav.req('POST', state.calHome, {
        headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
      });
      setOut($('#sh-reply-out', main), statusMsg(r, r.ok ? 'reply delivered to the sharer' : ''));
    });
  },
};
