// dav.js — the DAV plumbing: an instrumented fetch wrapper plus XML
// build/parse helpers for multistatus, PROPFIND, REPORT, and PROPPATCH.

export const NS = {
  d: 'DAV:',
  c: 'urn:ietf:params:xml:ns:caldav',
  card: 'urn:ietf:params:xml:ns:carddav',
  cs: 'http://calendarserver.org/ns/',
  a: 'http://apple.com/ns/ical/',
  s: 'http://sabredav.org/ns',
};

// ---- session -------------------------------------------------------------

export const session = {
  get user() { return sessionStorage.getItem('calclient.user') || ''; },
  get auth() { return sessionStorage.getItem('calclient.auth') || ''; },
  login(user, pass) {
    sessionStorage.setItem('calclient.user', user);
    sessionStorage.setItem('calclient.auth', btoa(`${user}:${pass}`));
  },
  logout() {
    sessionStorage.removeItem('calclient.user');
    sessionStorage.removeItem('calclient.auth');
  },
};

// ---- instrumented fetch --------------------------------------------------

// Set by the app; called with every exchange so the raw drawer stays live.
export let onExchange = null;
export function setOnExchange(fn) { onExchange = fn; }

// req issues a same-origin DAV request with Basic auth attached and records
// the full exchange. Returns {status, ok, headers, text, xml, url, redirected}.
export async function req(method, path, { headers = {}, body = null, redirect = 'follow', timeout = 20000 } = {}) {
  const h = { ...headers };
  if (session.auth) h['Authorization'] = 'Basic ' + session.auth;
  let res, text;
  // Time-box every request: a hung fetch (e.g. a browser stuck on an auth
  // challenge) must never freeze the UI — abort and surface it as an error.
  const ac = new AbortController();
  const timer = setTimeout(() => ac.abort(), timeout);
  try {
    // cache: 'no-store' keeps the browser HTTP cache out of the loop entirely.
    // DAV freshness is tracked by ctag/ETag, and object GETs carry an ETag but
    // no Cache-Control, so a heuristically-cached GET could return a stale body
    // and ETag — which the editor would then send back as a doomed If-Match,
    // failing the PUT with a 412 "changed elsewhere" against the newer server copy.
    res = await fetch(path, { method, headers: h, body, redirect, signal: ac.signal, cache: 'no-store' });
    text = await res.text();
  } catch (e) {
    const msg = e.name === 'AbortError' ? 'request timed out' : `network error: ${e.message}`;
    record(method, path, h, body, null, msg);
    throw new Error(msg);
  } finally {
    clearTimeout(timer);
  }
  let xml = null;
  const ct = res.headers.get('Content-Type') || '';
  if (text && (ct.includes('xml') || text.trimStart().startsWith('<?xml'))) {
    const doc = new DOMParser().parseFromString(text, 'application/xml');
    if (!doc.querySelector('parsererror')) xml = doc;
  }
  const out = {
    status: res.status, ok: res.ok, headers: res.headers,
    text, xml, url: res.url, redirected: res.redirected,
    // Request identity, so an error handler can say which exchange failed.
    method, path, reqHeaders: h,
  };
  record(method, path, h, body, out, null);
  return out;
}

function record(method, path, reqHeaders, reqBody, res, error) {
  if (!onExchange) return;
  const shown = { ...reqHeaders };
  if (shown['Authorization']) shown['Authorization'] = 'Basic ····';
  onExchange({ method, path, reqHeaders: shown, reqBody, res, error });
}

// ---- XML building --------------------------------------------------------

export function esc(s) {
  return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
    .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

const XMLNS = `xmlns:d="${NS.d}" xmlns:c="${NS.c}" xmlns:card="${NS.card}" xmlns:cs="${NS.cs}" xmlns:a="${NS.a}" xmlns:s="${NS.s}"`;

// props: array of prefixed names like 'd:displayname', 'cs:getctag'.
export function propfindBody(props) {
  const inner = props.map(p => `<${p}/>`).join('');
  return `<?xml version="1.0" encoding="utf-8"?>\n<d:propfind ${XMLNS}><d:prop>${inner}</d:prop></d:propfind>`;
}

export async function propfind(path, props, depth = '0') {
  return req('PROPFIND', path, {
    headers: { Depth: depth, 'Content-Type': 'application/xml; charset=utf-8' },
    body: propfindBody(props),
  });
}

export async function report(path, body, depth = '1') {
  return req('REPORT', path, {
    headers: { Depth: depth, 'Content-Type': 'application/xml; charset=utf-8' },
    body,
  });
}

// set: array of {name: 'a:calendar-color', value: '#ff0000ff'} (value is
// escaped text; pass {xml: '<raw/>'} for element content). remove: names.
export async function proppatch(path, set = [], remove = []) {
  const sets = set.map(p =>
    `<d:set><d:prop><${p.name}>${p.xml ?? esc(p.value)}</${p.name.split(' ')[0]}></d:prop></d:set>`).join('');
  const removes = remove.map(n => `<d:remove><d:prop><${n}/></d:prop></d:remove>`).join('');
  const body = `<?xml version="1.0" encoding="utf-8"?>\n<d:propertyupdate ${XMLNS}>${sets}${removes}</d:propertyupdate>`;
  return req('PROPPATCH', path, {
    headers: { 'Content-Type': 'application/xml; charset=utf-8' }, body,
  });
}

// wrapReport builds a namespaced report root around inner XML.
export function xmlDoc(rootPrefixed, attrs, inner) {
  const a = attrs ? ' ' + attrs : '';
  return `<?xml version="1.0" encoding="utf-8"?>\n<${rootPrefixed} ${XMLNS}${a}>${inner}</${rootPrefixed}>`;
}

// ---- multistatus parsing -------------------------------------------------

export const key = (ns, local) => `{${ns}}${local}`;

function directChildren(el, ns, local) {
  return [...el.children].filter(c => c.namespaceURI === ns && c.localName === local);
}
export function child(el, ns, local) { return directChildren(el, ns, local)[0] || null; }

// msResponses parses a {DAV:}multistatus document into
// [{href, status, props: Map key(ns,local)→Element, propStatus: Map key→'HTTP/1.1 …'}].
export function msResponses(xml) {
  if (!xml) return [];
  const root = xml.documentElement;
  return directChildren(root, NS.d, 'response').map(r => {
    const href = child(r, NS.d, 'href')?.textContent ?? '';
    const status = child(r, NS.d, 'status')?.textContent ?? '';
    const props = new Map();
    const propStatus = new Map();
    for (const ps of directChildren(r, NS.d, 'propstat')) {
      const st = child(ps, NS.d, 'status')?.textContent ?? '';
      const prop = child(ps, NS.d, 'prop');
      if (!prop) continue;
      for (const p of prop.children) {
        const k = key(p.namespaceURI, p.localName);
        propStatus.set(k, st);
        if (st.includes(' 200 ')) props.set(k, p);
      }
    }
    return { href, status, props, propStatus };
  });
}

// syncToken pulls the top-level {DAV:}sync-token off a multistatus.
export function syncToken(xml) {
  return xml ? child(xml.documentElement, NS.d, 'sync-token')?.textContent ?? '' : '';
}

// hrefText returns the text of a {DAV:}href inside a property element.
export function hrefText(propEl) {
  if (!propEl) return '';
  return propEl.getElementsByTagNameNS(NS.d, 'href')[0]?.textContent ?? propEl.textContent.trim();
}

export function hrefTexts(propEl) {
  if (!propEl) return [];
  return [...propEl.getElementsByTagNameNS(NS.d, 'href')].map(h => h.textContent);
}

// resourceTypes lists {ns}local names on a d:resourcetype property element.
export function resourceTypes(propEl) {
  return propEl ? [...propEl.children].map(c => key(c.namespaceURI, c.localName)) : [];
}

// privilegeNames flattens d:privilege elements (cupr-set, ACE grants) to
// prefixed names like d:read, c:read-free-busy.
export function privilegeNames(propEl) {
  if (!propEl) return [];
  return [...propEl.getElementsByTagNameNS(NS.d, 'privilege')]
    .map(p => p.firstElementChild)
    .filter(Boolean)
    .map(e => shortName(e.namespaceURI, e.localName));
}

export function shortName(ns, local) {
  const pfx = Object.entries(NS).find(([, uri]) => uri === ns)?.[0];
  return pfx ? `${pfx}:${local}` : `{${ns}}${local}`;
}

// prettyXML re-indents an XML string for the drawer / output areas.
export function prettyXML(text) {
  let out = '', depth = 0;
  const parts = text.replace(/>\s*</g, '><').split(/(?=<)|(?<=>)/g).filter(Boolean);
  for (const p of parts) {
    if (!p.startsWith('<')) { out = out.replace(/\n$/, '') + p.trim() + '\n'; continue; }
    if (p.startsWith('</')) depth = Math.max(0, depth - 1);
    out += '  '.repeat(depth) + p.trim() + '\n';
    if (p.startsWith('<') && !p.startsWith('</') && !p.endsWith('/>') &&
        !p.startsWith('<?') && !p.startsWith('<!')) depth++;
  }
  return out.trimEnd();
}
