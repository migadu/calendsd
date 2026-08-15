// guest-field.js — the guest-chips control with directory autocomplete, used
// by the event editor: type a name or email, pick from the directory with
// ↑/↓/Enter, remove with the chip's × or Backspace on an empty input.

import * as store from '/shared/store.js';
import { $, html, frag } from '/shared/dom.js';
import { icon } from '/shared/icons.js';
import { toast } from './ui.js';

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
      const chip = html`<span class="guest${cls}" title="${g.room ? 'Room' : g.partstat ?? ''}">${g.room ? frag`${icon('door', 13)} ` : ''}${g.email}<button class="icon-btn" aria-label="Remove ${g.email}">${icon('x', 12)}</button></span>`;
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
    if (!email) return false;
    // Typed text that isn't an address gets a visible nudge instead of just
    // vanishing — the silent clear read as "the app ate my input".
    if (!email.includes('@')) {
      input.classList.add('invalid');
      toast(`“${email}” doesn’t look like an email address.`, true);
      return false;
    }
    if (guests.some(g => g.email === email)) { input.value = ''; return false; }
    guests.push({ email, partstat: partstat || '', room });
    renderChips();
    input.value = '';
    closeSuggest();
    onChange();
    return true;
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
    input.classList.remove('invalid');   // clear the bad-address flag as they retype
    clearTimeout(suggestTimer);
    suggestTimer = setTimeout(async () => {
      const found = await store.searchDirectory(input.value);
      suggest.innerHTML = '';
      // Hide an individual already invited; a group (address-book or principal)
      // always shows since picking it adds/expands members.
      for (const f of found.filter(f => f.emails || !guests.some(g => g.email === f.email))) {
        // An address-book group expands into its members client-side; a person,
        // room, or group principal is added by its single address (the server
        // expands a group principal on save).
        // Picking an address-book group adds several people at once; say how
        // many so a dozen silent chip additions don't go unnoticed.
        const pick = () => {
          if (!f.emails) { addGuest(f.email, '', f.room); return; }
          const before = guests.length;
          f.emails.forEach(e => addGuest(e));
          const added = guests.length - before;
          toast(added
            ? `Added ${added} ${added > 1 ? 'people' : 'person'} from ${f.name || 'the group'}.`
            : 'Everyone in that group is already invited.');
        };
        const tag = f.room ? 'room' : f.group ? 'group' : '';
        const sub = f.emails ? `${f.emails.length} people` : f.email;
        const b = html`<button type="button">${f.name || f.email} <span class="mail">${sub}</span>${tag ? frag`<span class="type-tag">${tag}</span>` : ''}</button>`;
        b._pick = pick;
        b.addEventListener('click', () => { pick(); input.focus(); });
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
      if (open && active >= 0) suggest.children[active]._pick();
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
