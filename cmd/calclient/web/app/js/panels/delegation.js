// delegation.js — calendar proxy (delegation) via the calendar-proxy-read /
// calendar-proxy-write sub-principals.

import * as dav from '/shared/dav.js';
import { $, el, run, setOut, statusMsg } from '../ui.js';
import { state } from '../session.js';

const { NS, key, esc } = dav;

export default {
  id: 'delegation', title: 'Delegation',
  render(main) {
    main.append(el(`<div>
      <h2>Calendar proxy (delegation)</h2>
      <p class="hint">Delegation = membership of the calendar-proxy-read /
      calendar-proxy-write sub-principals. Needs -principals-readable on the
      server for non-admin browsing.</p>
      <fieldset><legend>Grant (PROPPATCH group-member-set)</legend>
        <label class="chk"><input type="radio" name="dg-kind" value="calendar-proxy-read">read</label>
        <label class="chk"><input type="radio" name="dg-kind" value="calendar-proxy-write" checked>write</label>
        <label class="fld">member principal hrefs (one per line)
          <textarea id="dg-members" rows="2" cols="40">/principals/bob/</textarea></label>
        <button class="primary" id="dg-set">PROPPATCH</button>
        <button id="dg-show">Show group-member-set</button>
        <div class="out" id="dg-out"></div>
      </fieldset>
      <fieldset><legend>Discovery (who am I a proxy for?)</legend>
        <button id="dg-for">PROPFIND calendar-proxy-*-for</button>
        <div class="out" id="dg-for-out"></div>
      </fieldset>
    </div>`));

    const proxyPath = () => state.principal +
      $('input[name="dg-kind"]:checked', main).value + '/';

    run($('#dg-set', main), async () => {
      const hrefs = $('#dg-members', main).value.split('\n').map(s => s.trim()).filter(Boolean)
        .map(h => `<d:href>${esc(h)}</d:href>`).join('');
      const r = await dav.proppatch(proxyPath(), [{ name: 'd:group-member-set', xml: hrefs }]);
      setOut($('#dg-out', main), statusMsg(r, 'see multistatus in drawer'));
    });

    run($('#dg-show', main), async () => {
      const r = await dav.propfind(proxyPath(), ['d:group-member-set'], '0');
      const p = dav.msResponses(r.xml)[0]?.props.get(key(NS.d, 'group-member-set'));
      setOut($('#dg-out', main), `members: <span class="mono">${dav.hrefTexts(p).map(esc).join(', ') || '(none)'}</span>`);
    });

    run($('#dg-for', main), async () => {
      const r = await dav.propfind(state.principal, ['cs:calendar-proxy-read-for', 'cs:calendar-proxy-write-for'], '0');
      const p = dav.msResponses(r.xml)[0]?.props ?? new Map();
      setOut($('#dg-for-out', main), `<table class="list">
        <tr><th>calendar-proxy-read-for</th><td class="mono">${dav.hrefTexts(p.get(key(NS.cs, 'calendar-proxy-read-for'))).map(esc).join('<br>') || '(none)'}</td></tr>
        <tr><th>calendar-proxy-write-for</th><td class="mono">${dav.hrefTexts(p.get(key(NS.cs, 'calendar-proxy-write-for'))).map(esc).join('<br>') || '(none)'}</td></tr>
      </table>`);
    });
  },
};
