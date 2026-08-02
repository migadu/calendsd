// escape.js — the escaping core shared by the HTML (dom.js) and XML (dav.js)
// template tags. Both markups escape the same metacharacters and want the
// same interpolation semantics (escape by default, Raw passes through,
// arrays flatten, null/booleans vanish), so the mechanism lives once in this
// dependency-free leaf and each layer wraps it in its own tag.

const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' };
export function esc(s) {
  return String(s).replace(/[&<>"]/g, ch => ESC[ch]);
}

// Raw wraps a string of markup that must NOT be escaped again. toString()
// returns the markup so a Raw can also be used where a string is expected
// (innerHTML assignments, request bodies, string concatenation).
export class Raw {
  constructor(s) { this.markup = s; }
  toString() { return this.markup; }
}

// raw marks already-safe markup for interpolation. Reach for it only when the
// markup genuinely cannot be produced by a tag — every raw() call site is a
// place a reviewer must check by hand.
export const raw = s => new Raw(String(s));

function part(v) {
  if (v == null || typeof v === 'boolean') return '';
  if (typeof v === 'number') return String(v);   // fast path for geometry values
  if (v instanceof Raw) return v.markup;
  if (Array.isArray(v)) return v.map(part).join('');
  return esc(v);
}

// render evaluates a tagged template with escape-by-default interpolation.
export function render(strings, values) {
  let out = strings[0];
  for (let i = 0; i < values.length; i++) out += part(values[i]) + strings[i + 1];
  return out;
}
