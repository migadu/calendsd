// dom.js — DOM construction and HTML escaping shared by both apps.
//
// The core is a pair of tagged templates that HTML-escape every interpolated
// value by default:
//
//   html`<button title="${title}">${label}</button>`   → Element
//   frag`<span class="tag">${text}</span>`             → Raw (composable markup)
//
// Interpolated values are escaped unless they are already Raw (the result of
// frag`...` or raw(...)); arrays are flattened; null/undefined/booleans render
// as nothing (so `${cond && frag`...`}` works). This inverts the old
// discipline of calling esc() at every interpolation site: markup is now safe
// by default, and a forgotten escape cannot become an injection hole.
//
// The escaping/interpolation engine itself lives in escape.js (shared with
// dav.js's xml tag). Only html() touches `document`, and only when called, so
// the string helpers stay unit-testable under node.

import { esc, Raw, render } from './escape.js';

export { esc, raw } from './escape.js';

export const $ = (sel, root = document) => root.querySelector(sel);

// frag`...` renders an escaped markup fragment for composing into a larger
// html`...` or frag`...` template.
export function frag(strings, ...values) { return new Raw(render(strings, values)); }

// brJoin renders plain-text lines separated by <br>.
export const brJoin = items => new Raw(items.map(esc).join('<br>'));

// html`...` renders a template to its first element.
export function html(strings, ...values) {
  const t = document.createElement('template');
  t.innerHTML = render(strings, values).trim();
  return t.content.firstElementChild;
}
