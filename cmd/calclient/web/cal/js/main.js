// main.js — calweb boot: login flow, top bar, sidebar, view routing. The
// app-level helpers (toast, formatting) live in ./ui.js and the DOM
// construction helpers in /shared/dom.js.

import * as store from '/shared/store.js';
import { $, html, frag } from '/shared/dom.js';
import { icon } from '/shared/icons.js';
import { toast, showError, enableOnValid, busyWhile } from './ui.js';
import { confirmDialog, promptDialog, openOverlay, submitOnEnter } from '/shared/dialogs.js';
import { renderGrid, gridNav, setGridView, applyCalendarRoute, currentCalendarRoute } from './grid.js';
import { openEventDialog } from './event-dialog.js';
import { openEventDetail } from './event-detail.js';
import * as router from './router.js';
import { renderInvites, toggleInvites } from './invites.js';
import { renderTasks } from './tasks.js';
import { renderContacts } from './contacts.js';
import { renderSettings, updateImportProgress } from './settings.js';
import * as pushSync from './push-sync.js';

// ---- routing -------------------------------------------------------------
//
// The URL is the source of truth (see router.js): a section path picks the
// chrome, /cal/calendar/… feeds the grid its view+date+filter, and /cal/event/…
// mounts a detail or editor overlay over the calendar. route() runs for every
// navigation (link click, popstate, or programmatic router.navigate).

let currentView = 'calendar';   // the section behind any overlay
let eventOverlay = null;        // the mounted event overlay element, if any
let overlayKey = null;          // identifies which event route it represents

function route(r = router.parseRoute()) {
  // Bus events (prefs/calendars) can fire during login, before the chrome is up.
  if ($('#topbar').hidden) return;
  if (r.name === 'event') { routeEvent(r); return; }

  closeEventOverlay();
  setSection(r.name);
  if (r.name === 'calendar') {
    applyCalendarRoute({ view: r.view, date: r.date, q: r.q });
    // Canonicalize when the path is bare (/cal, /cal/calendar) or the shown view
    // differs from what the URL asked — e.g. a narrow screen coerced week→day —
    // so the address bar stays shareable and matches the grid (replace only, no
    // re-render).
    const cur = currentCalendarRoute();
    if (!r.view || cur.view !== r.view) router.replaceUrl(router.calendarPath(cur));
  } else {
    render();
  }
}

// setSection switches the top-level chrome. The CSS keys off body.dataset.view
// to hide the calendar sidebar and date/view controls on non-calendar sections.
function setSection(name) {
  currentView = name;
  document.body.dataset.view = name;
  for (const a of document.querySelectorAll('#mainnav a')) {
    a.classList.toggle('active', a.dataset.nav === name);
  }
  closeMenus();
}

// routeEvent draws the calendar behind, then mounts the detail or editor overlay
// for the event the URL names. Closing the overlay navigates back to the
// calendar path, which re-enters route() as a section route and removes it.
async function routeEvent(r) {
  setSection('calendar');
  render();   // calendar behind the overlay (also kicks a range load on cold open)
  const kkey = `${r.uid}|${r.edit ? 'edit' : 'view'}|${r.occ || ''}`;
  if (overlayKey === kkey && eventOverlay && document.body.contains(eventOverlay)) return;
  closeEventOverlay();
  overlayKey = kkey;
  const back = () => router.navigate(router.calendarPath(currentCalendarRoute()), { replace: true });
  let inst;
  try { inst = await store.resolveEvent(r.uid, r.occ); }
  catch (e) { showError(e); overlayKey = null; back(); return; }
  if (overlayKey !== kkey) return;   // a newer navigation superseded this one
  if (!inst) { toast('That event could no longer be found.'); overlayKey = null; back(); return; }
  const overlay = r.edit ? await openEventDialog(inst, null, null, back) : openEventDetail(inst, back);
  // openEventDialog awaits the stored object; a navigation during that load may
  // have superseded us (openOverlay already mounted the element). Don't adopt an
  // overlay for a route that's no longer current — tear the stray one down.
  if (overlayKey !== kkey) { if (overlay) overlay.remove(); return; }
  eventOverlay = overlay;
  if (!eventOverlay) { overlayKey = null; back(); }   // editor bailed (e.g. load failed)
}

function closeEventOverlay() {
  if (eventOverlay) {
    eventOverlay.remove();
    // Return focus to whatever opened the overlay (see openOverlay) — a
    // direct remove() skips the close() path the router normally takes.
    eventOverlay._restoreFocus?.();
    eventOverlay = null;
  }
  overlayKey = null;
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

router.onRoute(route);
router.interceptLinks();
$('#nav-today').addEventListener('click', () => gridNav(0));
$('#nav-prev').addEventListener('click', () => gridNav(-1));
$('#nav-next').addEventListener('click', () => gridNav(1));
for (const b of document.querySelectorAll('#view-seg button')) {
  b.addEventListener('click', () => setGridView(b.dataset.view));
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
  // Wire drag-reordering once (the <ul> persists across re-renders): while a
  // calendar is dragged, slot it before whichever row the pointer is over.
  if (!ul.__dndWired) {
    ul.__dndWired = true;
    ul.addEventListener('dragover', ev => {
      const dragging = ul.querySelector('li.dragging');
      if (!dragging) return;
      ev.preventDefault();
      const after = [...ul.querySelectorAll('li:not(.dragging)')]
        .find(li => ev.clientY < li.getBoundingClientRect().top + li.offsetHeight / 2);
      if (after) ul.insertBefore(dragging, after);
      else ul.append(dragging);
    });
  }
  // calweb is an events app; a task/journal-only collection has nothing to show
  // here, so keep it out of the calendar list entirely.
  for (const cal of store.state.calendars.filter(c => c.supportsEvents)) {
    const li = html`<li data-href="${cal.href}">
      <label><input type="checkbox" ${store.state.hidden.has(cal.href) ? '' : 'checked'}>
        <span class="dot" style="background:${store.calColor(cal.href)}"></span>
        <span class="name">${cal.displayName || cal.name}</span>
        ${cal.delegatedFrom && frag`<span class="type-tag" title="Delegated by ${cal.delegatedFrom}">${cal.delegatedFrom}</span>`}
        ${cal.readOnly && frag`<span class="ro-badge" title="Shared with you (read-only)">${icon('lock', 12)}</span>`}</label>
      ${!cal.delegatedFrom && frag`<button class="gear" title="${cal.readOnly ? 'Shared calendar' : 'Calendar settings'}">${icon('gear', 14)}</button>`}
    </li>`;
    $('input', li).addEventListener('change', ev => {
      store.setCalendarHidden(cal.href, !ev.target.checked);
    });
    // Drag to reorder my own calendars (calendar-order); delegated ones aren't
    // draggable (I can't set properties on someone else's calendar).
    if (!cal.delegatedFrom) {
      li.draggable = true;
      li.addEventListener('dragstart', () => li.classList.add('dragging'));
      li.addEventListener('dragend', async () => {
        li.classList.remove('dragging');
        const order = [...ul.children].map(x => x.dataset.href);
        try { await store.reorderCalendars(order); } catch (e) { showError(e); }
      });
    }
    // A delegated calendar has no settings gear — you can't manage someone
    // else's calendar's properties.
    $('.gear', li)?.addEventListener('click', () => calendarMenu(cal));
    ul.append(li);
  }
}

function calendarMenu(cal) {
  closeMenus();
  const ro = cal.readOnly;
  const dialog = html`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true">
    <h2>${cal.displayName || cal.name}</h2>
    ${ro && frag`<p class="note">${icon('lock', 14)} This calendar is shared with you, read-only. You can hide it or remove it from your list, but not edit its events or settings.</p>`}
    <div class="grid">
      <label>Name<input id="cm-name" value="${cal.displayName || cal.name}" ${ro ? 'disabled' : ''}></label>
      <label>Color<input id="cm-color" type="color" value="${store.calColor(cal.href)}" ${ro ? 'disabled' : ''}></label>
      <label class="full">Description<textarea id="cm-desc" rows="2" placeholder="What this calendar is for (optional)" ${ro ? 'disabled' : ''}>${cal.description ?? ''}</textarea></label>
    </div>
    ${!ro && frag`<label class="row cm-private"><input type="checkbox" id="cm-private" ${cal.private ? 'checked' : ''}>
      <span>Private — hide this calendar's busy times from other people's free/busy and scheduling</span></label>`}
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
  // Enter saves — except in the share-email field, whose Enter runs the Share
  // action it sits next to (a read-only share has no Save; Enter does nothing).
  const saveBtn = $('#cm-save', dialog);
  if (saveBtn) submitOnEnter(dialog, saveBtn, t => t.id === 'cm-share-email');
  $('#cm-share-email', dialog)?.addEventListener('keydown', ev => {
    if (ev.key === 'Enter') { ev.preventDefault(); $('#cm-share', dialog).click(); }
  });
  $('#cm-cancel', dialog).addEventListener('click', close);
  saveBtn?.addEventListener('click', busyWhile(saveBtn, async () => {
    try {
      await store.updateCalendar(cal.href, {
        displayName: $('#cm-name', dialog).value,
        color: $('#cm-color', dialog).value,
        private: $('#cm-private', dialog).checked,
        description: $('#cm-desc', dialog).value,
      });
      close();
    } catch (e) { showError(e); }
  }));

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
          if (!await confirmDialog(`Stop sharing “${cal.displayName || cal.name}” with ${s.email}? They lose access immediately.`,
            { confirmLabel: 'Remove', danger: true })) return;
          ev.target.disabled = true;
          ev.target.classList.add('is-busy');
          try { await store.unshareCalendar(cal.href, s.email); toast(`Removed ${s.email}.`); await refreshSharees(); }
          catch (e) { showError(e); ev.target.disabled = false; ev.target.classList.remove('is-busy'); }
        });
        box.append(row);
      }
    } catch {
      box.innerHTML = '';
      box.append(html`<p class="note">Couldn’t load the sharing list.</p>`);
    }
  }
  const shareBtn = $('#cm-share', dialog);
  const shareInput = $('#cm-share-email', dialog);
  if (shareBtn && shareInput) {
    // The button only lights up once the field holds a plausible address, so an
    // empty or half-typed value can't be submitted (and Enter no-ops too).
    enableOnValid(shareInput, shareBtn, v => /.+@.+/.test(v.trim()));
    shareBtn.addEventListener('click', busyWhile(shareBtn, async () => {
      const email = shareEmail();
      try {
        await store.shareCalendar(cal.href, { email, readWrite: $('#cm-share-rw', dialog).checked });
        toast(`Shared with ${email}.`);
        shareInput.value = '';
        $('#cm-share-rw', dialog).checked = false;
        await refreshSharees();
      } catch (e) { showError(e); }
    }));
  }
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
// Import runs in the store, so its progress bar survives view switches: update
// the Settings panel in place (a no-op when it isn't mounted) rather than
// re-rendering, so the file input and focus are left alone.
store.bus.addEventListener('import', () => { if (currentView === 'settings') updateImportProgress(); });
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
