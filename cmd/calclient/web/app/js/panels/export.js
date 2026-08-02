// export.js — whole-collection export (GET ?export) and RFC 5995
// Add-Member.

import * as dav from '/shared/dav.js';
import * as ical from '/shared/ical.js';
import { $, html, frag, run, setOut, statusMsg, pre, localDT, plusHours } from '../ui.js';
import { DAY } from '/shared/time.js';
import { calSelect, wireCalSelect } from '../collections.js';

export default {
  id: 'export', title: 'Export / Add-Member',
  render(main) {
    main.append(html`<div>
      <h2>Collection export &amp; Add-Member</h2>
      <fieldset><legend>Whole-calendar export (GET ?export)</legend>
        ${calSelect}
        <label class="fld">format<select id="ex-fmt">
          <option value="">iCalendar</option>
          <option value="jcal">jCal (&amp;accept=jcal)</option>
        </select></label>
        <label class="fld">componentType<select id="ex-comp">
          <option value="">(all)</option><option>VEVENT</option><option>VTODO</option>
        </select></label>
        <label class="chk"><input type="checkbox" id="ex-expand">expand</label>
        <label class="fld">start<input id="ex-start" type="datetime-local"></label>
        <label class="fld">end<input id="ex-end" type="datetime-local"></label>
        <button class="primary" id="ex-run">GET export</button>
        <span id="ex-link"></span>
        <div class="out" id="ex-out"></div>
      </fieldset>
      <fieldset><legend>Add-Member (RFC 5995: POST …/;add-member, server assigns the name)</legend>
        <label class="fld">summary<input id="am-sum" value="Added via add-member"></label>
        <button id="am-run">POST ;add-member</button>
        <div class="out" id="am-out"></div>
      </fieldset>
    </div>`);
    $('#ex-start', main).value = localDT(new Date(Date.now() - 30 * DAY));
    $('#ex-end', main).value = localDT(new Date(Date.now() + 60 * DAY));
    const sel = wireCalSelect(main);

    const exportURL = () => {
      const p = new URLSearchParams();
      if ($('#ex-comp', main).value) p.set('componentType', $('#ex-comp', main).value);
      if ($('#ex-expand', main).checked) {
        p.set('expand', '1');
        p.set('start', String(Math.floor(new Date($('#ex-start', main).value).getTime() / 1000)));
        p.set('end', String(Math.floor(new Date($('#ex-end', main).value).getTime() / 1000)));
      }
      if ($('#ex-fmt', main).value) p.set('accept', $('#ex-fmt', main).value);
      const qs = p.toString();
      return sel.value + '?export' + (qs ? '&' + qs : '');
    };

    run($('#ex-run', main), async () => {
      const url = exportURL();
      const r = await dav.req('GET', url);
      $('#ex-link', main).replaceChildren(html`<a href="${url}" download>download</a>`);
      setOut($('#ex-out', main), frag`${statusMsg(r, r.headers.get('Content-Type'))}${pre(r.text)}`);
    });

    run($('#am-run', main), async () => {
      const body = ical.buildEvent({
        uid: ical.newUID(),
        summary: $('#am-sum', main).value,
        start: localDT(plusHours(3)),
        end: localDT(plusHours(4)),
      });
      const r = await dav.req('POST', sel.value.replace(/\/$/, '') + '/;add-member', {
        headers: { 'Content-Type': 'text/calendar; charset=utf-8' }, body,
      });
      setOut($('#am-out', main), statusMsg(r,
        `server assigned: ${r.headers.get('Location') ?? '(no Location header)'}`));
    });
  },
};
