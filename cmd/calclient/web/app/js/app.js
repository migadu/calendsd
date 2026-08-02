// app.js — entry point: the login form, the raw-exchange drawer, and the
// tab router. Each feature area lives in panels/ as {id, title, render};
// every panel funnels its requests through dav.req, so the drawer doubles
// as a protocol explorer.

import * as dav from '/shared/dav.js';
import { $, html, frag, pre, setOut } from './ui.js';
import { discover } from './session.js';

import discoverPanel from './panels/discover.js';
import calendarsPanel from './panels/calendars.js';
import eventsPanel from './panels/events.js';
import queryPanel from './panels/query.js';
import syncPanel from './panels/sync.js';
import freebusyPanel from './panels/freebusy.js';
import schedulingPanel from './panels/scheduling.js';
import sharingPanel from './panels/sharing.js';
import delegationPanel from './panels/delegation.js';
import subscriptionsPanel from './panels/subscriptions.js';
import exportPanel from './panels/export.js';
import contactsPanel from './panels/contacts.js';
import aclPanel from './panels/acl.js';
import rawPanel from './panels/raw.js';

// Tab order.
const PANELS = [
  discoverPanel, calendarsPanel, eventsPanel, queryPanel, syncPanel,
  freebusyPanel, schedulingPanel, sharingPanel, delegationPanel,
  subscriptionsPanel, exportPanel, contactsPanel, aclPanel, rawPanel,
];

// ---- raw exchange drawer -------------------------------------------------

dav.setOnExchange(({ method, path, reqHeaders, reqBody, res, error }) => {
  const box = $('#exchange');
  const reqHdrs = Object.entries(reqHeaders).map(([k, v]) => `${k}: ${v}`).join('\n');
  const parts = [frag`<div class="req-line">${method} ${path}</div>`,
    frag`<details open><summary>request headers</summary>${pre(reqHdrs || '(none)')}</details>`];
  if (reqBody) parts.push(frag`<details open><summary>request body</summary>${pre(String(reqBody))}</details>`);
  if (error) {
    parts.push(frag`<div class="status-err">${error}</div>`);
  } else {
    const cls = res.status < 400 ? 'status-ok' : 'status-err';
    let line = `HTTP ${res.status}`;
    if (res.redirected) line += ` (redirected → ${res.url.replace(location.origin, '')})`;
    parts.push(frag`<div class="req-line ${cls}">${line}</div>`);
    const respHdrs = [...res.headers.entries()].map(([k, v]) => `${k}: ${v}`).join('\n');
    parts.push(frag`<details open><summary>response headers</summary>${pre(respHdrs)}</details>`);
    if (res.text) {
      const body = res.xml ? dav.prettyXML(res.text) : res.text;
      parts.push(frag`<details open><summary>response body</summary>${pre(body)}</details>`);
    }
  }
  setOut(box, parts);
});

// ---- login ---------------------------------------------------------------

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
    main.append(html`<p class="hint">Log in first (top bar) — e.g. admin / admin against the dev server.</p>`);
    return;
  }
  p.render(main);
}

for (const p of PANELS) {
  const b = html`<button data-id="${p.id}">${p.title}</button>`;
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
