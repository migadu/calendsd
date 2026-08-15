// new-collection.js — the create dialog shared by calendars, task lists,
// address books and contact groups: a name field plus, for collections that
// carry a colour, a swatch picker. Replaces the bare prompt() the first
// versions used: choosing a colour at creation time avoids the wall of
// default-blue calendars that followed, and matches the quality bar the
// event editor sets for every other creation flow.

import * as store from '/shared/store.js';
import { $, html, frag } from '/shared/dom.js';
import { icon } from '/shared/icons.js';
import { openOverlay, submitOnEnter } from '/shared/dialogs.js';

// A palette that reads on white and on the dark scheme, distinct in hue and
// in lightness (so calendars stay tellable apart even without colour vision;
// inkOn() picks the chip label colour per swatch).
const SWATCHES = [
  '#3570FF', '#C62828', '#D4A500', '#388E3C',
  '#7C4DFF', '#00838F', '#F4511E', '#D81B60',
  '#546E7A', '#8D6E63', '#3949AB', '#00ACC1',
];

// newCollectionDialog({ title, withColor }) resolves { name, color } on
// Create (color '' when withColor is false), or null on Cancel / Escape /
// scrim. The swatches default to the first colour no existing calendar uses,
// so successive creations come out distinct without any thought.
export function newCollectionDialog({ title, label = 'Name', confirmLabel = 'Create', withColor = true } = {}) {
  return new Promise(resolve => {
    const used = new Set(store.state.calendars.map(c => (c.color || '').toLowerCase()));
    let color = withColor ? (SWATCHES.find(s => !used.has(s)) ?? SWATCHES[0]) : '';

    const dlg = html`<div class="overlay"><div class="dialog" role="dialog" aria-modal="true" aria-label="${title}">
      <h2>${title}</h2>
      <label>${label}<input id="nc-name" type="text" placeholder="e.g. Work" autocomplete="off"></label>
      ${withColor && frag`<div class="swatches" role="radiogroup" aria-label="Colour" id="nc-swatches">
        ${SWATCHES.map(s => frag`<button type="button" class="swatch" role="radio"
          style="background:${s}" data-c="${s}" aria-label="${s}"></button>`)}
      </div>`}
      <div class="actions">
        <button id="nc-cancel">Cancel</button>
        <button class="primary" id="nc-create" disabled>${icon('plus', 14)} ${confirmLabel}</button>
      </div>
    </div></div>`;
    const done = v => { dlg.remove(); resolve(v); };
    openOverlay(dlg, () => done(null));
    $('#nc-cancel', dlg).addEventListener('click', () => done(null));
    submitOnEnter(dlg, $('#nc-create', dlg));

    const name = $('#nc-name', dlg);
    const create = $('#nc-create', dlg);
    name.addEventListener('input', () => { create.disabled = !name.value.trim(); });

    const box = $('#nc-swatches', dlg);
    const syncSwatches = () => {
      for (const b of box.querySelectorAll('.swatch')) {
        const on = b.dataset.c === color;
        b.setAttribute('aria-checked', String(on));
        b.style.outline = on ? '2px solid var(--ink)' : 'none';
        b.style.outlineOffset = '2px';
      }
    };
    box?.addEventListener('click', ev => {
      const b = ev.target.closest('.swatch');
      if (!b) return;
      color = b.dataset.c;
      syncSwatches();
    });
    if (box) syncSwatches();

    create.addEventListener('click', () => {
      const n = name.value.trim();
      if (!n) return;   // guarded by the disabled button
      done({ name: n, color });
    });
    name.focus();
  });
}
