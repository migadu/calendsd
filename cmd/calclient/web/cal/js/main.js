// main.js — calweb boot: login flow, top bar, sidebar, view routing.

import * as store from '/shared/store.js';
import { renderGrid, gridNav, setGridView } from './grid.js';
import { openEventDialog } from './event-dialog.js';
import { renderInvites, toggleInvites } from './invites.js';
import { renderContacts } from './contacts.js';
import { renderSettings } from './settings.js';

// ---- tiny DOM helpers (shared with the other modules) --------------------

export const $ = (sel, root = document) => root.querySelector(sel);
export function h(html) {
  const t = document.createElement('template');
  t.innerHTML = html.trim();
  return t.content.firstElementChild;
}
export const esc = s => String(s ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;')
  .replace(/>/g, '&gt;').replace(/"/g, '&quot;');

let toastTimer = null;
export function toast(msg, isError = false) {
  const t = $('#toast');
  t.textContent = msg;
  t.classList.toggle('error', isError);
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, isError ? 6000 : 3000);
}

// linkify HTML-escapes text, then turns http(s) URLs into clickable links —
// so a meeting link pasted into a location or notes field is joinable.
export function linkify(text) {
  const re = /(https?:\/\/[^\s<]+)/g;
  let out = '', last = 0, m;
  while ((m = re.exec(text))) {
    out += esc(text.slice(last, m.index));
    out += `<a href="${esc(m[1])}" target="_blank" rel="noreferrer noopener">${esc(m[1])}</a>`;
    last = m.index + m[1].length;
  }
  return out + esc(text.slice(last));
}

// showError maps any thrown error to a friendly toast (store.DavError
// already carries friendly text).
export function showError(e) {
  console.error(e);
  toast(e?.message || 'Something went wrong.', true);
  if (e?.conflict) store.refreshEvents().catch(() => {});
}

// tzOpt adds the chosen display time zone to a toLocale* options object
// (empty pref = the device's own zone).
function tzOpt(opts) {
  const tz = store.state.prefs.tz;
  return tz ? { ...opts, timeZone: tz } : opts;
}
export function fmtTime(d) {
  return d.toLocaleTimeString([], tzOpt({ hour: 'numeric', minute: '2-digit', hour12: store.state.prefs.hour12 }));
}
export function fmtDate(d) {
  return d.toLocaleDateString([], tzOpt({ weekday: 'short', month: 'short', day: 'numeric' }));
}
export function fmtRange(inst) {
  if (inst.allDay) {
    const days = Math.round((inst.end - inst.start) / 864e5);
    return fmtDate(inst.start) + (days > 1 ? ' – ' + fmtDate(new Date(+inst.end - 864e5)) : '') + ' · all day';
  }
  const sameDay = inst.start.toDateString() === inst.end.toDateString();
  return fmtDate(inst.start) + ' ' + fmtTime(inst.start) + ' – ' +
    (sameDay ? '' : fmtDate(inst.end) + ' ') + fmtTime(inst.end);
}

// ---- routing -------------------------------------------------------------

let currentView = 'calendar';

function route() {
  const target = (location.hash.replace('#', '') || 'calendar').split('/')[0];
  currentView = ['calendar', 'contacts', 'settings'].includes(target) ? target : 'calendar';
  // Drives which chrome is relevant: the CSS hides the calendar sidebar and
  // the date/view controls on any non-calendar view.
  document.body.dataset.view = currentView;
  for (const a of document.querySelectorAll('#mainnav a')) {
    a.classList.toggle('active', a.dataset.nav === currentView);
  }
  closeMenus();
  render();
}

// closeMenus collapses the transient top-bar affordances (identity dropdown
// and the narrow-screen calendar drawer) — called on navigation and Escape.
function closeMenus() {
  document.body.classList.remove('sidebar-open');
  $('#menu-toggle').setAttribute('aria-expanded', 'false');
  $('#user-pop').hidden = true;
  $('#user-btn').setAttribute('aria-expanded', 'false');
}

export function render() {
  // Don't render (and don't let the grid kick off a range fetch) until the
  // app chrome is actually shown — bus events like 'prefs' can fire during
  // login, before calendars have loaded.
  if ($('#topbar').hidden) return;
  const main = $('#content');
  if (currentView === 'calendar') renderGrid(main);
  else if (currentView === 'contacts') renderContacts(main);
  else renderSettings(main);
}

// ---- session UI ----------------------------------------------------------

function showApp(loggedIn) {
  $('#login').hidden = loggedIn;
  $('#topbar').hidden = !loggedIn;
  $('#layout').hidden = !loggedIn;
  if (loggedIn) {
    $('#whoami').textContent = store.state.displayName || store.state.user;
    route();
  }
}

$('#login-form').addEventListener('submit', async e => {
  e.preventDefault();
  $('#login-error').textContent = '';
  const btn = $('#login-form button');
  btn.disabled = true;
  try {
    await store.login($('#login-user').value.trim(), $('#login-pass').value, $('#login-stay').checked);
    showApp(true);
  } catch (err) {
    $('#login-error').textContent = err.message || 'Sign-in failed.';
  } finally {
    btn.disabled = false;
  }
});

$('#signout').addEventListener('click', () => {
  store.logout();
  showApp(false);
});

// ---- top bar / sidebar wiring -------------------------------------------

window.addEventListener('hashchange', route);
$('#nav-today').addEventListener('click', () => gridNav(0));
$('#nav-prev').addEventListener('click', () => gridNav(-1));
$('#nav-next').addEventListener('click', () => gridNav(1));
for (const b of document.querySelectorAll('#view-seg button')) {
  b.addEventListener('click', () => {
    for (const x of document.querySelectorAll('#view-seg button')) x.classList.toggle('active', x === b);
    setGridView(b.dataset.view);
  });
}
$('#bell').addEventListener('click', () => toggleInvites());
$('#invites-close').addEventListener('click', () => toggleInvites(false));
$('#new-event').addEventListener('click', () => { closeMenus(); openEventDialog(null); });

// Identity dropdown (name → Settings / Sign out) in the top-right.
$('#user-btn').addEventListener('click', e => {
  e.stopPropagation();
  const pop = $('#user-pop');
  pop.hidden = !pop.hidden;
  $('#user-btn').setAttribute('aria-expanded', String(!pop.hidden));
});
$('#user-pop').addEventListener('click', () => { $('#user-pop').hidden = true; });
document.addEventListener('click', e => {
  if (!$('#usermenu').contains(e.target)) {
    $('#user-pop').hidden = true;
    $('#user-btn').setAttribute('aria-expanded', 'false');
  }
});

// Narrow-screen calendar drawer: the hamburger and a full-screen scrim.
$('#menu-toggle').addEventListener('click', () => {
  const open = document.body.classList.toggle('sidebar-open');
  $('#menu-toggle').setAttribute('aria-expanded', String(open));
});
$('#scrim').addEventListener('click', closeMenus);

$('#new-calendar').addEventListener('click', async () => {
  closeMenus();
  const name = prompt('Name for the new calendar:');
  if (!name) return;
  try {
    await store.createCalendar(name.toLowerCase().replace(/[^a-z0-9-]+/g, '-'), name, '');
    toast(`Calendar “${name}” created.`);
  } catch (e) { showError(e); }
});

function renderSidebar() {
  const ul = $('#cal-list');
  ul.innerHTML = '';
  for (const cal of store.state.calendars) {
    const li = h(`<li>
      <label><input type="checkbox" ${store.state.hidden.has(cal.href) ? '' : 'checked'}>
        <span class="dot" style="background:${esc(store.calColor(cal.href))}"></span>
        <span class="name">${esc(cal.displayName || cal.name)}</span></label>
      <button class="gear" title="Calendar settings">⚙</button>
    </li>`);
    $('input', li).addEventListener('change', ev => {
      if (ev.target.checked) store.state.hidden.delete(cal.href);
      else store.state.hidden.add(cal.href);
      render();
    });
    $('.gear', li).addEventListener('click', () => calendarMenu(cal));
    ul.append(li);
  }
}

function calendarMenu(cal) {
  closeMenus();
  const dialog = h(`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true">
    <h2>${esc(cal.displayName || cal.name)}</h2>
    <div class="grid">
      <label>Name<input id="cm-name" value="${esc(cal.displayName || cal.name)}"></label>
      <label>Color<input id="cm-color" type="color" value="${esc(store.calColor(cal.href))}"></label>
    </div>
    <div class="actions">
      <button class="danger" id="cm-delete">Delete calendar…</button>
      <button id="cm-cancel">Cancel</button>
      <button class="primary" id="cm-save">Save</button>
    </div>
  </div></div>`);
  const close = () => dialog.remove();
  dialog.addEventListener('click', ev => { if (ev.target === dialog) close(); });
  $('#cm-cancel', dialog).addEventListener('click', close);
  $('#cm-save', dialog).addEventListener('click', async () => {
    try {
      await store.updateCalendar(cal.href, {
        displayName: $('#cm-name', dialog).value,
        color: $('#cm-color', dialog).value,
      });
      close();
    } catch (e) { showError(e); }
  });
  $('#cm-delete', dialog).addEventListener('click', async () => {
    if (!confirm(`Delete “${cal.displayName || cal.name}” and all its events? This cannot be undone.`)) return;
    try { await store.deleteCalendar(cal.href); close(); } catch (e) { showError(e); }
  });
  $('#dialog-host').append(dialog);
  $('#cm-name', dialog).focus();
}

// ---- store subscriptions -------------------------------------------------

store.bus.addEventListener('calendars', renderSidebar);
store.bus.addEventListener('events', () => { if (currentView === 'calendar') render(); });
store.bus.addEventListener('contacts', () => { if (currentView === 'contacts') render(); });
store.bus.addEventListener('invites', () => {
  const n = store.state.invites.length;
  const badge = $('#bell-badge');
  badge.hidden = n === 0;
  badge.textContent = n;
  renderInvites();
});
store.bus.addEventListener('prefs', render);

document.addEventListener('keydown', e => {
  if (e.key === 'Escape') {
    const overlays = document.querySelectorAll('#dialog-host .overlay');
    if (overlays.length) overlays[overlays.length - 1].remove();  // topmost
    else if (!$('#user-pop').hidden || document.body.classList.contains('sidebar-open')) closeMenus();
    else toggleInvites(false);
  }
});

// ---- boot ----------------------------------------------------------------

(async () => {
  // Show the login screen up front so a slow or failing restore can never
  // leave a blank page; hide it only once restore confirms a live session.
  showApp(false);
  try {
    if (await store.restore()) showApp(true);
  } catch (e) {
    console.error('restore failed', e);
    store.logout();
    showApp(false);
  }
})();
