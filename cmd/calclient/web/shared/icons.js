// icons.js — the one icon set for the end-user app: hand-drawn inline SVG,
// stroke = currentColor, one weight (1.8px, round caps), one 24px viewBox.
// Icons therefore inherit text colour in both colour schemes, print clean,
// and render identically on every platform — unlike the emoji they replaced
// (🔔 📅 ⚙ …), which arrive pre-coloured per-OS and clash with the palette.
//
// icon(name, size) returns Raw markup for the html`…` templates:
//   html`<button>${icon('plus')} New event</button>`
// Static chrome (index.html) inlines the same SVGs by hand; this module is
// for everything built in JS.

import { raw } from './dom.js';

const PATHS = {
  menu: '<path d="M4 7h16M4 12h16M4 17h16"/>',
  bell: '<path d="M6 9a6 6 0 0 1 12 0c0 4.2 1.5 5.5 2 6H4c.5-.5 2-1.8 2-6Z"/><path d="M10.3 18.5a1.8 1.8 0 0 0 3.4 0"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  gear: '<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/>',
  lock: '<rect x="5" y="11" width="14" height="9" rx="1.5"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/>',
  calendar: '<rect x="4" y="5.5" width="16" height="15.5" rx="1.5"/><path d="M8 3v4M16 3v4M4 10.5h16"/>',
  door: '<rect x="6" y="3" width="12" height="18" rx="1"/><path d="M3 21h18"/><circle cx="15" cy="12" r="1" fill="currentColor" stroke="none"/>',
  pin: '<path d="M20 10c0 5.5-8 12-8 12S4 15.5 4 10a8 8 0 0 1 16 0Z"/><circle cx="12" cy="10" r="3"/>',
  chevDown: '<path d="m6 9 6 6 6-6"/>',
  chevLeft: '<path d="m15 18-6-6 6-6"/>',
  chevRight: '<path d="m9 18 6-6-6-6"/>',
  x: '<path d="M18 6 6 18M6 6l12 12"/>',
  pencil: '<path d="M17 3a2.85 2.85 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5Z"/>',
  warn: '<path d="M12 3.2 2.6 19.5a.8.8 0 0 0 .7 1.2h17.4a.8.8 0 0 0 .7-1.2Z"/><path d="M12 9.5v4.2"/><path d="M12 17.1v.01"/>',
  check: '<path d="m4.5 12.5 5 5L20 6.5"/>',
  circle: '<circle cx="12" cy="12" r="8.2"/>',
  circleDot: '<circle cx="12" cy="12" r="8.2"/><circle cx="12" cy="12" r="3" fill="currentColor" stroke="none"/>',
};

export function icon(name, size = 16) {
  const p = PATHS[name];
  if (!p) throw new Error('icons.js: unknown icon ' + name);
  return raw(`<svg class="icon icon-${name}" width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">${p}</svg>`);
}
