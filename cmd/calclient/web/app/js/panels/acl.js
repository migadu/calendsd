// acl.js — privilege introspection and principal-property-search.

import * as dav from '/shared/dav.js';
import { $, el, run, setOut, statusMsg, preXML } from '../ui.js';
import { state } from '../session.js';

const { NS, key, esc } = dav;

export default {
  id: 'acl', title: 'ACL',
  render(main) {
    main.append(el(`<div>
      <h2>Privileges &amp; principal search</h2>
      <fieldset><legend>Introspection (PROPFIND acl props)</legend>
        <label class="fld">path<input id="acl-path" size="40"></label>
        <button class="primary" id="acl-run">PROPFIND</button>
        <div class="out" id="acl-out"></div>
      </fieldset>
      <fieldset><legend>principal-property-search (REPORT on /principals/)</legend>
        <label class="fld">displayname contains<input id="acl-match" value="bob"></label>
        <label class="chk"><input type="checkbox" id="acl-all">apply-to-principal-collection-set</label>
        <button id="acl-search">REPORT</button>
        <div class="out" id="acl-search-out"></div>
      </fieldset>
    </div>`));
    $('#acl-path', main).value = state.calHome || '/';

    run($('#acl-run', main), async () => {
      const r = await dav.propfind($('#acl-path', main).value.trim(),
        ['d:current-user-privilege-set', 'd:acl', 'd:supported-privilege-set'], '0');
      const p = dav.msResponses(r.xml)[0]?.props ?? new Map();
      const cups = dav.privilegeNames(p.get(key(NS.d, 'current-user-privilege-set')));
      const aclEl = p.get(key(NS.d, 'acl'));
      const aces = aclEl ? [...aclEl.children].map(ace => {
        const principal = ace.getElementsByTagNameNS(NS.d, 'principal')[0];
        const who = principal?.getElementsByTagNameNS(NS.d, 'href')[0]?.textContent
          ?? principal?.firstElementChild?.localName ?? '?';
        const grants = dav.privilegeNames(ace.getElementsByTagNameNS(NS.d, 'grant')[0]);
        const prot = ace.getElementsByTagNameNS(NS.d, 'protected').length ? ' (protected)' : '';
        return `<tr><td class="mono">${esc(who)}${prot}</td><td class="mono">${grants.map(esc).join(', ')}</td></tr>`;
      }).join('') : '';
      setOut($('#acl-out', main), `
        <p>current-user-privilege-set: <span class="mono">${cups.map(esc).join(', ') || '(none)'}</span></p>
        <table class="list"><tr><th>principal</th><th>granted</th></tr>${aces}</table>
        <details><summary>supported-privilege-set (raw)</summary>${p.get(key(NS.d, 'supported-privilege-set')) ? preXML(new XMLSerializer().serializeToString(p.get(key(NS.d, 'supported-privilege-set')))) : '(none)'}</details>`);
    });

    run($('#acl-search', main), async () => {
      const all = $('#acl-all', main).checked ? '<d:apply-to-principal-collection-set/>' : '';
      const body = dav.xmlDoc('d:principal-property-search', '', `
        <d:property-search><d:prop><d:displayname/></d:prop>
          <d:match>${esc($('#acl-match', main).value)}</d:match></d:property-search>
        <d:prop><d:displayname/><c:calendar-user-address-set/></d:prop>${all}`);
      const r = await dav.report('/principals/', body, '0');
      const rows = dav.msResponses(r.xml).map(x => `<tr>
        <td class="mono">${esc(x.href)}</td>
        <td>${esc(x.props.get(key(NS.d, 'displayname'))?.textContent ?? '')}</td>
        <td class="mono">${dav.hrefTexts(x.props.get(key(NS.c, 'calendar-user-address-set'))).map(esc).join('<br>')}</td></tr>`).join('');
      setOut($('#acl-search-out', main), statusMsg(r, '') +
        `<table class="list"><tr><th>principal</th><th>displayname</th><th>addresses</th></tr>${rows}</table>`);
    });
  },
};
