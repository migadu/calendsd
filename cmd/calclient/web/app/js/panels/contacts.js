// contacts.js — CardDAV: address books (extended MKCOL), vCard CRUD,
// addressbook-query/-multiget, and export.

import * as dav from '/shared/dav.js';
import * as ical from '/shared/ical.js';
import { $, html, frag, run, setOut, statusMsg, pre, rowsOr } from '../ui.js';
import { confirmDialog } from '/shared/dialogs.js';
import { state } from '../session.js';
import { bookChildren, listObjects } from '../collections.js';

const { NS, key, xml } = dav;

export default {
  id: 'contacts', title: 'Contacts',
  render(main) {
    main.append(html`<div>
      <h2>CardDAV</h2>
      <fieldset><legend>Addressbooks</legend>
        <button class="primary" id="ct-books">List books</button>
        <label class="fld">new book uri<input id="ct-bname" value="demo-contacts"></label>
        <label class="fld">displayname<input id="ct-bdn" value="Demo contacts"></label>
        <button id="ct-mkcol">Extended MKCOL</button>
        <div class="out" id="ct-books-out"></div>
      </fieldset>
      <fieldset><legend>vCards</legend>
        <label class="fld">addressbook<select id="ct-sel"></select></label>
        <button id="ct-refresh">↻</button>
        <button id="ct-list">List cards</button>
        <label class="fld">FN<input id="ct-fn" value="Bob Example"></label>
        <label class="fld">EMAIL<input id="ct-email" value="bob@example.org"></label>
        <label class="fld">TEL<input id="ct-tel" value="+41 44 123 45 67"></label>
        <button class="primary" id="ct-put">PUT .vcf</button>
        <div class="out" id="ct-cards-out"></div>
      </fieldset>
      <fieldset><legend>Search (addressbook-query text-match) &amp; multiget</legend>
        <label class="fld">property<select id="ct-field"><option>FN</option><option>EMAIL</option><option>NICKNAME</option></select></label>
        <label class="fld">contains<input id="ct-match" value="bob"></label>
        <button id="ct-query">REPORT addressbook-query</button>
        <label class="fld">multiget hrefs (one per line)<textarea id="ct-hrefs" rows="2" cols="50"></textarea></label>
        <button id="ct-mget">REPORT addressbook-multiget</button>
        <div class="out" id="ct-query-out"></div>
      </fieldset>
      <fieldset><legend>Export</legend>
        <button id="ct-export">GET ?export (merged vCard)</button>
        <span id="ct-export-link"></span>
        <div class="out" id="ct-export-out"></div>
      </fieldset>
    </div>`);

    const sel = $('#ct-sel', main);
    const fillBooks = async () => {
      const books = await bookChildren();
      sel.replaceChildren(...books.map(b =>
        html`<option value="${b.href}">${b.displayName || b.name}</option>`));
      return books;
    };
    fillBooks();
    run($('#ct-refresh', main), fillBooks);

    run($('#ct-books', main), async () => {
      const books = await fillBooks();
      const rows = books.map(b => frag`<tr><td class="mono">${b.name}</td>
        <td>${b.displayName}</td><td class="mono">${b.ctag}</td>
        <td><button data-del="${b.href}">DELETE</button></td></tr>`);
      setOut($('#ct-books-out', main), frag`<table class="list">
        <tr><th>uri</th><th>displayname</th><th>getctag</th><th></th></tr>${rows}</table>`);
      for (const b of $('#ct-books-out', main).querySelectorAll('button[data-del]')) run(b, async () => {
        if (!await confirmDialog(`DELETE ${b.dataset.del}?`, { confirmLabel: 'Delete', danger: true })) return;
        await dav.req('DELETE', b.dataset.del);
        $('#ct-books', main).click();
      });
    });

    run($('#ct-mkcol', main), async () => {
      const body = dav.xmlDoc('d:mkcol', '', xml`<d:set><d:prop>
        <d:resourcetype><d:collection/><card:addressbook/></d:resourcetype>
        <d:displayname>${$('#ct-bdn', main).value}</d:displayname>
      </d:prop></d:set>`);
      const r = await dav.req('MKCOL', state.abHome + encodeURIComponent($('#ct-bname', main).value) + '/', {
        headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
      });
      setOut($('#ct-books-out', main), statusMsg(r, r.status === 201 ? 'created' : ''));
    });

    const renderCards = async () => {
      const objs = await listObjects(sel.value);
      const rows = objs.map(o => frag`<tr><td class="mono">${o.name}</td>
        <td class="mono">${o.etag}</td>
        <td><button data-get="${o.href}">GET</button>
            <button data-del="${o.href}">DELETE</button></td></tr>`);
      setOut($('#ct-cards-out', main),
        frag`<table class="list"><tr><th>card</th><th>etag</th><th></th></tr>${rowsOr(rows, 3)}</table><div class="out"></div>`);
      for (const b of $('#ct-cards-out', main).querySelectorAll('button[data-get]')) run(b, async () => {
        const r = await dav.req('GET', b.dataset.get);
        setOut($('#ct-cards-out .out', main), pre(r.text));
      });
      for (const b of $('#ct-cards-out', main).querySelectorAll('button[data-del]')) run(b, async () => {
        if (!await confirmDialog(`DELETE ${b.dataset.del}?`, { confirmLabel: 'Delete', danger: true })) return;
        await dav.req('DELETE', b.dataset.del);
        await renderCards();
      });
    };
    run($('#ct-list', main), renderCards);

    run($('#ct-put', main), async () => {
      const uid = ical.newUID();
      const body = ical.buildVCard({
        uid,
        fn: $('#ct-fn', main).value,
        email: $('#ct-email', main).value.trim(),
        tel: $('#ct-tel', main).value.trim(),
      });
      await dav.req('PUT', sel.value + uid + '.vcf', {
        headers: { 'Content-Type': 'text/vcard; charset=utf-8' }, body,
      });
      await renderCards();
    });

    const renderCardMS = (r, box) => {
      const rows = dav.msResponses(r.xml).map(x => {
        const data = x.props.get(key(NS.card, 'address-data'))?.textContent ?? '';
        return frag`<tr><td class="mono">${x.href}</td></tr><tr><td>${pre(data)}</td></tr>`;
      });
      setOut(box, frag`${statusMsg(r, `${dav.msResponses(r.xml).length} match(es)`)}
        <table class="list">${rows}</table>`);
    };

    run($('#ct-query', main), async () => {
      const body = dav.xmlDoc('card:addressbook-query', '', xml`
        <d:prop><d:getetag/><card:address-data/></d:prop>
        <card:filter test="anyof"><card:prop-filter name="${$('#ct-field', main).value}">
          <card:text-match collation="i;unicode-casemap" match-type="contains">${$('#ct-match', main).value}</card:text-match>
        </card:prop-filter></card:filter>`);
      renderCardMS(await dav.report(sel.value, body), $('#ct-query-out', main));
    });

    run($('#ct-mget', main), async () => {
      const hrefs = $('#ct-hrefs', main).value.split('\n').map(s => s.trim()).filter(Boolean)
        .map(h => xml`<d:href>${h}</d:href>`);
      const body = dav.xmlDoc('card:addressbook-multiget', '',
        xml`<d:prop><d:getetag/><card:address-data/></d:prop>${hrefs}`);
      renderCardMS(await dav.report(sel.value, body), $('#ct-query-out', main));
    });

    run($('#ct-export', main), async () => {
      const url = sel.value + '?export';
      const r = await dav.req('GET', url);
      $('#ct-export-link', main).replaceChildren(html`<a href="${url}" download>download</a>`);
      setOut($('#ct-export-out', main), frag`${statusMsg(r, r.headers.get('Content-Type'))}${pre(r.text)}`);
    });
  },
};
