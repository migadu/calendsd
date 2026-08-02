// calendars.js — calendar listing, MKCALENDAR, and PROPPATCH of
// color/description.

import * as dav from '/shared/dav.js';
import { $, html, frag, run, setOut, statusMsg } from '../ui.js';
import { confirmDialog } from '/shared/dialogs.js';
import { state } from '../session.js';
import { homeChildren, calSelect, wireCalSelect } from '../collections.js';

const { NS, key, xml } = dav;

export default {
  id: 'calendars', title: 'Calendars',
  render(main) {
    main.append(html`<div>
      <h2>Calendars</h2>
      <fieldset><legend>List (PROPFIND depth 1 on the calendar home)</legend>
        <button class="primary" id="cal-list">List calendars</button>
        <div class="out" id="cal-out"></div>
      </fieldset>
      <fieldset><legend>Create (MKCALENDAR)</legend>
        <label class="fld">uri name<input id="cal-name" value="demo"></label>
        <label class="fld">displayname<input id="cal-dn" value="Demo calendar"></label>
        <label class="chk"><input type="checkbox" id="cal-ve" checked>VEVENT</label>
        <label class="chk"><input type="checkbox" id="cal-vt" checked>VTODO</label>
        <button id="cal-create">MKCALENDAR</button>
        <div class="out" id="cal-create-out"></div>
      </fieldset>
      <fieldset><legend>Edit props (PROPPATCH color / description)</legend>
        ${calSelect}
        <label class="fld">calendar-color<input id="cal-color" value="#ff5733ff"></label>
        <label class="fld">calendar-description<input id="cal-desc" value="edited by calclient"></label>
        <button id="cal-patch">PROPPATCH</button>
        <button id="cal-verify">Re-PROPFIND</button>
        <div class="out" id="cal-patch-out"></div>
      </fieldset>
    </div>`);

    const renderList = async () => {
      const cals = await homeChildren();
      const rows = cals.filter(c => c.isCalendar).map(c => frag`<tr>
        <td class="mono">${c.name}</td><td>${c.displayName}</td>
        <td class="mono">${c.color}</td><td>${c.description}</td>
        <td class="mono">${c.components.join(',')}</td>
        <td class="mono">${c.ctag}</td>
        <td><button data-del="${c.href}">DELETE</button></td></tr>`);
      setOut($('#cal-out', main), frag`<table class="list">
        <tr><th>uri</th><th>displayname</th><th>color</th><th>description</th><th>components</th><th>getctag</th><th></th></tr>
        ${rows}</table>`);
      for (const b of $('#cal-out', main).querySelectorAll('button[data-del]')) {
        run(b, async () => {
          if (!await confirmDialog(`DELETE ${b.dataset.del}?`, { confirmLabel: 'Delete', danger: true })) return;
          await dav.req('DELETE', b.dataset.del);
          await renderList();
        });
      }
    };
    run($('#cal-list', main), renderList);

    run($('#cal-create', main), async () => {
      const comps = [$('#cal-ve', main).checked && 'VEVENT', $('#cal-vt', main).checked && 'VTODO']
        .filter(Boolean).map(c => xml`<c:comp name="${c}"/>`);
      const body = dav.xmlDoc('c:mkcalendar', '', xml`<d:set><d:prop>
        <d:displayname>${$('#cal-dn', main).value}</d:displayname>
        <c:supported-calendar-component-set>${comps}</c:supported-calendar-component-set>
      </d:prop></d:set>`);
      const r = await dav.req('MKCALENDAR', state.calHome + encodeURIComponent($('#cal-name', main).value) + '/', {
        headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
      });
      setOut($('#cal-create-out', main), statusMsg(r, r.status === 201 ? 'created' : ''));
    });

    const sel = wireCalSelect(main);
    run($('#cal-patch', main), async () => {
      const r = await dav.proppatch(sel.value, [
        { name: 'a:calendar-color', value: $('#cal-color', main).value },
        { name: 'c:calendar-description', value: $('#cal-desc', main).value },
      ]);
      setOut($('#cal-patch-out', main), statusMsg(r, 'see multistatus in drawer'));
    });
    run($('#cal-verify', main), async () => {
      const r = await dav.propfind(sel.value, ['a:calendar-color', 'c:calendar-description'], '0');
      const p = dav.msResponses(r.xml)[0]?.props ?? new Map();
      setOut($('#cal-patch-out', main), frag`color = <span class="mono">${p.get(key(NS.a, 'calendar-color'))?.textContent ?? '(unset)'}</span>,
        description = <span class="mono">${p.get(key(NS.c, 'calendar-description'))?.textContent ?? '(unset)'}</span>`);
    });
  },
};
