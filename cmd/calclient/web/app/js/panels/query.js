// query.js — calendar-query (time-range, optional expand) and
// calendar-multiget REPORTs.

import * as dav from '/shared/dav.js';
import * as ical from '/shared/ical.js';
import { $, html, frag, run, setOut, statusMsg, pre, localDT } from '../ui.js';
import { DAY } from '/shared/time.js';
import { calSelect, wireCalSelect } from '../collections.js';

const { NS, key, xml } = dav;

export default {
  id: 'query', title: 'Query',
  render(main) {
    main.append(html`<div>
      <h2>calendar-query / calendar-multiget</h2>
      <fieldset><legend>calendar-query (time-range, optional expand)</legend>
        ${calSelect}
        <label class="fld">start<input id="q-start" type="datetime-local"></label>
        <label class="fld">end<input id="q-end" type="datetime-local"></label>
        <label class="chk"><input type="checkbox" id="q-expand">expand recurrences</label>
        <button class="primary" id="q-run">REPORT calendar-query</button>
        <div class="out" id="q-out"></div>
      </fieldset>
      <fieldset><legend>calendar-multiget</legend>
        <label class="fld">hrefs (one per line)<textarea id="q-hrefs" rows="3" cols="60"></textarea></label>
        <button id="q-mget">REPORT calendar-multiget</button>
        <div class="out" id="q-mget-out"></div>
      </fieldset>
    </div>`);
    $('#q-start', main).value = localDT(new Date(Date.now() - 7 * DAY));
    $('#q-end', main).value = localDT(new Date(Date.now() + 30 * DAY));
    const sel = wireCalSelect(main);

    const renderMS = (r, box) => {
      const rows = dav.msResponses(r.xml).map(x => {
        const data = x.props.get(key(NS.c, 'calendar-data'))?.textContent ?? '';
        const n = ical.components(ical.parse(data), 'VEVENT').length;
        return frag`<tr><td class="mono">${x.href}</td><td>${n} VEVENT(s)</td></tr>
          <tr><td colspan="2">${pre(data)}</td></tr>`;
      });
      setOut(box, frag`${statusMsg(r, `${dav.msResponses(r.xml).length} response(s)`)}
        <table class="list">${rows}</table>`);
    };

    run($('#q-run', main), async () => {
      const s = ical.utc($('#q-start', main).value), e = ical.utc($('#q-end', main).value);
      const expand = $('#q-expand', main).checked && xml`<c:expand start="${s}" end="${e}"/>`;
      const body = dav.xmlDoc('c:calendar-query', '', xml`
        <d:prop><d:getetag/><c:calendar-data>${expand}</c:calendar-data></d:prop>
        <c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT">
          <c:time-range start="${s}" end="${e}"/>
        </c:comp-filter></c:comp-filter></c:filter>`);
      renderMS(await dav.report(sel.value, body), $('#q-out', main));
    });

    run($('#q-mget', main), async () => {
      const hrefs = $('#q-hrefs', main).value.split('\n').map(s => s.trim()).filter(Boolean)
        .map(h => xml`<d:href>${h}</d:href>`);
      const body = dav.xmlDoc('c:calendar-multiget', '',
        xml`<d:prop><d:getetag/><c:calendar-data/></d:prop>${hrefs}`);
      renderMS(await dav.report(sel.value, body), $('#q-mget-out', main));
    });
  },
};
