// guest-field.js — the guest-chips control with directory autocomplete, used
// by the event editor: type a name or email, pick from the directory with
// ↑/↓/Enter, remove with the chip's × or Backspace on an empty input.

import * as store from '/shared/store.js';
import { $, html, frag } from '/shared/dom.js';

// createGuestField renders the control. `guests` is the caller's live array
// of {email, partstat}, mutated in place; onChange fires after every add or
// remove so the caller can sync dependent UI (hint, availability button).
// The element ids are part of the editor's public surface (CSS and the e2e
// tests address #ev-guest directly), so they live here with the markup.
export function createGuestField(guests, onChange) {
  const el = html`<span class="suggest">
    <span class="chips" id="ev-chips"><input id="ev-guest" type="email" placeholder="Invite by name or email…" autocomplete="off"></span>
    <span class="suggest-list" id="ev-suggest" hidden></span></span>`;
  const chips = $('#ev-chips', el), input = $('#ev-guest', el), suggest = $('#ev-suggest', el);

  function renderChips() {
    for (const g of chips.querySelectorAll('.guest')) g.remove();
    for (const g of guests) {
      const cls = (g.partstat === 'ACCEPTED' ? ' accepted' : g.partstat === 'DECLINED' ? ' declined' : '')
        + (g.room ? ' room' : '');
      const chip = html`<span class="guest${cls}" title="${g.room ? 'Room' : g.partstat ?? ''}">${g.room ? '🚪 ' : ''}${g.email}<button aria-label="Remove ${g.email}">×</button></span>`;
      $('button', chip).addEventListener('click', () => {
        guests.splice(guests.indexOf(g), 1);
        renderChips();
        onChange();
      });
      chips.insertBefore(chip, input);
    }
  }

  function addGuest(email, partstat, room = false) {
    email = email.trim().replace(/,$/, '');
    if (!email || !email.includes('@') || guests.some(g => g.email === email)) return;
    guests.push({ email, partstat: partstat || '', room });
    renderChips();
    input.value = '';
    closeSuggest();
    onChange();
  }

  // Autocomplete with keyboard navigation: ↑/↓ move the highlight, Enter
  // picks it (or adds the typed text), Esc closes the list.
  let suggestTimer = null, active = -1;
  function highlight(i) {
    const items = [...suggest.children];
    active = items.length ? (i + items.length) % items.length : -1;
    items.forEach((b, n) => b.classList.toggle('active', n === active));
    if (active >= 0) items[active].scrollIntoView({ block: 'nearest' });
  }
  function closeSuggest() { suggest.hidden = true; suggest.innerHTML = ''; active = -1; }

  input.addEventListener('input', () => {
    clearTimeout(suggestTimer);
    suggestTimer = setTimeout(async () => {
      const found = await store.searchDirectory(input.value);
      suggest.innerHTML = '';
      for (const f of found.filter(f => !guests.some(g => g.email === f.email))) {
        const b = html`<button type="button">${f.name || f.email} <span class="mail">${f.email}</span>${f.room ? frag`<span class="type-tag">room</span>` : ''}</button>`;
        b.dataset.email = f.email;
        b.dataset.room = f.room ? '1' : '';
        b.addEventListener('click', () => { addGuest(f.email, '', f.room); input.focus(); });
        b.addEventListener('mousemove', () => highlight([...suggest.children].indexOf(b)));
        suggest.append(b);
      }
      suggest.hidden = !suggest.children.length;
      active = -1;
    }, 200);
  });
  input.addEventListener('keydown', ev => {
    const open = !suggest.hidden && suggest.children.length;
    if (ev.key === 'ArrowDown' && open) { ev.preventDefault(); highlight(active + 1); return; }
    if (ev.key === 'ArrowUp' && open) { ev.preventDefault(); highlight(active - 1); return; }
    if (ev.key === 'Enter' || ev.key === ',') {
      ev.preventDefault();
      if (open && active >= 0) { const b = suggest.children[active]; addGuest(b.dataset.email, '', b.dataset.room === '1'); }
      else addGuest(input.value);
      return;
    }
    if (ev.key === 'Escape' && open) { ev.stopPropagation(); closeSuggest(); return; }
    if (ev.key === 'Backspace' && !input.value && guests.length) { guests.pop(); renderChips(); onChange(); }
  });
  input.addEventListener('blur', () => setTimeout(closeSuggest, 150));

  renderChips();
  return el;
}
