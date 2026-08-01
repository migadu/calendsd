// sync.js — RFC 6578 incremental sync (sync-collection) and the getctag
// fast-path.

import * as dav from '/shared/dav.js';
import { $, el, run, setOut, statusMsg } from '../ui.js';
import { calSelect, wireCalSelect } from '../collections.js';

const { NS, key, esc } = dav;

export default {
  id: 'sync', title: 'Sync',
  render(main) {
    main.append(el(`<div>
      <h2>Incremental sync (RFC 6578)</h2>
      <fieldset><legend>sync-collection</legend>
        ${calSelect}
        <label class="fld">sync-token (empty = initial)<input id="sy-token" size="50"></label>
        <button class="primary" id="sy-run">REPORT sync-collection</button>
        <div class="out" id="sy-out"></div>
      </fieldset>
      <fieldset><legend>getctag fast-path</legend>
        <button id="sy-ctag">PROPFIND getctag + sync-token</button>
        <div class="out" id="sy-ctag-out"></div>
      </fieldset>
    </div>`));
    const sel = wireCalSelect(main);

    run($('#sy-run', main), async () => {
      const body = dav.xmlDoc('d:sync-collection', '', `
        <d:sync-token>${esc($('#sy-token', main).value.trim())}</d:sync-token>
        <d:sync-level>1</d:sync-level>
        <d:prop><d:getetag/></d:prop>`);
      const r = await dav.report(sel.value, body, '0');
      const token = dav.syncToken(r.xml);
      const rows = dav.msResponses(r.xml).map(x => `<tr>
        <td class="mono">${esc(x.href)}</td>
        <td class="mono">${esc(x.status || x.props.get(key(NS.d, 'getetag'))?.textContent || '')}</td></tr>`).join('');
      setOut($('#sy-out', main), statusMsg(r, '') +
        `<table class="list"><tr><th>href</th><th>etag / status</th></tr>${rows}</table>
         <p>new token: <span class="mono">${esc(token)}</span> (kept for the next delta)</p>`);
      if (token) $('#sy-token', main).value = token;
    });

    run($('#sy-ctag', main), async () => {
      const r = await dav.propfind(sel.value, ['cs:getctag', 'd:sync-token'], '0');
      const p = dav.msResponses(r.xml)[0]?.props ?? new Map();
      setOut($('#sy-ctag-out', main), `getctag = <span class="mono">${esc(p.get(key(NS.cs, 'getctag'))?.textContent ?? '')}</span>,
        sync-token = <span class="mono">${esc(p.get(key(NS.d, 'sync-token'))?.textContent ?? '')}</span>`);
    });
  },
};
