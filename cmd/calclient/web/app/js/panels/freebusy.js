// freebusy.js — the free-busy-query REPORT on a calendar.

import * as dav from '/shared/dav.js';
import * as ical from '/shared/ical.js';
import { $, el, run, setOut, statusMsg, pre, localDT } from '../ui.js';
import { calSelect, wireCalSelect } from '../collections.js';

export default {
  id: 'freebusy', title: 'Free/Busy',
  render(main) {
    main.append(el(`<div>
      <h2>free-busy-query</h2>
      <fieldset><legend>REPORT free-busy-query on a calendar</legend>
        ${calSelect}
        <label class="fld">start<input id="fb-start" type="datetime-local"></label>
        <label class="fld">end<input id="fb-end" type="datetime-local"></label>
        <button class="primary" id="fb-run">REPORT</button>
        <div class="out" id="fb-out"></div>
      </fieldset>
    </div>`));
    $('#fb-start', main).value = localDT(new Date(Date.now() - 864e5));
    $('#fb-end', main).value = localDT(new Date(Date.now() + 7 * 864e5));
    const sel = wireCalSelect(main);
    run($('#fb-run', main), async () => {
      const body = dav.xmlDoc('c:free-busy-query', '',
        `<c:time-range start="${ical.utc($('#fb-start', main).value)}" end="${ical.utc($('#fb-end', main).value)}"/>`);
      const r = await dav.report(sel.value, body, '0');
      setOut($('#fb-out', main), statusMsg(r, r.headers.get('Content-Type')) + pre(r.text));
    });
  },
};
