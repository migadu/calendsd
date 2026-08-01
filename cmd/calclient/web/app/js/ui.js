// ui.js — tiny DOM and formatting helpers shared by every panel.

import * as dav from '/shared/dav.js';

const { esc } = dav;

export const $ = (sel, root = document) => root.querySelector(sel);

export function el(html) {
  const t = document.createElement('template');
  t.innerHTML = html.trim();
  return t.content.firstElementChild;
}

// run wires a button to an async action, disabling it while the action runs.
export function run(btn, fn) {
  btn.addEventListener('click', async () => {
    btn.disabled = true;
    try { await fn(); } catch (e) { console.error(e); } finally { btn.disabled = false; }
  });
}

export function setOut(box, html) { box.innerHTML = html; }

export function statusMsg(res, okText) {
  const cls = res.ok || res.status === 207 ? 'msg-ok' : 'msg-err';
  return `<span class="${cls}">HTTP ${res.status}</span> ${esc(okText ?? '')}`;
}

export function pre(text) { return `<pre>${esc(text)}</pre>`; }
export function preXML(text) { return pre(dav.prettyXML(text)); }

export function lastSegment(href) {
  return decodeURIComponent(href.replace(/\/+$/, '').split('/').pop() || '/');
}

// datetime-local <-> iCal helpers
export function localDT(d) {
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}
export function plusHours(h) { const d = new Date(); d.setHours(d.getHours() + h, 0, 0, 0); return d; }
export function icalToLocal(v) {
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})(Z?)$/.exec(v || '');
  if (!m) return '';
  const iso = `${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}${m[7]}`;
  return localDT(new Date(iso));
}
