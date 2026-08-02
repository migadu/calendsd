// main.js — calweb boot: login flow, top bar, sidebar, view routing. The
// app-level helpers (toast, formatting) live in ./ui.js and the DOM
// construction helpers in /shared/dom.js.

import * as store from '/shared/store.js';
import { $, html, frag } from '/shared/dom.js';
import { toast, showError } from './ui.js';
import { confirmDialog, promptDialog, openOverlay } from '/shared/dialogs.js';
import { renderGrid, gridNav, setGridView } from './grid.js';
import { openEventDialog } from './event-dialog.js';
import { renderInvites, toggleInvites } from './invites.js';
import { renderTasks } from './tasks.js';
import { renderContacts } from './contacts.js';
import { renderSettings } from './settings.js';
import * as pushSync from './push-sync.js';

// ---- routing -------------------------------------------------------------

let currentView = 'calendar';

function route() {
  const target = (location.hash.replace('#', '') || 'calendar').split('/')[0];
  currentView = ['calendar', 'tasks', 'contacts', 'settings'].includes(target) ? target : 'calendar';
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

// showCalendarView switches to the calendar view programmatically — used by
// the invitations panel's "jump to event" button, which can fire from the
// contacts or settings view. Routing keys off the hash, so setting it drives
// currentView, the chrome, and the re-render.
export function showCalendarView() {
  if (currentView !== 'calendar') location.hash = 'calendar';
}

export function render() {
  // Don't render (and don't let the grid kick off a range fetch) until the
  // app chrome is actually shown — bus events like 'prefs' can fire during
  // login, before calendars have loaded.
  if ($('#topbar').hidden) return;
  const main = $('#content');
  if (currentView === 'calendar') renderGrid(main);
  else if (currentView === 'tasks') renderTasks(main);
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
  const name = await promptDialog('Name for the new calendar', { confirmLabel: 'Create' });
  if (!name) return;
  try {
    await store.createCalendar(name.toLowerCase().replace(/[^a-z0-9-]+/g, '-'), name, '');
    toast(`Calendar “${name}” created.`);
  } catch (e) { showError(e); }
});

function renderSidebar() {
  const ul = $('#cal-list');
  ul.innerHTML = '';
  // calweb is an events app; a task/journal-only collection has nothing to show
  // here, so keep it out of the calendar list entirely.
  for (const cal of store.state.calendars.filter(c => c.supportsEvents)) {
    const li = html`<li>
      <label><input type="checkbox" ${store.state.hidden.has(cal.href) ? '' : 'checked'}>
        <span class="dot" style="background:${store.calColor(cal.href)}"></span>
        <span class="name">${cal.displayName || cal.name}</span>
        ${cal.readOnly && frag`<span class="ro-badge" title="Shared with you (read-only)">🔒</span>`}</label>
      <button class="gear" title="${cal.readOnly ? 'Shared calendar' : 'Calendar settings'}">⚙</button>
    </li>`;
    $('input', li).addEventListener('change', ev => {
      store.setCalendarHidden(cal.href, !ev.target.checked);
    });
    $('.gear', li).addEventListener('click', () => calendarMenu(cal));
    ul.append(li);
  }
}

function calendarMenu(cal) {
  closeMenus();
  const ro = cal.readOnly;
  const dialog = html`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true">
    <h2>${cal.displayName || cal.name}</h2>
    ${ro && frag`<p class="note">🔒 This calendar is shared with you, read-only. You can hide it or remove it from your list, but not edit its events or settings.</p>`}
    <div class="grid">
      <label>Name<input id="cm-name" value="${cal.displayName || cal.name}" ${ro ? 'disabled' : ''}></label>
      <label>Color<input id="cm-color" type="color" value="${store.calColor(cal.href)}" ${ro ? 'disabled' : ''}></label>
    </div>
    ${!ro && frag`<section class="share">
      <h3>Shared with</h3>
      <div id="cm-sharees" class="sharee-list"><p class="note">Loading…</p></div>
      <p class="note">Invite someone by email. They'll see the calendar in their own list and get an invitation to accept.</p>
      <div class="share-row">
        <input id="cm-share-email" type="email" placeholder="person@example.org" autocomplete="off">
        <label class="row"><input type="checkbox" id="cm-share-rw"> Can edit</label>
      </div>
      <div class="share-actions">
        <button class="primary" id="cm-share">Share</button>
      </div>
    </section>`}
    <div class="actions">
      <button class="danger" id="cm-delete">${ro ? 'Remove from my calendars' : 'Delete calendar…'}</button>
      <button id="cm-cancel">${ro ? 'Close' : 'Cancel'}</button>
      ${!ro && frag`<button class="primary" id="cm-save">Save</button>`}
    </div>
  </div></div>`;
  const close = () => dialog.remove();
  openOverlay(dialog, close);
  $('#cm-cancel', dialog).addEventListener('click', close);
  $('#cm-save', dialog)?.addEventListener('click', async () => {
    try {
      await store.updateCalendar(cal.href, {
        displayName: $('#cm-name', dialog).value,
        color: $('#cm-color', dialog).value,
      });
      close();
    } catch (e) { showError(e); }
  });

  // Sharing (owned calendars only): the live roster plus an invite form.
  const shareEmail = () => $('#cm-share-email', dialog).value.trim();
  async function refreshSharees() {
    const box = $('#cm-sharees', dialog);
    if (!box) return;
    try {
      const sharees = await store.loadSharees(cal.href);
      box.innerHTML = '';
      if (!sharees.length) { box.append(html`<p class="note">Not shared with anyone yet.</p>`); return; }
      for (const s of sharees) {
        const detail = (s.readWrite ? 'can edit' : 'read-only') +
          (s.status === 'pending' ? ' · pending' : s.status === 'declined' ? ' · declined' : '');
        const row = html`<div class="sharee">
          <span class="who">${s.commonName || s.email}</span>
          <span class="lvl">${detail}</span>
          <button class="ghost" data-remove aria-label="Remove ${s.email}">Remove</button>
        </div>`;
        $('[data-remove]', row).addEventListener('click', async ev => {
          ev.target.disabled = true;
          try { await store.unshareCalendar(cal.href, s.email); toast(`Removed ${s.email}.`); await refreshSharees(); }
          catch (e) { showError(e); ev.target.disabled = false; }
        });
        box.append(row);
      }
    } catch {
      box.innerHTML = '';
      box.append(html`<p class="note">Couldn’t load the sharing list.</p>`);
    }
  }
  $('#cm-share', dialog)?.addEventListener('click', async () => {
    const email = shareEmail();
    if (!email || !email.includes('@')) { toast('Enter an email address to share with.', true); return; }
    try {
      await store.shareCalendar(cal.href, { email, readWrite: $('#cm-share-rw', dialog).checked });
      toast(`Shared with ${email}.`);
      $('#cm-share-email', dialog).value = '';
      $('#cm-share-rw', dialog).checked = false;
      await refreshSharees();
    } catch (e) { showError(e); }
  });
  if (!ro) refreshSharees();

  $('#cm-delete', dialog).addEventListener('click', async () => {
    const name = cal.displayName || cal.name;
    const ok = ro
      ? await confirmDialog(`Remove “${name}” from your calendars? The owner keeps it; you can be re-invited later.`,
        { confirmLabel: 'Remove', danger: true })
      : await confirmDialog(`Delete “${name}” and all its events? This cannot be undone.`,
        { confirmLabel: 'Delete', danger: true });
    if (!ok) return;
    try { await store.deleteCalendar(cal.href); close(); } catch (e) { showError(e); }
  });
  (ro ? $('#cm-cancel', dialog) : $('#cm-name', dialog)).focus();
}

// ---- store subscriptions -------------------------------------------------

store.bus.addEventListener('calendars', renderSidebar);
// WebDAV-Push: refresh instantly on an incoming push, and keep the browser's
// subscriptions in step with the user's calendars (a no-op unless enabled).
pushSync.wirePushRefresh();
store.bus.addEventListener('calendars', () => pushSync.syncSubscriptions());
store.bus.addEventListener('events', () => { if (currentView === 'calendar') render(); });
store.bus.addEventListener('tasks', () => { if (currentView === 'tasks') render(); });
// The tasks view re-renders on calendar refreshes too: renderTasks calls the
// ctag-guarded loadTasks, so the 30 s poll picks up remote task changes.
store.bus.addEventListener('calendars', () => { if (currentView === 'tasks') render(); });
store.bus.addEventListener('contacts', () => { if (currentView === 'contacts') render(); });
// The bell counts scheduling invitations plus still-unanswered calendar shares.
function updateBell() {
  // Only actionable schedule items count: REQUESTs awaiting a reply and CANCELs
  // awaiting removal. REPLY/other cards are dismiss-only notifications and would
  // otherwise inflate the badge with nothing to act on.
  const pending = store.state.invites
    .filter(i => i.method === 'REQUEST' || i.method === 'CANCEL').length;
  const n = pending +
    store.state.shareNotifications.filter(s => s.status === 'noresponse').length;
  const badge = $('#bell-badge');
  badge.hidden = n === 0;
  badge.textContent = n > 9 ? '9+' : n;
  renderInvites();
}
store.bus.addEventListener('invites', updateBell);
store.bus.addEventListener('sharenotifs', updateBell);
// The open panel derives conflict lines and its empty-state pointer from the
// instance cache — refresh it when that cache changes.
store.bus.addEventListener('events', () => {
  if (!$('#invites-panel').hidden) renderInvites();
});
store.bus.addEventListener('prefs', render);

document.addEventListener('keydown', e => {
  if (e.key === 'Escape') {
    const overlays = document.querySelectorAll('#dialog-host .overlay');
    if (overlays.length) {
      // Close the topmost dialog via its registered handler (see openOverlay)
      // so promise-based dialogs resolve; fall back to removal for overlays
      // opened without one.
      const top = overlays[overlays.length - 1];
      if (typeof top.close === 'function') top.close(); else top.remove();
    }
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
