// contacts.js — the contacts view: searchable list + a full add/edit dialog
// (name, org/title, repeatable emails & phones, address, website, birthday,
// notes). Fields the editor doesn't model are preserved on save by the store.

import * as store from '/shared/store.js';
import { $, html, frag, brJoin } from '/shared/dom.js';
import { toast, showError } from './ui.js';
import { confirmDialog, promptDialog, openOverlay } from '/shared/dialogs.js';

let filter = '';
// Sidebar selection: {type:'all'} | {type:'book', href} | {type:'group', href}.
let selection = { type: 'all' };
let mount = null;

const EMAIL_TYPES = ['', 'HOME', 'WORK', 'OTHER'];
const TEL_TYPES = ['CELL', 'HOME', 'WORK', 'FAX', 'OTHER'];

export function renderContacts(main) {
  mount = main;
  main.innerHTML = '';
  const books = store.state.books;
  const groups = store.state.groups;

  // Forget a selection whose book/group has since disappeared.
  if (selection.type === 'book' && !books.some(b => b.href === selection.href)) selection = { type: 'all' };
  if (selection.type === 'group' && !groups.some(g => g.href === selection.href)) selection = { type: 'all' };

  const el = html`<div class="contacts contacts-2col">
    <aside class="ct-side" aria-label="Address books and groups"></aside>
    <div class="ct-main">
      <div class="bar">
        <input id="ct-search" type="search" placeholder="Search contacts…" value="${filter}">
        <button class="primary" id="ct-add">＋ New contact</button>
      </div>
      <div id="ct-rows"></div>
    </div>
  </div>`;
  main.append(el);

  buildSidebar($('.ct-side', el), books, groups);
  $('#ct-search', el).addEventListener('input', ev => {
    filter = ev.target.value;
    renderRows($('#ct-rows', el));
  });
  $('#ct-add', el).addEventListener('click', () => contactDialog(null));
  renderRows($('#ct-rows', el));
}

// buildSidebar renders the address-book list (with an "All Contacts" entry) and,
// below it, any contact groups. Each heading carries a ＋ to create one; each
// book/group (not "All Contacts") reveals a × to delete it.
function buildSidebar(side, books, groups) {
  side.innerHTML = '';
  const heading = (title, onAdd) => {
    const h = html`<div class="ct-side-h"><span>${title}</span></div>`;
    const add = html`<button type="button" class="ct-side-add" aria-label="Add ${title}" title="Add ${title}">＋</button>`;
    add.addEventListener('click', onAdd);
    h.append(add);
    side.append(h);
  };
  const item = (label, count, active, onClick, cls, { onRename, onDelete } = {}) => {
    const btn = html`<button type="button" class="ct-nav ${cls}${active ? ' active' : ''}">
      <span class="ct-nav-label">${label}</span>
      ${count != null && frag`<span class="ct-nav-count">${count}</span>`}
    </button>`;
    btn.addEventListener('click', onClick);
    if (!onRename && !onDelete) { side.append(btn); return; }
    // A button can't nest a button, so the row controls are siblings.
    const row = html`<div class="ct-nav-row"></div>`;
    row.append(btn);
    if (onRename) {
      const ren = html`<button type="button" class="ct-nav-act" aria-label="Rename ${label}" title="Rename">✎</button>`;
      ren.addEventListener('click', ev => { ev.stopPropagation(); onRename(); });
      row.append(ren);
    }
    if (onDelete) {
      const del = html`<button type="button" class="ct-nav-act ct-nav-del" aria-label="Delete ${label}" title="Delete">×</button>`;
      del.addEventListener('click', ev => { ev.stopPropagation(); onDelete(); });
      row.append(del);
    }
    side.append(row);
  };

  heading('Address Books', addBookFlow);
  item('All Contacts', store.state.contacts.length, selection.type === 'all',
    () => select({ type: 'all' }), 'ct-book');
  for (const book of books) {
    const count = store.state.contacts.filter(c => c.book === book.href).length;
    item(book.displayName, count,
      selection.type === 'book' && selection.href === book.href,
      () => select({ type: 'book', href: book.href }), 'ct-book',
      { onRename: () => renameBookFlow(book), onDelete: () => deleteBookFlow(book, count) });
  }

  heading('Groups', addGroupFlow);
  for (const g of groups) {
    item(g.fn || 'Group', g.members.length,
      selection.type === 'group' && selection.href === g.href,
      () => select({ type: 'group', href: g.href }), 'ct-group',
      { onRename: () => renameGroupFlow(g), onDelete: () => deleteGroupFlow(g) });
  }
}

async function addBookFlow() {
  const name = await promptDialog('Name for the new address book', { confirmLabel: 'Create' });
  if (!name?.trim()) return;
  try { await store.createBook(name.trim()); toast(`Address book “${name.trim()}” created.`); }
  catch (e) { showError(e); }
}

async function renameBookFlow(book) {
  const name = await promptDialog('Rename address book', { confirmLabel: 'Rename', value: book.displayName });
  const trimmed = name?.trim();
  if (!trimmed || trimmed === book.displayName) return;
  try { await store.renameBook(book.href, trimmed); toast('Address book renamed.'); }
  catch (e) { showError(e); }
}

async function deleteBookFlow(book, count) {
  const msg = count
    ? `Delete “${book.displayName}” and its ${count} contact${count === 1 ? '' : 's'}? This cannot be undone.`
    : `Delete “${book.displayName}”?`;
  if (!await confirmDialog(msg, { confirmLabel: 'Delete', danger: true })) return;
  if (selection.type === 'book' && selection.href === book.href) selection = { type: 'all' };
  try { await store.deleteBook(book.href); toast('Address book deleted.'); }
  catch (e) { showError(e); }
}

async function addGroupFlow() {
  const name = await promptDialog('Name for the new group', { confirmLabel: 'Create' });
  if (!name?.trim()) return;
  try {
    await store.createGroup(name.trim(), selection.type === 'book' ? selection.href : undefined);
    toast(`Group “${name.trim()}” created.`);
  } catch (e) { showError(e); }
}

async function renameGroupFlow(g) {
  const name = await promptDialog('Rename group', { confirmLabel: 'Rename', value: g.fn });
  const trimmed = name?.trim();
  if (!trimmed || trimmed === g.fn) return;
  try { await store.renameGroup(g.href, trimmed); toast('Group renamed.'); }
  catch (e) { showError(e); }
}

async function deleteGroupFlow(g) {
  if (!await confirmDialog(`Delete the group “${g.fn || 'Group'}”? The contacts in it aren't deleted.`,
    { confirmLabel: 'Delete', danger: true })) return;
  if (selection.type === 'group' && selection.href === g.href) selection = { type: 'all' };
  try { await store.deleteGroup(g.href); toast('Group deleted.'); }
  catch (e) { showError(e); }
}

function select(sel) {
  selection = sel;
  if (mount) renderContacts(mount);
}

// visibleContacts narrows the flat contact list to the current sidebar
// selection: a single book, or a group's members (matched by UID).
function visibleContacts() {
  const all = store.state.contacts;
  if (selection.type === 'book') return all.filter(c => c.book === selection.href);
  if (selection.type === 'group') {
    const g = store.state.groups.find(x => x.href === selection.href);
    const members = new Set(g?.members || []);
    return all.filter(c => members.has(store.normalizeUID(c.uid)));
  }
  return all;
}

function renderRows(box) {
  box.innerHTML = '';
  const q = filter.trim().toLowerCase();
  const scoped = visibleContacts();
  const rows = scoped.filter(c => !q ||
    c.fn.toLowerCase().includes(q) ||
    c.org?.toLowerCase().includes(q) ||
    c.emails.some(e => e.value.toLowerCase().includes(q)));
  if (!rows.length) {
    const msg = scoped.length ? 'No contacts match your search.'
      : selection.type === 'all' ? 'No contacts yet. Add your first one!'
        : 'No contacts here.';
    box.append(html`<p class="empty">${msg}</p>`);
    return;
  }
  for (const c of rows) {
    const sub = [c.emails[0]?.value, c.tels[0]?.value, c.org].filter(Boolean).join(' · ');
    const row = html`<button class="contact" type="button">
      <span class="avatar" aria-hidden="true">${initials(c.fn)}</span>
      <span class="who"><div>${c.fn}${c.title && frag` <span class="mail">— ${c.title}</span>`}</div>
        <div class="mail">${sub}</div></span>
      <span class="chev" aria-hidden="true">›</span>
    </button>`;
    row.addEventListener('click', () => contactDetail(c));
    box.append(row);
  }
}

function initials(fn) {
  return fn.split(/\s+/).map(w => w[0]).slice(0, 2).join('').toUpperCase() || '?';
}

// contactDetail shows a read-only card with every stored field, plus Edit and
// Delete. Emails/phones/website are actionable links.
function contactDetail(c) {
  const rows = [];
  const line = (label, value) => rows.push(
    frag`<div class="detail-row"><span class="dl">${label}</span><span class="dv">${value}</span></div>`);
  const typeLabel = t => t && frag`<span class="type-tag">${t[0] + t.slice(1).toLowerCase()}</span>`;

  if (c.org || c.title) line('', [c.title, c.org].filter(Boolean).join(', '));
  for (const e of c.emails) line('Email', frag`<a href="mailto:${e.value}">${e.value}</a> ${typeLabel(e.type)}`);
  for (const t of c.tels) line('Phone', frag`<a href="tel:${t.value.replace(/\s+/g, '')}">${t.value}</a> ${typeLabel(t.type)}`);
  const a = c.adr || {};
  const addr = [a.street, [a.postal, a.city].filter(Boolean).join(' '), a.region, a.country].filter(Boolean);
  if (addr.length) line('Address', brJoin(addr));
  if (c.url) line('Website', frag`<a href="${c.url}" target="_blank" rel="noreferrer noopener">${c.url}</a>`);
  if (c.bday) line('Birthday', formatBday(c.bday));
  if (c.note) line('Notes', brJoin(c.note.split('\n')));
  const body = rows.length ? rows : frag`<p class="empty" style="margin:1rem 0">No other details.</p>`;

  const dlg = html`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="Contact details">
    <button class="dialog-close" id="cv-x" aria-label="Close">×</button>
    <div class="detail-head">
      <span class="avatar avatar-lg" aria-hidden="true">${initials(c.fn)}</span>
      <div><h2 style="margin:0">${c.fn}</h2>${(c.title || c.org)
        && frag`<div class="mail">${[c.title, c.org].filter(Boolean).join(' · ')}</div>`}</div>
    </div>
    <div class="detail-body">${body}</div>
    <div class="actions">
      <button class="danger" id="cv-del">Delete…</button>
      <button id="cv-edit">Edit</button>
      <button class="primary" id="cv-close">Done</button>
    </div>
  </div></div>`;
  const close = () => dlg.remove();
  openOverlay(dlg, close);
  $('#cv-x', dlg).addEventListener('click', close);
  $('#cv-close', dlg).addEventListener('click', close);
  $('#cv-edit', dlg).addEventListener('click', () => { close(); contactDialog(c); });
  $('#cv-del', dlg).addEventListener('click', async () => {
    if (!await confirmDialog(`Delete ${c.fn}?`, { confirmLabel: 'Delete', danger: true })) return;
    try { await store.deleteContact(c.href); close(); toast('Contact deleted.'); }
    catch (e) { showError(e); }
  });

  // Surface a merge path only on the handful of cards that actually have a
  // likely duplicate — no global banner, no permanent toolbar button.
  const dup = store.findDuplicatesOf(c, store.state.contacts);
  if (dup) {
    const others = dup.contacts.filter(x => x.href !== c.href);
    const label = others.length === 1
      ? `Possible duplicate of ${others[0].fn || 'another contact'}`
      : `Possible duplicate of ${others.length} contacts`;
    const banner = html`<div class="dup-banner">
      <span>${label}</span><button type="button" class="ghost">Review &amp; merge</button>
    </div>`;
    banner.querySelector('button').addEventListener('click', () => { close(); mergeDialog(dup); });
    $('.detail-body', dlg).prepend(banner);
  }
  $('#cv-close', dlg).focus();
}

// confidenceBadge labels a cluster: strong email/phone overlap ("Likely") vs a
// name-only match ("Possible").
function confidenceBadge(confidence) {
  return frag`<span class="type-tag">${confidence === 'high' ? 'Likely' : 'Possible'}</span>`;
}

const SCALAR_LABELS = { org: 'Organization', title: 'Job title', url: 'Website', bday: 'Birthday', note: 'Notes' };
const CONFLICT_LABELS = { fn: 'Name', ...SCALAR_LABELS };

// humanType renders a vCard TYPE (CELL/HOME/WORK) the way the rest of the UI
// does — capitalized, with CELL shown as the friendlier "Mobile".
function humanType(t) {
  if (!t) return '';
  return t.toUpperCase() === 'CELL' ? 'Mobile' : t[0] + t.slice(1).toLowerCase();
}

// Friendly names for the vCard properties the editor doesn't model but the
// merge still carries through (see mergePreview's `preserve`).
const PRESERVED_LABELS = {
  PHOTO: 'Photo', LOGO: 'Logo', CATEGORIES: 'Categories', NICKNAME: 'Nickname',
  IMPP: 'Instant messaging', ADR: 'Additional address', GEO: 'Location',
  ANNIVERSARY: 'Anniversary', ROLE: 'Role', RELATED: 'Related', LANG: 'Language',
  SOUND: 'Sound', KEY: 'Key', TZ: 'Time zone', GENDER: 'Gender',
};

// preservedLabels turns the carried-through property lines into a deduped,
// human list (with a count when a property repeats), so the preview can show
// that nothing beyond the modeled fields is silently dropped.
function preservedLabels(lines) {
  const counts = new Map();
  for (const l of lines) {
    const name = (/^([^;:]+)/.exec(l)?.[1] || '').toUpperCase();
    if (!name) continue;
    const label = PRESERVED_LABELS[name]
      || (name.startsWith('X-') ? name : name[0] + name.slice(1).toLowerCase());
    counts.set(label, (counts.get(label) || 0) + 1);
  }
  return [...counts].map(([label, n]) => (n > 1 ? `${label} (${n})` : label));
}

// previewPanel renders the card a merge would produce. Fields the cluster
// disagrees on (name, org, …) become a <select> (class mg-conflict) so the user
// picks the winner; everything else — including the unioned emails and phones,
// each with its Home/Mobile/Work type — is shown read-only.
function previewPanel(p) {
  const el = html`<div class="mg-preview"><div class="fg-head"><span>Merged result</span></div></div>`;
  const line = (label, value) => el.append(
    html`<div class="detail-row"><span class="dl">${label}</span><span class="dv">${value}</span></div>`);
  const tag = t => t && frag` <span class="type-tag">${humanType(t)}</span>`;
  const conflicted = new Set(p.conflicts.map(c => c.field));
  if (!conflicted.has('fn')) line('Name', p.fn);
  for (const e of p.emails) line('Email', frag`${e.value}${tag(e.type)}`);
  for (const t of p.tels) line('Phone', frag`${t.value}${tag(t.type)}`);
  for (const c of p.conflicts) {
    // The <select> is a live element, so append it into the row's value cell
    // rather than interpolating it (the template engine would stringify it).
    const row = html`<div class="detail-row"><span class="dl">${CONFLICT_LABELS[c.field] || c.field}</span><span class="dv"></span></div>`;
    row.querySelector('.dv').append(
      html`<select class="mg-conflict" data-field="${c.field}">${
        c.values.map(v => frag`<option value="${v}" ${v === p[c.field] ? 'selected' : ''}>${v}</option>`)}</select>`);
    el.append(row);
  }
  for (const [field, label] of Object.entries(SCALAR_LABELS)) {
    if (conflicted.has(field) || !p[field]) continue;
    line(label, field === 'note' ? brJoin(p[field].split('\n')) : p[field]);
  }
  const a = p.adr || {};
  const addr = [a.street, [a.postal, a.city].filter(Boolean).join(' '), a.region, a.country].filter(Boolean);
  if (addr.length) line('Address', brJoin(addr));
  // Everything the editor doesn't model (photo, categories, X-*, a second
  // address, …) is merged too — name it so nothing looks dropped.
  const kept = preservedLabels(p.preserve);
  if (kept.length) line('Also kept', frag`<span class="mail">${kept.join(', ')}</span>`);
  return el;
}

// mergeDialog is the shared review dialog for one duplicate cluster, opened from
// the per-contact affordance or the Settings sweep. Resolves true if a merge
// was performed (so the sweep can refresh), false on any dismissal.
function mergeDialog(cluster) {
  return new Promise(resolve => {
    const preview = store.mergePreview(cluster);
    const n = preview.losers.length;
    const dlg = html`<div class="overlay"><div class="dialog dialog-wide" role="dialog" aria-modal="true" aria-label="Merge duplicates">
      <h2>Merge ${cluster.contacts.length} contacts ${confidenceBadge(cluster.confidence)}</h2>
      <p class="note">Their emails, phones and addresses are combined into one card.${
        preview.conflicts.length ? ' Choose a value where the contacts disagree.' : ''}</p>
      <div id="mg-body"></div>
      <p class="error" id="mg-error"></p>
      <div class="actions">
        <button id="mg-cancel">Cancel</button>
        <button class="primary" id="mg-merge">Merge…</button>
      </div>
    </div></div>`;
    // While the merge is in flight the dialog stays open and locked — a spinner
    // on the button, and scrim/Escape/Cancel ignored — so its several
    // round-trips can't be interrupted half-applied.
    let busy = false;
    const done = merged => { dlg.remove(); resolve(merged); };
    openOverlay(dlg, () => { if (!busy) done(false); });
    $('#mg-body', dlg).append(previewPanel(preview));

    const mergeBtn = $('#mg-merge', dlg), cancelBtn = $('#mg-cancel', dlg);
    cancelBtn.addEventListener('click', () => { if (!busy) done(false); });
    mergeBtn.addEventListener('click', async () => {
      // Apply the user's choice for each conflicting field. A chosen display
      // name also pulls its matching N (first/last) so they stay consistent.
      for (const sel of dlg.querySelectorAll('.mg-conflict')) preview[sel.dataset.field] = sel.value;
      const nm = preview.nameOptions.find(o => o.fn === preview.fn);
      if (nm) { preview.first = nm.first; preview.last = nm.last; }
      if (!await confirmDialog(`Merge these ${n + 1} contacts into one? The other ${n} will be deleted.`,
        { confirmLabel: 'Merge', danger: true })) return;
      busy = true;
      $('#mg-error', dlg).textContent = '';
      mergeBtn.disabled = cancelBtn.disabled = true;
      mergeBtn.classList.add('is-busy');
      mergeBtn.textContent = 'Merging…';
      try {
        await store.mergeContacts(preview, preview.losers);
        toast('Contacts merged.');
        done(true);
      } catch (e) {
        busy = false;
        mergeBtn.disabled = cancelBtn.disabled = false;
        mergeBtn.classList.remove('is-busy');
        mergeBtn.textContent = 'Merge…';
        if (e.conflict) $('#mg-error', dlg).textContent = e.message;
        else showError(e);
      }
    });
  });
}

// contactsDedupSweep is the whole-book cleanup opened from Settings: lists every
// duplicate cluster with a Review button, refreshing as merges complete.
export function contactsDedupSweep() {
  const dlg = html`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="Duplicate contacts">
    <button class="dialog-close" id="dd-x" aria-label="Close">×</button>
    <h2 id="dd-title">Duplicate contacts</h2>
    <div id="dd-list" class="dd-list"></div>
    <div class="actions"><button class="primary" id="dd-close">Done</button></div>
  </div></div>`;
  const close = () => dlg.remove();
  openOverlay(dlg, close);
  $('#dd-x', dlg).addEventListener('click', close);
  $('#dd-close', dlg).addEventListener('click', close);

  const list = $('#dd-list', dlg);
  const paint = () => {
    const clusters = store.findDuplicateClusters(store.state.contacts);
    // Count the cards that would be removed by merging every group (all but the
    // one kept per group) — the tangible "how many duplicates" number.
    const extra = clusters.reduce((n, cl) => n + cl.contacts.length - 1, 0);
    $('#dd-title', dlg).textContent = extra
      ? `Duplicate contacts — ${extra} in ${clusters.length} ${clusters.length === 1 ? 'group' : 'groups'}`
      : 'Duplicate contacts';
    list.innerHTML = '';
    if (!clusters.length) { list.append(html`<p class="empty">No duplicates found.</p>`); return; }
    for (const cl of clusters) {
      const row = html`<div class="dd-row">
        <span class="who"><div>${cl.contacts.map(c => c.fn || 'Unnamed').join(', ')}</div>
          <div class="mail">${cl.contacts.length} cards ${confidenceBadge(cl.confidence)}</div></span>
      </div>`;
      const btn = html`<button type="button">Review</button>`;
      btn.addEventListener('click', async () => { if (await mergeDialog(cl)) paint(); });
      row.append(btn);
      list.append(row);
    }
  };
  paint();
}

// formatBday renders YYYY-MM-DD / YYYYMMDD as a friendly date.
function formatBday(bday) {
  const m = /^(\d{4})-?(\d{2})-?(\d{2})/.exec(bday);
  if (!m) return bday;
  const d = new Date(+m[1], +m[2] - 1, +m[3]);
  return isNaN(d) ? bday : d.toLocaleDateString([], { year: 'numeric', month: 'long', day: 'numeric' });
}

// multiRow builds a repeatable value+type row for emails/phones.
function multiRow(container, { value = '', type = '' }, types, inputType) {
  const opts = types.map(t =>
    frag`<option value="${t}" ${t === type ? 'selected' : ''}>${t ? t[0] + t.slice(1).toLowerCase() : '—'}</option>`);
  const row = html`<div class="multi-row">
    <input type="${inputType}" class="mr-val" value="${value}">
    <select class="mr-type">${opts}</select>
    <button type="button" class="mr-del" aria-label="Remove">×</button>
  </div>`;
  row.querySelector('.mr-del').addEventListener('click', () => row.remove());
  container.append(row);
  return row;
}

function collectMulti(container) {
  return [...container.querySelectorAll('.multi-row')]
    .map(r => ({ value: r.querySelector('.mr-val').value.trim(), type: r.querySelector('.mr-type').value }))
    .filter(x => x.value);
}

function contactDialog(c) {
  const a = c?.adr ?? {};
  const dlg = html`<div class="overlay"><div class="dialog dialog-wide" role="dialog" aria-modal="true">
    <h2>${c ? 'Edit contact' : 'New contact'}</h2>
    <div class="grid">
      <label>First name<input id="cd-first" type="text" value="${c?.first ?? ''}"></label>
      <label>Last name<input id="cd-last" type="text" value="${c?.last ?? ''}"></label>
      <label class="full">Display name (optional)<input id="cd-fn" type="text" value="${c?.fn ?? ''}" placeholder="Defaults to first + last"></label>
      <label>Organization<input id="cd-org" type="text" value="${c?.org ?? ''}"></label>
      <label>Job title<input id="cd-title" type="text" value="${c?.title ?? ''}"></label>

      <div class="full field-group">
        <div class="fg-head"><span>Email</span><button type="button" id="cd-add-email" class="ghost">＋ Add email</button></div>
        <div id="cd-emails"></div>
      </div>
      <div class="full field-group">
        <div class="fg-head"><span>Phone</span><button type="button" id="cd-add-tel" class="ghost">＋ Add phone</button></div>
        <div id="cd-tels"></div>
      </div>

      <div class="full field-group">
        <div class="fg-head"><span>Address</span></div>
        <div class="grid">
          <label class="full">Street<input id="cd-street" type="text" value="${a.street ?? ''}"></label>
          <label>City<input id="cd-city" type="text" value="${a.city ?? ''}"></label>
          <label>Region / State<input id="cd-region" type="text" value="${a.region ?? ''}"></label>
          <label>Postal code<input id="cd-postal" type="text" value="${a.postal ?? ''}"></label>
          <label>Country<input id="cd-country" type="text" value="${a.country ?? ''}"></label>
        </div>
      </div>

      <label>Website<input id="cd-url" type="url" value="${c?.url ?? ''}" placeholder="https://"></label>
      <label>Birthday<input id="cd-bday" type="date" value="${bdayInput(c?.bday)}"></label>
      <label class="full">Notes<textarea id="cd-note" rows="2">${c?.note ?? ''}</textarea></label>
    </div>
    <p class="error" id="cd-error"></p>
    <div class="actions">
      <button id="cd-cancel">Cancel</button>
      <button class="primary" id="cd-save">Save</button>
    </div>
  </div></div>`;

  const q = sel => $(sel, dlg);
  const emailsBox = q('#cd-emails'), telsBox = q('#cd-tels');
  const emails = c?.emails?.length ? c.emails : [{ value: '', type: '' }];
  const tels = c?.tels?.length ? c.tels : [{ value: '', type: 'CELL' }];
  for (const e of emails) multiRow(emailsBox, e, EMAIL_TYPES, 'email');
  for (const t of tels) multiRow(telsBox, t, TEL_TYPES, 'tel');
  q('#cd-add-email').addEventListener('click', () => multiRow(emailsBox, { value: '', type: '' }, EMAIL_TYPES, 'email').querySelector('.mr-val').focus());
  q('#cd-add-tel').addEventListener('click', () => multiRow(telsBox, { value: '', type: 'CELL' }, TEL_TYPES, 'tel').querySelector('.mr-val').focus());

  const close = () => dlg.remove();
  openOverlay(dlg, close);
  q('#cd-cancel').addEventListener('click', close);
  q('#cd-save').addEventListener('click', async () => {
    const first = q('#cd-first').value.trim(), last = q('#cd-last').value.trim();
    const fn = q('#cd-fn').value.trim() || `${first} ${last}`.trim();
    if (!fn) { q('#cd-error').textContent = 'Please enter a name.'; return; }
    try {
      await store.saveContact({
        href: c?.href, etag: c?.etag, uid: c?.uid, preserve: c?.preserve,
        fn, first, last,
        org: q('#cd-org').value.trim(), title: q('#cd-title').value.trim(),
        emails: collectMulti(emailsBox), tels: collectMulti(telsBox),
        adr: {
          type: a.type || 'HOME',
          street: q('#cd-street').value.trim(), city: q('#cd-city').value.trim(),
          region: q('#cd-region').value.trim(), postal: q('#cd-postal').value.trim(),
          country: q('#cd-country').value.trim(),
        },
        url: q('#cd-url').value.trim(), bday: q('#cd-bday').value.trim(),
        note: q('#cd-note').value.trim(),
      }, !c && selection.type === 'book' ? selection.href : undefined);
      close();
      toast('Contact saved.');
    } catch (e) {
      if (e.conflict) q('#cd-error').textContent = e.message;
      else showError(e);
    }
  });
  q('#cd-first').focus();
}

// bdayInput normalizes a stored BDAY (often YYYYMMDD) to the date input's
// YYYY-MM-DD, leaving already-hyphenated or partial values alone.
function bdayInput(bday) {
  if (!bday) return '';
  const m = /^(\d{4})-?(\d{2})-?(\d{2})/.exec(bday);
  return m ? `${m[1]}-${m[2]}-${m[3]}` : '';
}
