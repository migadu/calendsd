// ui.js — panel helpers on top of the shared DOM and time modules. The DOM
// construction/escaping primitives and the date helpers are re-exported so
// panels have a single import for their UI needs.

import * as dav from '/shared/dav.js';
import { frag } from '/shared/dom.js';

export { $, html, frag, raw, esc, brJoin } from '/shared/dom.js';
export { localDT, plusHours } from '/shared/time.js';
export { icalToLocal } from '/shared/ical.js';

// run wires a button to an async action, disabling it while the action runs.
export function run(btn, fn) {
  btn.addEventListener('click', async () => {
    btn.disabled = true;
    try { await fn(); } catch (e) { console.error(e); } finally { btn.disabled = false; }
  });
}

// setOut renders a frag/raw value (or plain text, which is escaped like any
// other interpolation) into an output box — the sink keeps dom.js's
// escape-by-default rule instead of trusting arbitrary strings.
export function setOut(box, out) { box.innerHTML = frag`${out}`; }

// rowsOr renders a list of <tr> frags, or a single placeholder row spanning
// `cols` columns when the list is empty.
export function rowsOr(rows, cols, label = '(empty)') {
  return rows.length ? rows : frag`<tr><td colspan="${cols}">${label}</td></tr>`;
}

export function statusMsg(res, okText) {
  const cls = res.ok || res.status === 207 ? 'msg-ok' : 'msg-err';
  return frag`<span class="${cls}">HTTP ${res.status}</span> ${okText ?? ''}`;
}

export function pre(text) { return frag`<pre>${text}</pre>`; }
export function preXML(text) { return pre(dav.prettyXML(text)); }

// lastSegment: dav's, with '/' standing in for the root path in listings.
export const lastSegment = href => dav.lastSegment(href) || '/';
