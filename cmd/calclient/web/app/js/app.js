// app.js — session/discovery, the tab router, and one panel per feature
// area. Every panel funnels its requests through dav.req, so the raw
// exchange drawer doubles as a protocol explorer.

import * as dav from '/shared/dav.js';
import * as ical from '/shared/ical.js';

const { NS, key, esc } = dav;

const state = {
  principal: '', displayName: '',
  calHome: '', abHome: '', inbox: '', outbox: '',
  addresses: [],
};

// ---- tiny DOM helpers ----------------------------------------------------

const $ = (sel, root = document) => root.querySelector(sel);

function el(html) {
  const t = document.createElement('template');
  t.innerHTML = html.trim();
  return t.content.firstElementChild;
}

function run(btn, fn) {
  btn.addEventListener('click', async () => {
    btn.disabled = true;
    try { await fn(); } catch (e) { console.error(e); } finally { btn.disabled = false; }
  });
}

function setOut(box, html) { box.innerHTML = html; }

function statusMsg(res, okText) {
  const cls = res.ok || res.status === 207 ? 'msg-ok' : 'msg-err';
  return `<span class="${cls}">HTTP ${res.status}</span> ${esc(okText ?? '')}`;
}

function pre(text) { return `<pre>${esc(text)}</pre>`; }
function preXML(text) { return pre(dav.prettyXML(text)); }

function lastSegment(href) {
  return decodeURIComponent(href.replace(/\/+$/, '').split('/').pop() || '/');
}

// datetime-local <-> iCal helpers
function localDT(d) {
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}
function plusHours(h) { const d = new Date(); d.setHours(d.getHours() + h, 0, 0, 0); return d; }
function icalToLocal(v) {
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})(Z?)$/.exec(v || '');
  if (!m) return '';
  const iso = `${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}${m[7]}`;
  return localDT(new Date(iso));
}

function primaryEmail() {
  const mailto = state.addresses.find(a => a.startsWith('mailto:'));
  return mailto ? mailto.slice(7) : (state.addresses[0] || '');
}

// ---- raw exchange drawer -------------------------------------------------

dav.setOnExchange(({ method, path, reqHeaders, reqBody, res, error }) => {
  const box = $('#exchange');
  const reqHdrs = Object.entries(reqHeaders).map(([k, v]) => `${k}: ${v}`).join('\n');
  let html = `<div class="req-line">${esc(method)} ${esc(path)}</div>`;
  html += `<details open><summary>request headers</summary>${pre(reqHdrs || '(none)')}</details>`;
  if (reqBody) html += `<details open><summary>request body</summary>${pre(String(reqBody))}</details>`;
  if (error) {
    html += `<div class="status-err">${esc(error)}</div>`;
  } else {
    const cls = res.status < 400 ? 'status-ok' : 'status-err';
    let line = `HTTP ${res.status}`;
    if (res.redirected) line += ` (redirected → ${res.url.replace(location.origin, '')})`;
    html += `<div class="req-line ${cls}">${esc(line)}</div>`;
    const respHdrs = [...res.headers.entries()].map(([k, v]) => `${k}: ${v}`).join('\n');
    html += `<details open><summary>response headers</summary>${pre(respHdrs)}</details>`;
    if (res.text) {
      const body = res.xml ? dav.prettyXML(res.text) : res.text;
      html += `<details open><summary>response body</summary>${pre(body)}</details>`;
    }
  }
  box.innerHTML = html;
});

// ---- session + discovery -------------------------------------------------

async function discover() {
  const r1 = await dav.propfind('/', ['d:current-user-principal']);
  if (r1.status === 401) throw new Error('unauthorized');
  const ms1 = dav.msResponses(r1.xml);
  state.principal = dav.hrefText(ms1[0]?.props.get(key(NS.d, 'current-user-principal')));
  if (!state.principal) throw new Error('no current-user-principal');

  const r2 = await dav.propfind(state.principal, [
    'd:displayname', 'c:calendar-home-set', 'card:addressbook-home-set',
    'c:schedule-inbox-URL', 'c:schedule-outbox-URL', 'c:calendar-user-address-set',
  ]);
  const p = dav.msResponses(r2.xml)[0]?.props ?? new Map();
  state.displayName = p.get(key(NS.d, 'displayname'))?.textContent ?? '';
  state.calHome = dav.hrefText(p.get(key(NS.c, 'calendar-home-set')));
  state.abHome = dav.hrefText(p.get(key(NS.card, 'addressbook-home-set')));
  state.inbox = dav.hrefText(p.get(key(NS.c, 'schedule-inbox-URL')));
  state.outbox = dav.hrefText(p.get(key(NS.c, 'schedule-outbox-URL')));
  state.addresses = dav.hrefTexts(p.get(key(NS.c, 'calendar-user-address-set')));
  $('#session-status').textContent =
    `${dav.session.user} — ${state.principal}` + (state.displayName ? ` (${state.displayName})` : '');
}

$('#login-form').addEventListener('submit', async e => {
  e.preventDefault();
  dav.session.login($('#login-user').value.trim(), $('#login-pass').value);
  try {
    await discover();
    $('#login-btn').textContent = 'Re-login';
    activate(currentPanel || 'discover');
  } catch (err) {
    dav.session.logout();
    $('#session-status').textContent = `login failed (${err.message})`;
  }
});

// ---- collection listing helpers ------------------------------------------

async function homeChildren() {
  const r = await dav.propfind(state.calHome, [
    'd:resourcetype', 'd:displayname', 'a:calendar-color', 'c:calendar-description',
    'c:supported-calendar-component-set', 'cs:getctag', 'cs:source', 'd:sync-token',
  ], '1');
  return dav.msResponses(r.xml).filter(x => x.href !== state.calHome).map(x => {
    const rt = dav.resourceTypes(x.props.get(key(NS.d, 'resourcetype')));
    return {
      href: x.href,
      name: lastSegment(x.href),
      displayName: x.props.get(key(NS.d, 'displayname'))?.textContent ?? '',
      color: x.props.get(key(NS.a, 'calendar-color'))?.textContent ?? '',
      description: x.props.get(key(NS.c, 'calendar-description'))?.textContent ?? '',
      ctag: x.props.get(key(NS.cs, 'getctag'))?.textContent ?? '',
      syncToken: x.props.get(key(NS.d, 'sync-token'))?.textContent ?? '',
      source: dav.hrefText(x.props.get(key(NS.cs, 'source'))),
      components: [...(x.props.get(key(NS.c, 'supported-calendar-component-set'))?.children ?? [])]
        .map(c => c.getAttribute('name')).filter(Boolean),
      isCalendar: rt.includes(key(NS.c, 'calendar')),
      isSubscription: rt.includes(key(NS.cs, 'subscribed')),
    };
  });
}

async function fillCalendarSelect(sel) {
  const cals = (await homeChildren()).filter(c => c.isCalendar);
  sel.innerHTML = cals.map(c =>
    `<option value="${esc(c.href)}">${esc(c.displayName || c.name)}</option>`).join('');
  return cals;
}

async function bookChildren() {
  const r = await dav.propfind(state.abHome, ['d:resourcetype', 'd:displayname', 'cs:getctag'], '1');
  return dav.msResponses(r.xml).filter(x => x.href !== state.abHome).map(x => ({
    href: x.href,
    name: lastSegment(x.href),
    displayName: x.props.get(key(NS.d, 'displayname'))?.textContent ?? '',
    ctag: x.props.get(key(NS.cs, 'getctag'))?.textContent ?? '',
    isBook: dav.resourceTypes(x.props.get(key(NS.d, 'resourcetype'))).includes(key(NS.card, 'addressbook')),
  })).filter(x => x.isBook);
}

async function listObjects(collection) {
  const r = await dav.propfind(collection, ['d:getetag', 'd:getcontenttype'], '1');
  return dav.msResponses(r.xml).filter(x => x.href !== collection && !x.href.endsWith('/')).map(x => ({
    href: x.href,
    name: lastSegment(x.href),
    etag: x.props.get(key(NS.d, 'getetag'))?.textContent ?? '',
    type: x.props.get(key(NS.d, 'getcontenttype'))?.textContent ?? '',
  }));
}

const calSelect = `<label class="fld">calendar<select class="cal-sel"></select></label>
  <button class="cal-refresh" type="button">↻</button>`;

function wireCalSelect(root, onReady) {
  const sel = $('.cal-sel', root);
  run($('.cal-refresh', root), () => fillCalendarSelect(sel));
  fillCalendarSelect(sel).then(onReady ?? (() => {}));
  return sel;
}

// ---- panels --------------------------------------------------------------

const PANELS = [];
function panel(id, title, render) { PANELS.push({ id, title, render }); }

// -- Discover --------------------------------------------------------------

panel('discover', 'Discover', main => {
  main.append(el(`<div>
    <h2>Discovery</h2>
    <p class="hint">current-user-principal from the root, then home sets,
    scheduling URLs and user addresses from the principal; plus the
    /.well-known/caldav bootstrap redirect (server replies 301 → /).</p>
    <fieldset><legend>Run</legend>
      <button class="primary" id="disc-run">PROPFIND / + principal</button>
      <button id="disc-wk">GET /.well-known/caldav</button>
      <div class="out" id="disc-out"></div>
    </fieldset>
  </div>`));
  run($('#disc-run', main), async () => {
    await discover();
    setOut($('#disc-out', main), `<table class="list">
      <tr><th>current-user-principal</th><td class="mono">${esc(state.principal)}</td></tr>
      <tr><th>displayname</th><td>${esc(state.displayName)}</td></tr>
      <tr><th>calendar-home-set</th><td class="mono">${esc(state.calHome)}</td></tr>
      <tr><th>addressbook-home-set</th><td class="mono">${esc(state.abHome)}</td></tr>
      <tr><th>schedule-inbox-URL</th><td class="mono">${esc(state.inbox)}</td></tr>
      <tr><th>schedule-outbox-URL</th><td class="mono">${esc(state.outbox)}</td></tr>
      <tr><th>calendar-user-address-set</th><td class="mono">${state.addresses.map(esc).join('<br>')}</td></tr>
    </table>`);
  });
  run($('#disc-wk', main), async () => {
    const r = await dav.req('GET', '/.well-known/caldav');
    setOut($('#disc-out', main), statusMsg(r,
      r.redirected ? `redirect followed to ${r.url.replace(location.origin, '')}` : ''));
  });
});

// -- Calendars -------------------------------------------------------------

panel('calendars', 'Calendars', main => {
  main.append(el(`<div>
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
  </div>`));

  const renderList = async () => {
    const cals = await homeChildren();
    const rows = cals.filter(c => c.isCalendar).map(c => `<tr>
      <td class="mono">${esc(c.name)}</td><td>${esc(c.displayName)}</td>
      <td class="mono">${esc(c.color)}</td><td>${esc(c.description)}</td>
      <td class="mono">${esc(c.components.join(','))}</td>
      <td class="mono">${esc(c.ctag)}</td>
      <td><button data-del="${esc(c.href)}">DELETE</button></td></tr>`).join('');
    setOut($('#cal-out', main), `<table class="list">
      <tr><th>uri</th><th>displayname</th><th>color</th><th>description</th><th>components</th><th>getctag</th><th></th></tr>
      ${rows}</table>`);
    for (const b of $('#cal-out', main).querySelectorAll('button[data-del]')) {
      run(b, async () => {
        if (!confirm(`DELETE ${b.dataset.del}?`)) return;
        await dav.req('DELETE', b.dataset.del);
        await renderList();
      });
    }
  };
  run($('#cal-list', main), renderList);

  run($('#cal-create', main), async () => {
    const comps = [$('#cal-ve', main).checked && 'VEVENT', $('#cal-vt', main).checked && 'VTODO']
      .filter(Boolean).map(c => `<c:comp name="${c}"/>`).join('');
    const body = dav.xmlDoc('c:mkcalendar', '', `<d:set><d:prop>
      <d:displayname>${esc($('#cal-dn', main).value)}</d:displayname>
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
    setOut($('#cal-patch-out', main), `color = <span class="mono">${esc(p.get(key(NS.a, 'calendar-color'))?.textContent ?? '(unset)')}</span>,
      description = <span class="mono">${esc(p.get(key(NS.c, 'calendar-description'))?.textContent ?? '(unset)')}</span>`);
  });
});

// -- Events ----------------------------------------------------------------

panel('events', 'Events', main => {
  main.append(el(`<div>
    <h2>Events</h2>
    <fieldset><legend>Objects (PROPFIND depth 1)</legend>
      ${calSelect}
      <button class="primary" id="ev-list">List objects</button>
      <div class="out" id="ev-out"></div>
    </fieldset>
    <fieldset><legend>Editor (PUT with conditional headers)</legend>
      <label class="fld">uid<input id="ev-uid" size="38"></label>
      <button id="ev-newuid" type="button">new UID</button><br>
      <label class="fld">summary<input id="ev-sum" value="Demo event"></label>
      <label class="fld">start<input id="ev-start" type="datetime-local"></label>
      <label class="fld">end<input id="ev-end" type="datetime-local"></label>
      <label class="fld">RRULE<input id="ev-rrule" placeholder="FREQ=DAILY;COUNT=3" size="28"></label>
      <label class="fld">attendees (comma emails)<input id="ev-att" size="34"></label><br>
      <label class="fld">If-Match (etag)<input id="ev-ifmatch" size="30"></label>
      <label class="chk"><input type="checkbox" id="ev-ifnone">If-None-Match: * (create only)</label>
      <button class="primary" id="ev-put">PUT .ics</button>
      <div class="out" id="ev-put-out"></div>
    </fieldset>
    <div class="out" id="ev-view"></div>
  </div>`));

  $('#ev-uid', main).value = ical.newUID();
  $('#ev-start', main).value = localDT(plusHours(1));
  $('#ev-end', main).value = localDT(plusHours(2));
  run($('#ev-newuid', main), () => { $('#ev-uid', main).value = ical.newUID(); });

  const sel = wireCalSelect(main);
  const renderObjects = async () => {
    const objs = await listObjects(sel.value);
    const rows = objs.map(o => `<tr><td class="mono">${esc(o.name)}</td>
      <td class="mono">${esc(o.etag)}</td>
      <td><button data-get="${esc(o.href)}">GET iCal</button>
          <button data-jcal="${esc(o.href)}">GET jCal</button>
          <button data-load="${esc(o.href)}" data-etag="${esc(o.etag)}">Load</button>
          <button data-del="${esc(o.href)}">DELETE</button></td></tr>`).join('');
    setOut($('#ev-out', main), `<table class="list"><tr><th>object</th><th>etag</th><th></th></tr>${rows}</table>`);
    const out = $('#ev-out', main);
    for (const b of out.querySelectorAll('button[data-get]')) run(b, async () => {
      const r = await dav.req('GET', b.dataset.get);
      setOut($('#ev-view', main), statusMsg(r, r.headers.get('Content-Type')) + pre(r.text));
    });
    for (const b of out.querySelectorAll('button[data-jcal]')) run(b, async () => {
      const r = await dav.req('GET', b.dataset.jcal, { headers: { Accept: 'application/calendar+json' } });
      let body = r.text;
      try { body = JSON.stringify(JSON.parse(r.text), null, 2); } catch { /* leave as-is */ }
      setOut($('#ev-view', main), statusMsg(r, r.headers.get('Content-Type')) + pre(body));
    });
    for (const b of out.querySelectorAll('button[data-load]')) run(b, async () => {
      const r = await dav.req('GET', b.dataset.load);
      const ev = ical.components(ical.parse(r.text), 'VEVENT')[0];
      if (!ev) return;
      $('#ev-uid', main).value = ical.propValue(ev, 'UID');
      $('#ev-sum', main).value = ical.propValue(ev, 'SUMMARY').replace(/\\([\\;,n])/g, (_, c) => c === 'n' ? '\n' : c);
      $('#ev-start', main).value = icalToLocal(ical.propValue(ev, 'DTSTART')) || $('#ev-start', main).value;
      $('#ev-end', main).value = icalToLocal(ical.propValue(ev, 'DTEND')) || $('#ev-end', main).value;
      $('#ev-rrule', main).value = ical.propValue(ev, 'RRULE');
      $('#ev-ifmatch', main).value = r.headers.get('ETag') ?? b.dataset.etag;
    });
    for (const b of out.querySelectorAll('button[data-del]')) run(b, async () => {
      if (!confirm(`DELETE ${b.dataset.del}?`)) return;
      await dav.req('DELETE', b.dataset.del);
      await renderObjects();
    });
  };
  run($('#ev-list', main), renderObjects);

  run($('#ev-put', main), async () => {
    const uid = $('#ev-uid', main).value.trim();
    const body = ical.buildEvent({
      uid,
      summary: $('#ev-sum', main).value,
      start: $('#ev-start', main).value,
      end: $('#ev-end', main).value,
      rrule: $('#ev-rrule', main).value.trim(),
      organizer: $('#ev-att', main).value.trim() ? primaryEmail() : '',
      attendees: $('#ev-att', main).value.split(',').map(s => s.trim()).filter(Boolean),
    });
    const headers = { 'Content-Type': 'text/calendar; charset=utf-8' };
    if ($('#ev-ifmatch', main).value.trim()) headers['If-Match'] = $('#ev-ifmatch', main).value.trim();
    if ($('#ev-ifnone', main).checked) headers['If-None-Match'] = '*';
    const r = await dav.req('PUT', sel.value + encodeURIComponent(uid) + '.ics', { headers, body });
    const note = r.status === 412 ? 'precondition failed (stale If-Match / existing If-None-Match)' :
      `ETag ${r.headers.get('ETag') ?? '(none)'}`;
    setOut($('#ev-put-out', main), statusMsg(r, note));
    if (r.headers.get('ETag')) $('#ev-ifmatch', main).value = r.headers.get('ETag');
  });
});

// -- Query -----------------------------------------------------------------

panel('query', 'Query', main => {
  main.append(el(`<div>
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
  </div>`));
  $('#q-start', main).value = localDT(new Date(Date.now() - 7 * 864e5));
  $('#q-end', main).value = localDT(new Date(Date.now() + 30 * 864e5));
  const sel = wireCalSelect(main);

  const renderMS = (r, box) => {
    const rows = dav.msResponses(r.xml).map(x => {
      const data = x.props.get(key(NS.c, 'calendar-data'))?.textContent ?? '';
      const n = ical.components(ical.parse(data), 'VEVENT').length;
      return `<tr><td class="mono">${esc(x.href)}</td><td>${n} VEVENT(s)</td></tr>
        <tr><td colspan="2">${pre(data)}</td></tr>`;
    }).join('');
    setOut(box, statusMsg(r, `${dav.msResponses(r.xml).length} response(s)`) +
      `<table class="list">${rows}</table>`);
  };

  run($('#q-run', main), async () => {
    const s = ical.utc($('#q-start', main).value), e = ical.utc($('#q-end', main).value);
    const expand = $('#q-expand', main).checked ? `<c:expand start="${s}" end="${e}"/>` : '';
    const body = dav.xmlDoc('c:calendar-query', '', `
      <d:prop><d:getetag/><c:calendar-data>${expand}</c:calendar-data></d:prop>
      <c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT">
        <c:time-range start="${s}" end="${e}"/>
      </c:comp-filter></c:comp-filter></c:filter>`);
    renderMS(await dav.report(sel.value, body), $('#q-out', main));
  });

  run($('#q-mget', main), async () => {
    const hrefs = $('#q-hrefs', main).value.split('\n').map(s => s.trim()).filter(Boolean)
      .map(h => `<d:href>${esc(h)}</d:href>`).join('');
    const body = dav.xmlDoc('c:calendar-multiget', '',
      `<d:prop><d:getetag/><c:calendar-data/></d:prop>${hrefs}`);
    renderMS(await dav.report(sel.value, body), $('#q-mget-out', main));
  });
});

// -- Sync ------------------------------------------------------------------

panel('sync', 'Sync', main => {
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
});

// -- Free/Busy -------------------------------------------------------------

panel('freebusy', 'Free/Busy', main => {
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
});

// -- Scheduling ------------------------------------------------------------

panel('scheduling', 'Scheduling', main => {
  main.append(el(`<div>
    <h2>iTIP scheduling</h2>
    <fieldset><legend>Outbox free-busy probe (POST VFREEBUSY REQUEST)</legend>
      <label class="fld">attendees (comma emails)<input id="sc-fb-att" value="bob@example.org" size="34"></label>
      <label class="fld">start<input id="sc-fb-start" type="datetime-local"></label>
      <label class="fld">end<input id="sc-fb-end" type="datetime-local"></label>
      <button class="primary" id="sc-fb">POST outbox</button>
      <div class="out" id="sc-fb-out"></div>
    </fieldset>
    <fieldset><legend>Invite (PUT with ATTENDEEs → inbox delivery + SCHEDULE-STATUS)</legend>
      ${calSelect}
      <label class="fld">summary<input id="sc-sum" value="Planning meeting"></label>
      <label class="fld">start<input id="sc-start" type="datetime-local"></label>
      <label class="fld">end<input id="sc-end" type="datetime-local"></label>
      <label class="fld">attendees<input id="sc-att" value="bob@example.org" size="34"></label>
      <button class="primary" id="sc-invite">PUT invite</button>
      <button id="sc-status">Reload organizer copy (SCHEDULE-STATUS)</button>
      <div class="out" id="sc-inv-out"></div>
    </fieldset>
    <fieldset><legend>Inbox</legend>
      <button id="sc-inbox">PROPFIND inbox depth 1</button>
      <div class="out" id="sc-inbox-out"></div>
    </fieldset>
  </div>`));
  $('#sc-fb-start', main).value = localDT(plusHours(1));
  $('#sc-fb-end', main).value = localDT(plusHours(9));
  $('#sc-start', main).value = localDT(plusHours(24));
  $('#sc-end', main).value = localDT(plusHours(25));
  const sel = wireCalSelect(main);
  let lastInvite = null;

  run($('#sc-fb', main), async () => {
    const body = ical.buildFreeBusyRequest({
      organizer: primaryEmail(),
      attendees: $('#sc-fb-att', main).value.split(',').map(s => s.trim()).filter(Boolean),
      start: $('#sc-fb-start', main).value,
      end: $('#sc-fb-end', main).value,
    });
    const r = await dav.req('POST', state.outbox, {
      headers: { 'Content-Type': 'text/calendar; charset=utf-8' }, body,
    });
    setOut($('#sc-fb-out', main), statusMsg(r, 'schedule-response') +
      (r.xml ? preXML(r.text) : pre(r.text)));
  });

  run($('#sc-invite', main), async () => {
    const uid = ical.newUID();
    const body = ical.buildEvent({
      uid,
      summary: $('#sc-sum', main).value,
      start: $('#sc-start', main).value,
      end: $('#sc-end', main).value,
      organizer: primaryEmail(),
      attendees: $('#sc-att', main).value.split(',').map(s => s.trim()).filter(Boolean),
    });
    lastInvite = sel.value + uid + '.ics';
    const r = await dav.req('PUT', lastInvite, {
      headers: { 'Content-Type': 'text/calendar; charset=utf-8' }, body,
    });
    setOut($('#sc-inv-out', main), statusMsg(r, `stored ${lastInvite} — the server delivers copies to each attendee's inbox`));
  });

  run($('#sc-status', main), async () => {
    if (!lastInvite) { setOut($('#sc-inv-out', main), 'send an invite first'); return; }
    const r = await dav.req('GET', lastInvite);
    const ev = ical.components(ical.parse(r.text), 'VEVENT')[0];
    const rows = (ev?.props ?? []).filter(p => p.name === 'ATTENDEE').map(p =>
      `<tr><td class="mono">${esc(p.value)}</td><td class="mono">${esc(p.params['SCHEDULE-STATUS'] ?? '(none)')}</td></tr>`).join('');
    setOut($('#sc-inv-out', main), statusMsg(r, '') +
      `<table class="list"><tr><th>attendee</th><th>SCHEDULE-STATUS</th></tr>${rows}</table>` + pre(r.text));
  });

  run($('#sc-inbox', main), async () => {
    const objs = await listObjects(state.inbox);
    const rows = objs.map(o => `<tr><td class="mono">${esc(o.name)}</td>
      <td><button data-get="${esc(o.href)}">GET</button>
          <button data-del="${esc(o.href)}">DELETE</button></td></tr>`).join('');
    setOut($('#sc-inbox-out', main),
      `<table class="list"><tr><th>scheduling object</th><th></th></tr>${rows || '<tr><td colspan="2">(empty)</td></tr>'}</table><div class="out"></div>`);
    const box = $('#sc-inbox-out .out', main);
    for (const b of $('#sc-inbox-out', main).querySelectorAll('button[data-get]')) run(b, async () => {
      const r = await dav.req('GET', b.dataset.get);
      setOut(box, pre(r.text));
    });
    for (const b of $('#sc-inbox-out', main).querySelectorAll('button[data-del]')) run(b, async () => {
      await dav.req('DELETE', b.dataset.del);
      $('#sc-inbox', main).click();
    });
  });
});

// -- Sharing ---------------------------------------------------------------

panel('sharing', 'Sharing', main => {
  main.append(el(`<div>
    <h2>Calendar sharing</h2>
    <p class="hint">The Postgres storage backend does not implement sharing,
    notifications, or invite replies (sabre PDO parity) — these requests
    demonstrate the protocol and surface the server's honest 403/501/404 in
    the drawer. Mock-backend test servers accept them.</p>
    <fieldset><legend>Share / unshare (POST {cs}share to the calendar)</legend>
      ${calSelect}
      <label class="fld">sharee href<input id="sh-href" value="mailto:bob@example.org" size="30"></label>
      <label class="fld">summary<input id="sh-sum" value="shared from calclient"></label>
      <label class="chk"><input type="checkbox" id="sh-rw" checked>read-write</label>
      <button class="primary" id="sh-share">Share (set)</button>
      <button id="sh-unshare">Unshare (remove)</button>
      <div class="out" id="sh-out"></div>
    </fieldset>
    <fieldset><legend>Notifications collection</legend>
      <button id="sh-notif">List /calendars/{u}/notifications/</button>
      <div class="out" id="sh-notif-out"></div>
    </fieldset>
    <fieldset><legend>Invite reply (POST {cs}invite-reply to the calendar home — log in as the sharee)</legend>
      <label class="fld">hosturl (shared calendar href)<input id="sh-host" size="40"></label>
      <label class="fld">in-reply-to (uid from the notification)<input id="sh-irt" size="30"></label>
      <label class="chk"><input type="checkbox" id="sh-accept" checked>accept</label>
      <button id="sh-reply">POST invite-reply</button>
      <div class="out" id="sh-reply-out"></div>
    </fieldset>
  </div>`));
  const sel = wireCalSelect(main);

  run($('#sh-share', main), async () => {
    const rw = $('#sh-rw', main).checked ? '<cs:read-write/>' : '';
    const body = dav.xmlDoc('cs:share', '', `<cs:set>
      <d:href>${esc($('#sh-href', main).value.trim())}</d:href>
      <cs:summary>${esc($('#sh-sum', main).value)}</cs:summary>${rw}
    </cs:set>`);
    const r = await dav.req('POST', sel.value, {
      headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
    });
    setOut($('#sh-out', main), statusMsg(r, r.ok ? 'sharee invited — a notification lands for them' : ''));
  });

  run($('#sh-unshare', main), async () => {
    const body = dav.xmlDoc('cs:share', '',
      `<cs:remove><d:href>${esc($('#sh-href', main).value.trim())}</d:href></cs:remove>`);
    const r = await dav.req('POST', sel.value, {
      headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
    });
    setOut($('#sh-out', main), statusMsg(r, r.ok ? 'access removed' : ''));
  });

  run($('#sh-notif', main), async () => {
    const base = state.calHome + 'notifications/';
    const objs = await listObjects(base);
    const rows = objs.map(o => `<tr><td class="mono">${esc(o.name)}</td>
      <td><button data-get="${esc(o.href)}">GET</button></td></tr>`).join('');
    setOut($('#sh-notif-out', main),
      `<table class="list"><tr><th>notification</th><th></th></tr>${rows || '<tr><td colspan="2">(empty)</td></tr>'}</table><div class="out"></div>`);
    for (const b of $('#sh-notif-out', main).querySelectorAll('button[data-get]')) run(b, async () => {
      const r = await dav.req('GET', b.dataset.get);
      setOut($('#sh-notif-out .out', main), preXML(r.text));
      const uid = r.xml?.getElementsByTagNameNS(NS.cs, 'uid')[0]?.textContent;
      const host = r.xml ? dav.hrefText(r.xml.getElementsByTagNameNS(NS.cs, 'hosturl')[0]) : '';
      if (uid) $('#sh-irt', main).value = uid;
      if (host) $('#sh-host', main).value = host;
    });
  });

  run($('#sh-reply', main), async () => {
    const verdict = $('#sh-accept', main).checked ? '<cs:invite-accepted/>' : '<cs:invite-declined/>';
    const body = dav.xmlDoc('cs:invite-reply', '', `
      <cs:hosturl><d:href>${esc($('#sh-host', main).value.trim())}</d:href></cs:hosturl>
      <cs:in-reply-to>${esc($('#sh-irt', main).value.trim())}</cs:in-reply-to>${verdict}`);
    const r = await dav.req('POST', state.calHome, {
      headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
    });
    setOut($('#sh-reply-out', main), statusMsg(r, r.ok ? 'reply delivered to the sharer' : ''));
  });
});

// -- Delegation ------------------------------------------------------------

panel('delegation', 'Delegation', main => {
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
});

// -- Subscriptions ---------------------------------------------------------

panel('subscriptions', 'Subscriptions', main => {
  main.append(el(`<div>
    <h2>Webcal subscriptions</h2>
    <p class="hint">The Postgres storage backend does not store subscriptions
    (sabre PDO parity): expect a 403 "backend does not support subscriptions"
    in the drawer — the request itself is the demonstration.</p>
    <fieldset><legend>Create (MKCALENDAR with {cs}subscribed + {cs}source)</legend>
      <label class="fld">uri name<input id="su-name" value="holidays"></label>
      <label class="fld">displayname<input id="su-dn" value="Holidays feed"></label>
      <label class="fld">source URL<input id="su-src" value="webcal://example.org/holidays.ics" size="36"></label>
      <button class="primary" id="su-create">MKCALENDAR</button>
      <div class="out" id="su-create-out"></div>
    </fieldset>
    <fieldset><legend>List</legend>
      <button id="su-list">List subscriptions</button>
      <div class="out" id="su-out"></div>
    </fieldset>
  </div>`));

  run($('#su-create', main), async () => {
    // xmlns declared inline: the server re-parses the resourcetype/source
    // fragments standalone, so prefixes bound on the root would be lost.
    const body = dav.xmlDoc('c:mkcalendar', '', `<d:set><d:prop>
      <d:resourcetype><d:collection/><subscribed xmlns="${NS.cs}"/></d:resourcetype>
      <d:displayname>${esc($('#su-dn', main).value)}</d:displayname>
      <cs:source><href xmlns="${NS.d}">${esc($('#su-src', main).value.trim())}</href></cs:source>
    </d:prop></d:set>`);
    const r = await dav.req('MKCALENDAR', state.calHome + encodeURIComponent($('#su-name', main).value) + '/', {
      headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
    });
    setOut($('#su-create-out', main), statusMsg(r, r.status === 201 ? 'subscription created' : ''));
  });

  const renderSubs = async () => {
    const subs = (await homeChildren()).filter(c => c.isSubscription);
    const rows = subs.map(s => `<tr><td class="mono">${esc(s.name)}</td>
      <td>${esc(s.displayName)}</td><td class="mono">${esc(s.source)}</td>
      <td><button data-del="${esc(s.href)}">DELETE</button></td></tr>`).join('');
    setOut($('#su-out', main), `<table class="list">
      <tr><th>uri</th><th>displayname</th><th>source</th><th></th></tr>
      ${rows || '<tr><td colspan="4">(none)</td></tr>'}</table>`);
    for (const b of $('#su-out', main).querySelectorAll('button[data-del]')) run(b, async () => {
      if (!confirm(`DELETE ${b.dataset.del}?`)) return;
      await dav.req('DELETE', b.dataset.del);
      await renderSubs();
    });
  };
  run($('#su-list', main), renderSubs);
});

// -- Export / Add-Member ---------------------------------------------------

panel('export', 'Export / Add-Member', main => {
  main.append(el(`<div>
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
  </div>`));
  $('#ex-start', main).value = localDT(new Date(Date.now() - 30 * 864e5));
  $('#ex-end', main).value = localDT(new Date(Date.now() + 60 * 864e5));
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
    $('#ex-link', main).innerHTML = `<a href="${esc(url)}" download>download</a>`;
    setOut($('#ex-out', main), statusMsg(r, r.headers.get('Content-Type')) + pre(r.text));
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
});

// -- Contacts --------------------------------------------------------------

panel('contacts', 'Contacts', main => {
  main.append(el(`<div>
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
  </div>`));

  const sel = $('#ct-sel', main);
  const fillBooks = async () => {
    const books = await bookChildren();
    sel.innerHTML = books.map(b =>
      `<option value="${esc(b.href)}">${esc(b.displayName || b.name)}</option>`).join('');
    return books;
  };
  fillBooks();
  run($('#ct-refresh', main), fillBooks);

  run($('#ct-books', main), async () => {
    const books = await fillBooks();
    const rows = books.map(b => `<tr><td class="mono">${esc(b.name)}</td>
      <td>${esc(b.displayName)}</td><td class="mono">${esc(b.ctag)}</td>
      <td><button data-del="${esc(b.href)}">DELETE</button></td></tr>`).join('');
    setOut($('#ct-books-out', main), `<table class="list">
      <tr><th>uri</th><th>displayname</th><th>getctag</th><th></th></tr>${rows}</table>`);
    for (const b of $('#ct-books-out', main).querySelectorAll('button[data-del]')) run(b, async () => {
      if (!confirm(`DELETE ${b.dataset.del}?`)) return;
      await dav.req('DELETE', b.dataset.del);
      $('#ct-books', main).click();
    });
  });

  run($('#ct-mkcol', main), async () => {
    const body = dav.xmlDoc('d:mkcol', '', `<d:set><d:prop>
      <d:resourcetype><d:collection/><card:addressbook/></d:resourcetype>
      <d:displayname>${esc($('#ct-bdn', main).value)}</d:displayname>
    </d:prop></d:set>`);
    const r = await dav.req('MKCOL', state.abHome + encodeURIComponent($('#ct-bname', main).value) + '/', {
      headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
    });
    setOut($('#ct-books-out', main), statusMsg(r, r.status === 201 ? 'created' : ''));
  });

  const renderCards = async () => {
    const objs = await listObjects(sel.value);
    const rows = objs.map(o => `<tr><td class="mono">${esc(o.name)}</td>
      <td class="mono">${esc(o.etag)}</td>
      <td><button data-get="${esc(o.href)}">GET</button>
          <button data-del="${esc(o.href)}">DELETE</button></td></tr>`).join('');
    setOut($('#ct-cards-out', main),
      `<table class="list"><tr><th>card</th><th>etag</th><th></th></tr>${rows || '<tr><td colspan="3">(empty)</td></tr>'}</table><div class="out"></div>`);
    for (const b of $('#ct-cards-out', main).querySelectorAll('button[data-get]')) run(b, async () => {
      const r = await dav.req('GET', b.dataset.get);
      setOut($('#ct-cards-out .out', main), pre(r.text));
    });
    for (const b of $('#ct-cards-out', main).querySelectorAll('button[data-del]')) run(b, async () => {
      if (!confirm(`DELETE ${b.dataset.del}?`)) return;
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
      return `<tr><td class="mono">${esc(x.href)}</td></tr><tr><td>${pre(data)}</td></tr>`;
    }).join('');
    setOut(box, statusMsg(r, `${dav.msResponses(r.xml).length} match(es)`) +
      `<table class="list">${rows}</table>`);
  };

  run($('#ct-query', main), async () => {
    const body = dav.xmlDoc('card:addressbook-query', '', `
      <d:prop><d:getetag/><card:address-data/></d:prop>
      <card:filter test="anyof"><card:prop-filter name="${esc($('#ct-field', main).value)}">
        <card:text-match collation="i;unicode-casemap" match-type="contains">${esc($('#ct-match', main).value)}</card:text-match>
      </card:prop-filter></card:filter>`);
    renderCardMS(await dav.report(sel.value, body), $('#ct-query-out', main));
  });

  run($('#ct-mget', main), async () => {
    const hrefs = $('#ct-hrefs', main).value.split('\n').map(s => s.trim()).filter(Boolean)
      .map(h => `<d:href>${esc(h)}</d:href>`).join('');
    const body = dav.xmlDoc('card:addressbook-multiget', '',
      `<d:prop><d:getetag/><card:address-data/></d:prop>${hrefs}`);
    renderCardMS(await dav.report(sel.value, body), $('#ct-query-out', main));
  });

  run($('#ct-export', main), async () => {
    const url = sel.value + '?export';
    const r = await dav.req('GET', url);
    $('#ct-export-link', main).innerHTML = `<a href="${esc(url)}" download>download</a>`;
    setOut($('#ct-export-out', main), statusMsg(r, r.headers.get('Content-Type')) + pre(r.text));
  });
});

// -- ACL -------------------------------------------------------------------

panel('acl', 'ACL', main => {
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
});

// -- Raw -------------------------------------------------------------------

panel('raw', 'Raw', main => {
  main.append(el(`<div>
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
  </div>`));
  $('#raw-body', main).value = dav.propfindBody(['d:resourcetype', 'd:displayname']);

  run($('#raw-send', main), async () => {
    const headers = {};
    if ($('#raw-depth', main).value) headers['Depth'] = $('#raw-depth', main).value;
    if ($('#raw-dest', main).value.trim()) headers['Destination'] = $('#raw-dest', main).value.trim();
    const body = $('#raw-body', main).value;
    if (body) headers['Content-Type'] = $('#raw-ct', main).value;
    const r = await dav.req($('#raw-method', main).value.trim().toUpperCase(),
      $('#raw-path', main).value.trim(), { headers, body: body || null });
    setOut($('#raw-out', main), statusMsg(r, '') + (r.text ? (r.xml ? preXML(r.text) : pre(r.text)) : ''));
  });
});

// ---- router --------------------------------------------------------------

let currentPanel = '';

function activate(id) {
  const p = PANELS.find(x => x.id === id) ?? PANELS[0];
  currentPanel = p.id;
  location.hash = p.id;
  for (const b of $('#tabs').children) b.classList.toggle('active', b.dataset.id === p.id);
  const main = $('#panel');
  main.innerHTML = '';
  if (!dav.session.auth) {
    main.append(el('<p class="hint">Log in first (top bar) — e.g. admin / admin against the dev server.</p>'));
    return;
  }
  p.render(main);
}

for (const p of PANELS) {
  const b = el(`<button data-id="${p.id}">${esc(p.title)}</button>`);
  b.addEventListener('click', () => activate(p.id));
  $('#tabs').append(b);
}

(async () => {
  if (dav.session.auth) {
    $('#login-user').value = dav.session.user;
    try { await discover(); $('#login-btn').textContent = 'Re-login'; } catch { dav.session.logout(); }
  }
  activate(location.hash.replace('#', '') || 'discover');
})();
