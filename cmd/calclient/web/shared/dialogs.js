// dialogs.js — the shared overlay-dismissal wiring plus promise-based
// confirm/prompt, replacing window.confirm/prompt (which block the event
// loop and can't be styled or keyboard-wired consistently). Each app using
// this provides a #dialog-host element and the .overlay/.dialog/.actions
// styles in its own stylesheet.

import { $, html } from './dom.js';

// openOverlay is the one place dialog dismissal is wired: scrim click and
// Escape route to onClose, and onClose is stored on the element so the
// app-global Escape handler (which acts on the topmost overlay without
// knowing what it is) can invoke the same path instead of removing a node
// whose caller may be awaiting a promise.
export function openOverlay(dlg, onClose) {
  dlg.close = onClose;
  dlg.addEventListener('click', ev => { if (ev.target === dlg) onClose(); });
  dlg.addEventListener('keydown', ev => { if (ev.key === 'Escape') { ev.stopPropagation(); onClose(); } });
  $('#dialog-host').append(dlg);
}

// submitOnEnter makes a non-<form> editor dialog submit like a form: Enter in
// a single-line field clicks `button`. Enter keeps its native meaning where it
// already has one — a textarea inserts a newline, a <select> opens its menu,
// and a focused button or link activates *itself* (hijacking it would make
// Tab + Enter on Cancel save instead). `skip` excludes fields that own Enter
// themselves (e.g. an autocomplete input).
export function submitOnEnter(dlg, button, skip = () => false) {
  dlg.addEventListener('keydown', ev => {
    const t = ev.target;
    if (ev.key !== 'Enter' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT'
      || t.tagName === 'BUTTON' || t.tagName === 'A' || skip(t)) return;
    ev.preventDefault();
    button.click();
  });
}

// confirmDialog resolves true on confirm, false on any way out.
export function confirmDialog(message, { confirmLabel = 'OK', danger = false } = {}) {
  return new Promise(resolve => {
    const dlg = html`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="Confirm">
      <p style="margin:.2rem 0 0">${message}</p>
      <div class="actions">
        <button data-cancel>Cancel</button>
        <button class="${danger ? 'danger' : 'primary'}" data-ok>${confirmLabel}</button>
      </div>
    </div></div>`;
    const done = v => { dlg.remove(); resolve(v); };
    openOverlay(dlg, () => done(false));
    $('[data-cancel]', dlg).addEventListener('click', () => done(false));
    $('[data-ok]', dlg).addEventListener('click', () => done(true));
    // Focus Cancel for a destructive ask so a stray Enter can't destroy data.
    $(danger ? '[data-cancel]' : '[data-ok]', dlg).focus();
  });
}

// choiceDialog offers a message and a row of choice buttons, resolving the
// chosen option's `value` (or null on Cancel / Escape / scrim). Each option is
// {label, value, danger?, primary?}; a Cancel button is always appended and
// focused so a stray Enter or click-out can't pick a destructive choice.
export function choiceDialog(message, options) {
  return new Promise(resolve => {
    // Own the button row (not .actions) so the "danger sits on the left"
    // auto-margin rule doesn't fire — every choice packs to the right; the
    // base button.danger colour still applies for a destructive tint.
    const dlg = html`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="Choose">
      <p style="margin:.2rem 0 0">${message}</p>
      <div class="choices" style="display:flex;flex-wrap:wrap;gap:.5rem;margin-top:1rem;justify-content:flex-end">
        <button data-cancel>Cancel</button>
      </div>
    </div></div>`;
    const done = v => { dlg.remove(); resolve(v); };
    const row = $('.choices', dlg);
    for (const o of options) {
      const btn = html`<button class="${o.danger ? 'danger' : o.primary ? 'primary' : ''}">${o.label}</button>`;
      btn.addEventListener('click', () => done(o.value));
      row.append(btn);
    }
    openOverlay(dlg, () => done(null));
    $('[data-cancel]', dlg).addEventListener('click', () => done(null));
    $('[data-cancel]', dlg).focus();
  });
}

// promptDialog resolves the entered string on submit, null on any way out.
export function promptDialog(message, { placeholder = '', value = '', confirmLabel = 'OK' } = {}) {
  return new Promise(resolve => {
    const dlg = html`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="${message}">
      <form>
        <label>${message}<input type="text" value="${value}" placeholder="${placeholder}" required></label>
        <div class="actions">
          <button type="button" data-cancel>Cancel</button>
          <button type="submit" class="primary">${confirmLabel}</button>
        </div>
      </form>
    </div></div>`;
    const done = v => { dlg.remove(); resolve(v); };
    openOverlay(dlg, () => done(null));
    $('[data-cancel]', dlg).addEventListener('click', () => done(null));
    $('form', dlg).addEventListener('submit', ev => {
      ev.preventDefault();
      done($('input', dlg).value);
    });
    $('input', dlg).focus();
  });
}
