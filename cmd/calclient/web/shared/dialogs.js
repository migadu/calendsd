// dialogs.js — the shared overlay-dismissal wiring plus promise-based
// confirm/prompt, replacing window.confirm/prompt (which block the event
// loop and can't be styled or keyboard-wired consistently). Each app using
// this provides a #dialog-host element and the .overlay/.dialog/.actions
// styles in its own stylesheet.

import { $, html } from './dom.js';

// trapTabIn keeps Tab cycling inside `el` while it is open — the keyboard
// half of a modal. Used by openOverlay for every dialog and by the invites
// panel (which isn't an overlay but is modal over the content beneath).
export function trapTabIn(el) {
  el.addEventListener('keydown', ev => {
    if (ev.key !== 'Tab') return;
    const focusable = [...el.querySelectorAll('button, input, select, textarea, a[href], [tabindex]:not([tabindex="-1"])')]
      .filter(x => !x.disabled && x.offsetParent !== null);
    if (!focusable.length) return;
    const first = focusable[0], last = focusable[focusable.length - 1];
    if (ev.shiftKey && (document.activeElement === first || document.activeElement === el)) {
      ev.preventDefault(); last.focus();
    } else if (!ev.shiftKey && document.activeElement === last) {
      ev.preventDefault(); first.focus();
    }
  });
}

// openOverlay is the one place dialog dismissal and keyboard modality are
// wired: scrim click and Escape route to onClose, Tab stays inside, and the
// element that had focus when the dialog opened gets it back when the dialog
// closes — so closing an event returns focus to the chip that opened it, not
// to the top of the page. `close` (and dlg._restoreFocus for callers that
// remove the element directly, e.g. a superseding route) expose that path.
export function openOverlay(dlg, onClose) {
  dlg._opener = document.activeElement;
  dlg._restoreFocus = () => {
    if (dlg._opener && dlg._opener.isConnected && !document.body.contains(dlg)) dlg._opener.focus();
  };
  const wrapped = () => { onClose(); dlg._restoreFocus(); };
  dlg.close = wrapped;
  dlg.addEventListener('click', ev => { if (ev.target === dlg) wrapped(); });
  dlg.addEventListener('keydown', ev => { if (ev.key === 'Escape') { ev.stopPropagation(); wrapped(); } });
  trapTabIn(dlg);
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

// confirmDialog resolves true on confirm, false on any way out. Button order
// follows the app-wide convention: the action sits on the left (a destructive
// one gets the danger tint and the .actions auto-margin), Cancel on the right
// and focused — a stray Enter can't confirm a destructive ask.
export function confirmDialog(message, { confirmLabel = 'OK', danger = false } = {}) {
  return new Promise(resolve => {
    const dlg = html`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="Confirm">
      <p style="margin:.2rem 0 0">${message}</p>
      <div class="actions"></div>
    </div></div>`;
    const row = $('.actions', dlg);
    // danger puts the action on the left (the app-wide "scary button lives
    // left" convention, with the .actions auto-margin rule); otherwise the
    // action is the primary on the right.
    if (danger) {
      const ok = html`<button class="danger" data-ok>${confirmLabel}</button>`;
      const cancel = html`<button data-cancel>Cancel</button>`;
      row.append(ok, cancel);
    } else {
      const cancel = html`<button data-cancel>Cancel</button>`;
      const ok = html`<button class="primary" data-ok>${confirmLabel}</button>`;
      row.append(cancel, ok);
    }
    const done = v => { dlg.remove(); resolve(v); };
    openOverlay(dlg, () => done(false));
    $('[data-cancel]', dlg).addEventListener('click', () => done(false));
    $('[data-ok]', dlg).addEventListener('click', () => done(true));
    $('[data-cancel]', dlg).focus();
  });
}

// choiceDialog offers a message and a row of choice buttons, resolving the
// chosen option's `value` (or null on Cancel / Escape / scrim). Each option is
// {label, value, danger?, primary?}. The choices pack to the left (danger ones
// tinted, mirroring confirmDialog) and Cancel sits at the far right, focused,
// so a stray Enter or click-out can't pick a destructive choice.
export function choiceDialog(message, options) {
  return new Promise(resolve => {
    const dlg = html`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="Choose">
      <p style="margin:.2rem 0 0">${message}</p>
      <div class="choices" style="display:flex;flex-wrap:wrap;gap:.5rem;margin-top:1rem;justify-content:flex-end">
        <button data-cancel>Cancel</button>
      </div>
    </div></div>`;
    const done = v => { dlg.remove(); resolve(v); };
    const row = $('.choices', dlg);
    const cancel = $('[data-cancel]', dlg);
    for (const o of options) {
      const btn = html`<button class="${o.danger ? 'danger' : o.primary ? 'primary' : ''}">${o.label}</button>`;
      btn.addEventListener('click', () => done(o.value));
      row.insertBefore(btn, cancel);
    }
    openOverlay(dlg, () => done(null));
    cancel.addEventListener('click', () => done(null));
    cancel.focus();
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
