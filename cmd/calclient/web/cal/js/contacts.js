// contacts.js — the contacts view: searchable list + a full add/edit dialog
// (name, org/title, repeatable emails & phones, address, website, birthday,
// notes). Fields the editor doesn't model are preserved on save by the store.

import * as store from '/shared/store.js';
import { icalUnescape, buildPhotoLine, isPhotoLine } from '/shared/ical.js';
import { $, html, frag, brJoin } from '/shared/dom.js';
import { icon } from '/shared/icons.js';
import { toast, showError } from './ui.js';
import { confirmDialog, promptDialog, openOverlay, submitOnEnter } from '/shared/dialogs.js';
import { newCollectionDialog } from './new-collection.js';

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
        <button class="primary" id="ct-add">${icon('plus', 14)} New contact</button>
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
// below it, any contact groups. Each heading carries an add (+) control; each
// book/group (not "All Contacts") reveals a × to delete it.
function buildSidebar(side, books, groups) {
  side.innerHTML = '';
  const heading = (title, onAdd) => {
    const h = html`<div class="ct-side-h"><span>${title}</span></div>`;
    const add = html`<button type="button" class="ct-side-add icon-btn" aria-label="Add ${title}" title="Add ${title}">${icon('plus', 14)}</button>`;
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
      const ren = html`<button type="button" class="ct-nav-act icon-btn" aria-label="Rename ${label}" title="Rename">${icon('pencil', 13)}</button>`;
      ren.addEventListener('click', ev => { ev.stopPropagation(); onRename(); });
      row.append(ren);
    }
    if (onDelete) {
      const del = html`<button type="button" class="ct-nav-act ct-nav-del icon-btn" aria-label="Delete ${label}" title="Delete">${icon('x', 13)}</button>`;
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
  const r = await newCollectionDialog({ title: 'New address book', withColor: false });
  if (!r?.name) return;
  try { await store.createBook(r.name); toast(`Address book “${r.name}” created.`); }
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
  const r = await newCollectionDialog({ title: 'New group', withColor: false });
  if (!r?.name) return;
  try {
    await store.createGroup(r.name, selection.type === 'book' ? selection.href : undefined);
    toast(`Group “${r.name}” created.`);
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
  // First load still in flight — show a spinner rather than a bare "no contacts"
  // (which would read as an empty address book).
  if (!store.state.contactsLoaded) {
    box.append(html`<p class="ct-loading"><span class="spinner" aria-hidden="true"></span>Loading contacts…</p>`);
    return;
  }
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
      <span class="who"><div>${c.fn}${c.title && frag` <span class="mail">— ${c.title}</span>`}</div>
        <div class="mail">${sub}</div></span>
      <span class="chev" aria-hidden="true">${icon('chevRight', 14)}</span>
    </button>`;
    row.prepend(avatarEl(c));
    row.addEventListener('click', () => contactDetail(c));
    box.append(row);
  }
}

function initials(fn) {
  return fn.split(/\s+/).map(w => w[0]).slice(0, 2).join('').toUpperCase() || '?';
}

// avatarEl renders a contact's photo as a round <img> when one is present,
// falling back to the initials chip. `extra` adds a modifier class (e.g.
// avatar-lg for the detail head).
function avatarEl(c, extra = '') {
  const cls = `avatar${extra ? ' ' + extra : ''}`;
  return c.photo
    ? html`<img class="${cls} avatar-img" src="${c.photo}" alt="" aria-hidden="true">`
    : html`<span class="${cls}" aria-hidden="true">${initials(c.fn)}</span>`;
}

// contactDetail shows a read-only card with every stored field, plus Edit and
// Delete. Emails/phones/website are actionable links.
async function contactDetail(c) {
  // During the brief window before the full-card pass finishes, the clicked
  // contact may be a list-pass stub missing address/notes/preserved props;
  // refetch it so the detail (and any later Edit) has the complete card.
  try { c = await store.ensureFullContact(c); } catch (e) { showError(e); return; }
  const rows = [];
  const line = (label, value) => rows.push(
    frag`<div class="detail-row"><span class="dl">${label}</span><span class="dv">${value}</span></div>`);
  const typeLabel = t => t && frag`<span class="type-tag">${t[0] + t.slice(1).toLowerCase()}</span>`;

  // Organization/title already appear under the name in the detail head, so
  // they're not repeated as a body row here.
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
  $('.detail-head', dlg).prepend(avatarEl(c, 'avatar-lg'));
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
    banner.querySelector('button').addEventListener('click', async () => {
      // mergePreview reads every field (incl. preserved props) of all cluster
      // members, so upgrade any list-pass stubs to full cards first.
      try {
        const full = await Promise.all(dup.contacts.map(x => store.ensureFullContact(x)));
        close();
        mergeDialog({ ...dup, contacts: full });
      } catch (e) { showError(e); }
    });
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

// Friendly names for the vCard properties the editor doesn't model but the
// merge still carries through (see mergePreview's `preserve`).
const PRESERVED_LABELS = {
  PHOTO: 'Photo', LOGO: 'Logo', CATEGORIES: 'Categories', NICKNAME: 'Nickname',
  IMPP: 'Instant messaging', ADR: 'Additional address', GEO: 'Location',
  ANNIVERSARY: 'Anniversary', ROLE: 'Role', RELATED: 'Related', LANG: 'Language',
  SOUND: 'Sound', KEY: 'Key', TZ: 'Time zone', GENDER: 'Gender',
};

// Friendly names for the Apple X- properties that ride along on cards exported
// from Contacts.app — the ones that otherwise show up as raw "X-ABLABEL".
const X_LABELS = {
  'X-ABLABEL': 'Custom label', 'X-ABADR': 'Address label',
  'X-ABRELATEDNAMES': 'Related name', 'X-ABDATE': 'Custom date',
  'X-SOCIALPROFILE': 'Social profile', 'X-ABSHOWAS': 'Show as',
};

// Preserved properties whose value is an opaque blob (base64) — summarize them
// rather than dumping the payload into the "Also kept" disclosure.
const OPAQUE_PRESERVED = new Set(['PHOTO', 'LOGO', 'SOUND', 'KEY']);

// prettyPreservedName turns a raw vCard property name into a human label. Apple
// groups related lines under an item prefix (item1.URL / item1.X-ABLABEL); strip
// it, then map known names and clean up remaining X-* ones.
function prettyPreservedName(rawName) {
  const name = String(rawName || '').replace(/^item\d+\./i, '').toUpperCase();
  if (PRESERVED_LABELS[name]) return PRESERVED_LABELS[name];
  if (X_LABELS[name]) return X_LABELS[name];
  if (name.startsWith('X-')) {
    return name.slice(2).replace(/[-_]+/g, ' ').toLowerCase().replace(/^./, s => s.toUpperCase());
  }
  return name ? name[0] + name.slice(1).toLowerCase() : String(rawName || '');
}

// Apple wraps custom label text as _$!<HomePage>!$_ — unwrap it for display.
function unwrapAppleLabel(v) {
  const m = /^_\$!<(.*)>!\$_$/.exec(v);
  return m ? m[1] : v;
}

// preservedLabels turns the carried-through property lines into a deduped,
// human list (with a count when a property repeats) — the "Also kept" summary
// that shows nothing beyond the modeled fields is silently dropped.
function preservedLabels(lines) {
  const counts = new Map();
  for (const l of lines) {
    const raw = /^([^;:]+)/.exec(l)?.[1] || '';
    if (!raw) continue;
    const label = prettyPreservedName(raw);
    counts.set(label, (counts.get(label) || 0) + 1);
  }
  return [...counts].map(([label, n]) => (n > 1 ? `${label} (${n})` : label));
}

// preservedDetails expands each carried-through vCard line into a readable
// {label, value} pair, so the "Also kept" disclosure can show what's actually in
// there instead of just naming the properties.
function preservedDetails(lines) {
  return lines.map(l => {
    const colon = l.indexOf(':');
    const raw = /^([^;:]+)/.exec(colon < 0 ? l : l.slice(0, colon))?.[1] || '';
    const name = raw.replace(/^item\d+\./i, '').toUpperCase();
    let value = colon < 0 ? '' : unwrapAppleLabel(icalUnescape(l.slice(colon + 1)));
    if (OPAQUE_PRESERVED.has(name)) value = '(embedded data)';
    else if (value.length > 120) value = value.slice(0, 120) + '…';
    return { label: prettyPreservedName(raw), value };
  });
}

// mergeMultiGroup renders an editable email/phone section (header + Add button +
// the unioned rows) into the merge preview, reusing the contact editor's
// multiRow. The union is often noisy — a near-duplicate the normalizer kept, or
// an entry whose type is wrong or missing — so the user can retype, remove, or
// add before the merge is written; each row's type dropdown keeps Mobile/Home/…
// visible and changeable. Returns the rows container for collectMulti().
function mergeMultiGroup(parent, label, items, types, inputType, addType) {
  const group = html`<div class="field-group mg-multi">
    <div class="fg-head"><span>${label}</span><button type="button" class="ghost">${icon('plus', 13)} Add ${label.toLowerCase()}</button></div>
    <div class="mg-rows"></div>
  </div>`;
  const box = group.querySelector('.mg-rows');
  for (const it of items) multiRow(box, it, types, inputType);
  group.querySelector('.fg-head button').addEventListener('click',
    () => multiRow(box, { value: '', type: addType }, types, inputType).querySelector('.mr-val').focus());
  parent.append(group);
  return box;
}

// previewPanel renders the card a merge would produce. Fields the cluster
// disagrees on (name, org, …) become a <select> (class mg-conflict) so the user
// picks the winner; the unioned emails and phones are editable rows (see
// mergeMultiGroup); the rest is read-only. The email/phone rows containers are
// stashed on the element so mergeDialog can collect the user's edits back.
function previewPanel(p) {
  const el = html`<div class="mg-preview"><div class="fg-head"><span>Merged result</span></div></div>`;
  const line = (label, value) => el.append(
    html`<div class="detail-row"><span class="dl">${label}</span><span class="dv">${value}</span></div>`);
  const conflicted = new Set(p.conflicts.map(c => c.field));
  if (!conflicted.has('fn')) line('Name', p.fn);
  el._emailsBox = mergeMultiGroup(el, 'Email', p.emails, EMAIL_TYPES, 'email', '');
  el._telsBox = mergeMultiGroup(el, 'Phone', p.tels, TEL_TYPES, 'tel', 'CELL');
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
  // address, …) is merged too. Summarize it so nothing looks dropped, and let
  // the row expand to the actual carried-through values on demand.
  if (p.preserve.length) {
    const details = html`<details class="mg-kept">
      <summary><span class="dl">Also kept</span><span class="dv mail">${preservedLabels(p.preserve).join(', ')}</span></summary>
    </details>`;
    for (const item of preservedDetails(p.preserve)) {
      details.append(html`<div class="detail-row mg-kept-row"><span class="dl">${item.label}</span><span class="dv">${item.value}</span></div>`);
    }
    el.append(details);
  }
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
    const panel = previewPanel(preview);
    $('#mg-body', dlg).append(panel);

    const mergeBtn = $('#mg-merge', dlg), cancelBtn = $('#mg-cancel', dlg);
    cancelBtn.addEventListener('click', () => { if (!busy) done(false); });
    mergeBtn.addEventListener('click', async () => {
      // Apply the user's choice for each conflicting field. A chosen display
      // name also pulls its matching N (first/last) so they stay consistent.
      for (const sel of dlg.querySelectorAll('.mg-conflict')) preview[sel.dataset.field] = sel.value;
      // Take the edited (added/retyped/removed) emails and phones as the result.
      preview.emails = collectMulti(panel._emailsBox);
      preview.tels = collectMulti(panel._telsBox);
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

// readPhoto reads an image File into a compact JPEG data: URL, downscaling to at
// most `max` px on the long edge so an embedded contact photo stays small — the
// base64 rides along in every card fetch. Falls back to the raw data URL if the
// canvas path is unavailable (e.g. a format the browser can't draw).
function readPhoto(file, max = 256) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(reader.error);
    reader.onload = () => {
      const raw = reader.result;
      const img = document.createElement('img');
      img.onerror = () => resolve(raw);
      img.onload = () => {
        try {
          const scale = Math.min(1, max / Math.max(img.width, img.height));
          const w = Math.max(1, Math.round(img.width * scale)), h = Math.max(1, Math.round(img.height * scale));
          const canvas = document.createElement('canvas');
          canvas.width = w; canvas.height = h;
          canvas.getContext('2d').drawImage(img, 0, 0, w, h);
          resolve(canvas.toDataURL('image/jpeg', 0.85));
        } catch { resolve(raw); }
      };
      img.src = raw;
    };
    reader.readAsDataURL(file);
  });
}

function contactDialog(c) {
  const a = c?.adr ?? {};
  const dlg = html`<div class="overlay"><div class="dialog dialog-wide" role="dialog" aria-modal="true">
    <h2>${c ? 'Edit contact' : 'New contact'}</h2>
    <div class="grid">
      <div class="full cd-photo">
        <div id="cd-photo-prev" class="avatar avatar-xl" aria-hidden="true"></div>
        <div class="cd-photo-btns">
          <button type="button" id="cd-photo-add" class="ghost">Upload photo…</button>
          <button type="button" id="cd-photo-del" class="ghost" hidden>Remove</button>
          <input type="file" id="cd-photo-file" accept="image/*" hidden>
        </div>
      </div>
      <label>First name<input id="cd-first" type="text" value="${c?.first ?? ''}"></label>
      <label>Last name<input id="cd-last" type="text" value="${c?.last ?? ''}"></label>
      <label class="full">Display name (optional)<input id="cd-fn" type="text" value="${c?.fn ?? ''}" placeholder="Defaults to first + last"></label>
      <label>Organization<input id="cd-org" type="text" value="${c?.org ?? ''}"></label>
      <label>Job title<input id="cd-title" type="text" value="${c?.title ?? ''}"></label>
      <label class="full" id="cd-book-row">Address book<select id="cd-book"></select></label>

      <div class="full field-group">
        <div class="fg-head"><span>Email</span><button type="button" id="cd-add-email" class="ghost">${icon('plus', 13)} Add email</button></div>
        <div id="cd-emails"></div>
      </div>
      <div class="full field-group">
        <div class="fg-head"><span>Phone</span><button type="button" id="cd-add-tel" class="ghost">${icon('plus', 13)} Add phone</button></div>
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

  // Address book: where a new contact lands (the sidebar's book, else the
  // first) and, for an existing one, where it moves on save. Hidden when there
  // is no book yet — the first save creates one.
  const books = store.state.books;
  const bookSel = q('#cd-book');
  for (const b of books) bookSel.append(html`<option value="${b.href}">${b.displayName}</option>`);
  bookSel.value = c?.book || (selection.type === 'book' ? selection.href : books[0]?.href) || '';
  q('#cd-book-row').hidden = !books.length;

  const emailsBox = q('#cd-emails'), telsBox = q('#cd-tels');
  const emails = c?.emails?.length ? c.emails : [{ value: '', type: '' }];
  const tels = c?.tels?.length ? c.tels : [{ value: '', type: 'CELL' }];
  for (const e of emails) multiRow(emailsBox, e, EMAIL_TYPES, 'email');
  for (const t of tels) multiRow(telsBox, t, TEL_TYPES, 'tel');
  q('#cd-add-email').addEventListener('click', () => multiRow(emailsBox, { value: '', type: '' }, EMAIL_TYPES, 'email').querySelector('.mr-val').focus());
  q('#cd-add-tel').addEventListener('click', () => multiRow(telsBox, { value: '', type: 'CELL' }, TEL_TYPES, 'tel').querySelector('.mr-val').focus());

  // Photo: PHOTO isn't a modeled field — it rides in `preserve`. Keep the other
  // preserved lines aside and swap just the PHOTO line as the user uploads or
  // removes; `photoUrl` drives the live preview, `photoLine` is what's saved.
  const otherPreserve = (c?.preserve || []).filter(l => !isPhotoLine(l));
  let photoLine = (c?.preserve || []).find(isPhotoLine) || '';
  let photoUrl = c?.photo || '';
  const prev = q('#cd-photo-prev'), delBtn = q('#cd-photo-del');
  const renderPhoto = () => {
    prev.classList.toggle('avatar-img', !!photoUrl);
    prev.style.backgroundImage = photoUrl ? `url("${photoUrl}")` : '';
    prev.textContent = photoUrl ? '' : initials(q('#cd-fn').value || `${q('#cd-first').value} ${q('#cd-last').value}`.trim());
    delBtn.hidden = !photoUrl;
  };
  renderPhoto();
  for (const id of ['#cd-first', '#cd-last', '#cd-fn']) q(id).addEventListener('input', () => { if (!photoUrl) renderPhoto(); });
  q('#cd-photo-add').addEventListener('click', () => q('#cd-photo-file').click());
  q('#cd-photo-file').addEventListener('change', async ev => {
    const file = ev.target.files?.[0];
    ev.target.value = '';
    if (!file) return;
    if (!file.type.startsWith('image/')) { q('#cd-error').textContent = 'Please choose an image file.'; return; }
    try {
      photoUrl = await readPhoto(file);
      photoLine = buildPhotoLine(photoUrl);
      q('#cd-error').textContent = '';
      renderPhoto();
    } catch { q('#cd-error').textContent = 'Could not read that image.'; }
  });
  delBtn.addEventListener('click', () => { photoUrl = ''; photoLine = ''; renderPhoto(); });

  const close = () => dlg.remove();
  openOverlay(dlg, close);
  submitOnEnter(dlg, q('#cd-save'));
  q('#cd-cancel').addEventListener('click', close);
  q('#cd-save').addEventListener('click', async () => {
    const first = q('#cd-first').value.trim(), last = q('#cd-last').value.trim();
    const fn = q('#cd-fn').value.trim() || `${first} ${last}`.trim();
    if (!fn) { q('#cd-error').textContent = 'Please enter a name.'; return; }
    try {
      await store.saveContact({
        href: c?.href, etag: c?.etag, uid: c?.uid, book: c?.book,
        preserve: photoLine ? [...otherPreserve, photoLine] : otherPreserve,
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
      }, bookSel.value || undefined);
      close();
      toast('Contact saved.');
    } catch (e) {
      // The edit was stored and only the move failed: keeping the dialog open
      // would invite a re-save with the old ETag, which can only 412.
      if (e.saved) { close(); showError(e); }
      else if (e.conflict) q('#cd-error').textContent = e.message;
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
