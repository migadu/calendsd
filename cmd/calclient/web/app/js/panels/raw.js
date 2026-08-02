// raw.js — free-form requests through the same instrumented fetch.

import * as dav from '/shared/dav.js';
import { $, html, frag, run, setOut, statusMsg, pre, preXML } from '../ui.js';

export default {
  id: 'raw', title: 'Raw',
  render(main) {
    main.append(html`<div>
      <h2>Raw console</h2>
      <p class="hint">Free-form requests through the same instrumented fetch —
      anything the panels don't cover.</p>
      <fieldset><legend>Request</legend>
        <label class="fld">method<input id="raw-method" list="raw-methods" value="PROPFIND" size="12"></label>
        <datalist id="raw-methods">
          <option>PROPFIND</option><option>REPORT</option><option>GET</option>
          <option>PUT</option><option>DELETE</option><option>POST</option>
          <option>MKCALENDAR</option><option>MKCOL</option><option>PROPPATCH</option>
          <option>MOVE</option><option>COPY</option><option>OPTIONS</option><option>HEAD</option>
        </datalist>
        <label class="fld">path<input id="raw-path" value="/" size="40"></label>
        <label class="fld">Depth<select id="raw-depth"><option value="">(none)</option><option>0</option><option selected>1</option><option>infinity</option></select></label>
        <label class="fld">Content-Type<input id="raw-ct" value="application/xml; charset=utf-8" size="28"></label>
        <label class="fld">Destination (MOVE/COPY)<input id="raw-dest" size="30"></label>
        <label class="fld">body<textarea id="raw-body" rows="8"></textarea></label>
        <button class="primary" id="raw-send">Send</button>
        <div class="out" id="raw-out"></div>
      </fieldset>
    </div>`);
    $('#raw-body', main).value = dav.propfindBody(['d:resourcetype', 'd:displayname']);

    run($('#raw-send', main), async () => {
      const headers = {};
      if ($('#raw-depth', main).value) headers['Depth'] = $('#raw-depth', main).value;
      if ($('#raw-dest', main).value.trim()) headers['Destination'] = $('#raw-dest', main).value.trim();
      const body = $('#raw-body', main).value;
      if (body) headers['Content-Type'] = $('#raw-ct', main).value;
      const r = await dav.req($('#raw-method', main).value.trim().toUpperCase(),
        $('#raw-path', main).value.trim(), { headers, body: body || null });
      setOut($('#raw-out', main), frag`${statusMsg(r, '')}${r.text ? (r.xml ? preXML(r.text) : pre(r.text)) : ''}`);
    });
  },
};
