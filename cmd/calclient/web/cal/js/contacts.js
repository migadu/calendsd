// contacts.js — the contacts view: searchable list + a full add/edit dialog
// (name, org/title, repeatable emails & phones, address, website, birthday,
// notes). Fields the editor doesn't model are preserved on save by the store.

import * as store from '/shared/store.js';
import { $, h, esc, toast, showError } from './main.js';

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

  const el = h(`<div class="contacts contacts-2col">
    <aside class="ct-side" aria-label="Address books and groups"></aside>
    <div class="ct-main">
      <div class="bar">
        <input id="ct-search" type="search" placeholder="Search contacts…" value="${esc(filter)}">
        <button class="primary" id="ct-add">＋ New contact</button>
      </div>
      <div id="ct-rows"></div>
    </div>
  </div>`);
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
// below it, any contact groups — each a selectable filter for the list.
function buildSidebar(side, books, groups) {
  side.innerHTML = '';
  const item = (label, count, active, onClick, cls) => {
    const b = h(`<button type="button" class="ct-nav ${cls}${active ? ' active' : ''}">
      <span class="ct-nav-label">${esc(label)}</span>
      ${count != null ? `<span class="ct-nav-count">${count}</span>` : ''}
    </button>`);
    b.addEventListener('click', onClick);
    side.append(b);
  };
  const heading = title => side.append(h(`<div class="ct-side-h">${esc(title)}</div>`));

  heading('Address Books');
  item('All Contacts', store.state.contacts.length, selection.type === 'all',
    () => select({ type: 'all' }), 'ct-book');
  for (const book of books) {
    const count = store.state.contacts.filter(c => c.book === book.href).length;
    item(book.displayName, count,
      selection.type === 'book' && selection.href === book.href,
      () => select({ type: 'book', href: book.href }), 'ct-book');
  }

  if (groups.length) {
    heading('Groups');
    for (const g of groups) {
      item(g.fn || 'Group', g.members.length,
        selection.type === 'group' && selection.href === g.href,
        () => select({ type: 'group', href: g.href }), 'ct-group');
    }
  }
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
    box.append(h(`<p class="empty">${msg}</p>`));
    return;
  }
  for (const c of rows) {
    const sub = [c.emails[0]?.value, c.tels[0]?.value, c.org].filter(Boolean).join(' · ');
    const row = h(`<button class="contact" type="button">
      <span class="avatar" aria-hidden="true">${esc(initials(c.fn))}</span>
      <span class="who"><div>${esc(c.fn)}${c.title ? ` <span class="mail">— ${esc(c.title)}</span>` : ''}</div>
        <div class="mail">${esc(sub)}</div></span>
      <span class="chev" aria-hidden="true">›</span>
    </button>`);
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
  const line = (label, valueHtml) => rows.push(
    `<div class="detail-row"><span class="dl">${esc(label)}</span><span class="dv">${valueHtml}</span></div>`);
  const typeLabel = t => t ? `<span class="type-tag">${esc(t[0] + t.slice(1).toLowerCase())}</span>` : '';

  if (c.org || c.title) line('', esc([c.title, c.org].filter(Boolean).join(', ')));
  for (const e of c.emails) line('Email', `<a href="mailto:${esc(e.value)}">${esc(e.value)}</a> ${typeLabel(e.type)}`);
  for (const t of c.tels) line('Phone', `<a href="tel:${esc(t.value.replace(/\s+/g, ''))}">${esc(t.value)}</a> ${typeLabel(t.type)}`);
  const a = c.adr || {};
  const addr = [a.street, [a.postal, a.city].filter(Boolean).join(' '), a.region, a.country].filter(Boolean);
  if (addr.length) line('Address', addr.map(esc).join('<br>'));
  if (c.url) line('Website', `<a href="${esc(c.url)}" target="_blank" rel="noreferrer noopener">${esc(c.url)}</a>`);
  if (c.bday) line('Birthday', esc(formatBday(c.bday)));
  if (c.note) line('Notes', esc(c.note).replace(/\n/g, '<br>'));
  if (!rows.length) rows.push('<p class="empty" style="margin:1rem 0">No other details.</p>');

  const dlg = h(`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="Contact details">
    <button class="dialog-close" id="cv-x" aria-label="Close">×</button>
    <div class="detail-head">
      <span class="avatar avatar-lg" aria-hidden="true">${esc(initials(c.fn))}</span>
      <div><h2 style="margin:0">${esc(c.fn)}</h2>${c.title || c.org
        ? `<div class="mail">${esc([c.title, c.org].filter(Boolean).join(' · '))}</div>` : ''}</div>
    </div>
    <div class="detail-body">${rows.join('')}</div>
    <div class="actions">
      <button class="danger" id="cv-del">Delete…</button>
      <button id="cv-edit">Edit</button>
      <button class="primary" id="cv-close">Done</button>
    </div>
  </div></div>`);
  const close = () => dlg.remove();
  dlg.addEventListener('click', ev => { if (ev.target === dlg) close(); });
  dlg.addEventListener('keydown', ev => { if (ev.key === 'Escape') { ev.stopPropagation(); close(); } });
  $('#cv-x', dlg).addEventListener('click', close);
  $('#cv-close', dlg).addEventListener('click', close);
  $('#cv-edit', dlg).addEventListener('click', () => { close(); contactDialog(c); });
  $('#cv-del', dlg).addEventListener('click', async () => {
    if (!confirm(`Delete ${c.fn}?`)) return;
    try { await store.deleteContact(c.href); close(); toast('Contact deleted.'); }
    catch (e) { showError(e); }
  });
  $('#dialog-host').append(dlg);
  $('#cv-close', dlg).focus();
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
    `<option value="${t}" ${t === type ? 'selected' : ''}>${t ? t[0] + t.slice(1).toLowerCase() : '—'}</option>`).join('');
  const row = h(`<div class="multi-row">
    <input type="${inputType}" class="mr-val" value="${esc(value)}">
    <select class="mr-type">${opts}</select>
    <button type="button" class="mr-del" aria-label="Remove">×</button>
  </div>`);
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
  const dlg = h(`<div class="overlay"><div class="dialog dialog-wide" role="dialog" aria-modal="true">
    <h2>${c ? 'Edit contact' : 'New contact'}</h2>
    <div class="grid">
      <label>First name<input id="cd-first" type="text" value="${esc(c?.first ?? '')}"></label>
      <label>Last name<input id="cd-last" type="text" value="${esc(c?.last ?? '')}"></label>
      <label class="full">Display name (optional)<input id="cd-fn" type="text" value="${esc(c?.fn ?? '')}" placeholder="Defaults to first + last"></label>
      <label>Organization<input id="cd-org" type="text" value="${esc(c?.org ?? '')}"></label>
      <label>Job title<input id="cd-title" type="text" value="${esc(c?.title ?? '')}"></label>

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
          <label class="full">Street<input id="cd-street" type="text" value="${esc(a.street ?? '')}"></label>
          <label>City<input id="cd-city" type="text" value="${esc(a.city ?? '')}"></label>
          <label>Region / State<input id="cd-region" type="text" value="${esc(a.region ?? '')}"></label>
          <label>Postal code<input id="cd-postal" type="text" value="${esc(a.postal ?? '')}"></label>
          <label>Country<input id="cd-country" type="text" value="${esc(a.country ?? '')}"></label>
        </div>
      </div>

      <label>Website<input id="cd-url" type="url" value="${esc(c?.url ?? '')}" placeholder="https://"></label>
      <label>Birthday<input id="cd-bday" type="date" value="${esc(bdayInput(c?.bday))}"></label>
      <label class="full">Notes<textarea id="cd-note" rows="2">${esc(c?.note ?? '')}</textarea></label>
    </div>
    <p class="error" id="cd-error"></p>
    <div class="actions">
      <button id="cd-cancel">Cancel</button>
      <button class="primary" id="cd-save">Save</button>
    </div>
  </div></div>`);

  const q = sel => $(sel, dlg);
  const emailsBox = q('#cd-emails'), telsBox = q('#cd-tels');
  const emails = c?.emails?.length ? c.emails : [{ value: '', type: '' }];
  const tels = c?.tels?.length ? c.tels : [{ value: '', type: 'CELL' }];
  for (const e of emails) multiRow(emailsBox, e, EMAIL_TYPES, 'email');
  for (const t of tels) multiRow(telsBox, t, TEL_TYPES, 'tel');
  q('#cd-add-email').addEventListener('click', () => multiRow(emailsBox, { value: '', type: '' }, EMAIL_TYPES, 'email').querySelector('.mr-val').focus());
  q('#cd-add-tel').addEventListener('click', () => multiRow(telsBox, { value: '', type: 'CELL' }, TEL_TYPES, 'tel').querySelector('.mr-val').focus());

  const close = () => dlg.remove();
  dlg.addEventListener('click', ev => { if (ev.target === dlg) close(); });
  dlg.addEventListener('keydown', ev => { if (ev.key === 'Escape') { ev.stopPropagation(); close(); } });
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
  $('#dialog-host').append(dlg);
  q('#cd-first').focus();
}

// bdayInput normalizes a stored BDAY (often YYYYMMDD) to the date input's
// YYYY-MM-DD, leaving already-hyphenated or partial values alone.
function bdayInput(bday) {
  if (!bday) return '';
  const m = /^(\d{4})-?(\d{2})-?(\d{2})/.exec(bday);
  return m ? `${m[1]}-${m[2]}-${m[3]}` : '';
}
