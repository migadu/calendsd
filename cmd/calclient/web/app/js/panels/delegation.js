// delegation.js — calendar proxy (delegation) via the calendar-proxy-read /
// calendar-proxy-write sub-principals.

import * as dav from '/shared/dav.js';
import { $, html, frag, brJoin, run, setOut, statusMsg } from '../ui.js';
import { state } from '../session.js';

const { NS, key, xml } = dav;

export default {
  id: 'delegation', title: 'Delegation',
  render(main) {
    main.append(html`<div>
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
    </div>`);

    const proxyPath = () => state.principal +
      $('input[name="dg-kind"]:checked', main).value + '/';

    run($('#dg-set', main), async () => {
      const hrefs = $('#dg-members', main).value.split('\n').map(s => s.trim()).filter(Boolean)
        .map(h => xml`<d:href>${h}</d:href>`);
      const r = await dav.proppatch(proxyPath(), [{ name: 'd:group-member-set', xml: xml`${hrefs}` }]);
      setOut($('#dg-out', main), statusMsg(r, 'see multistatus in drawer'));
    });

    run($('#dg-show', main), async () => {
      const r = await dav.propfind(proxyPath(), ['d:group-member-set'], '0');
      const p = dav.msResponses(r.xml)[0]?.props.get(key(NS.d, 'group-member-set'));
      setOut($('#dg-out', main), frag`members: <span class="mono">${dav.hrefTexts(p).join(', ') || '(none)'}</span>`);
    });

    run($('#dg-for', main), async () => {
      const r = await dav.propfind(state.principal, ['cs:calendar-proxy-read-for', 'cs:calendar-proxy-write-for'], '0');
      const p = dav.msResponses(r.xml)[0]?.props ?? new Map();
      const list = name => {
        const hrefs = dav.hrefTexts(p.get(key(NS.cs, name)));
        return hrefs.length ? brJoin(hrefs) : '(none)';
      };
      setOut($('#dg-for-out', main), frag`<table class="list">
        <tr><th>calendar-proxy-read-for</th><td class="mono">${list('calendar-proxy-read-for')}</td></tr>
        <tr><th>calendar-proxy-write-for</th><td class="mono">${list('calendar-proxy-write-for')}</td></tr>
      </table>`);
    });
  },
};
