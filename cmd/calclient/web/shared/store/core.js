// core.js — the store's shared foundation: the mutable state tree, the
// event bus views subscribe to, persisted preferences, and DAV error
// mapping. Every other store module builds on this one; it imports nothing
// itself, so it can never be part of an import cycle.

export const state = {
  user: '', principal: '', displayName: '',
  calHome: '', abHome: '', inbox: '', outbox: '', addresses: [],
  calendars: [],           // {href,name,displayName,color,ctag,description}
  hidden: new Set(),       // calendar hrefs toggled off in the sidebar
  range: null,             // {start: Date, end: Date} currently loaded
  instances: new Map(),    // calHref -> [instance]
  invites: [],             // parsed inbox items
  invitesLoaded: false,    // first inbox fetch finished (drives the panel's loading state)
  shareNotifications: [],  // parsed calendar-share invitations (cs:notification)
  books: [], contacts: [], groups: [], // carddav (groups = X-ADDRESSBOOKSERVER-KIND:group cards)
  prefs: loadPrefs(),
};

export const bus = new EventTarget();
export const emit = type => bus.dispatchEvent(new Event(type));

// ---- preferences ---------------------------------------------------------

function loadPrefs() {
  // tz: IANA name for display, '' = the device's own time zone.
  const defaults = { weekStart: 1, hour12: false, tz: '', defaultCal: '', staySignedIn: false,
    defaultView: 'month', defaultDuration: 60 };
  try {
    return { ...defaults, ...JSON.parse(localStorage.getItem('calweb.prefs') || '{}') };
  } catch { return { ...defaults }; }
}
export function savePrefs(patch) {
  Object.assign(state.prefs, patch);
  localStorage.setItem('calweb.prefs', JSON.stringify(state.prefs));
  emit('prefs');
}

// ---- errors --------------------------------------------------------------

export class DavError extends Error {
  constructor(res, fallback) {
    super(friendly(res.status, fallback));
    this.status = res.status;
    this.conflict = res.status === 412 || res.status === 409;
    this.detail = res.text?.slice(0, 500) ?? '';
    // The friendly message hides the exchange; log it so a report like
    // "changed elsewhere" can be traced to the concrete request (method,
    // path, status, the If-Match sent) from the browser console.
    const cond = res.reqHeaders?.['If-Match'] ? ` if-match=${res.reqHeaders['If-Match']}`
      : res.reqHeaders?.['If-None-Match'] ? ` if-none-match=${res.reqHeaders['If-None-Match']}` : '';
    console.error(`DAV ${res.method ?? '?'} ${res.path ?? res.url ?? '?'} -> ${res.status}${cond}`, this.detail);
  }
}
export function friendly(status, fallback) {
  switch (status) {
    case 401: return 'Wrong username or password.';
    case 403: return "You don't have permission to do that.";
    case 404: return "That item doesn't exist any more.";
    case 409: case 412: return 'This item was changed elsewhere — your view has been refreshed. Please re-apply your edit.';
    case 415: return "That file doesn't look like a calendar or contact file.";
    case 502: case 503: return "Can't reach the calendar server.";
    default: return fallback || 'Something went wrong (HTTP ' + status + ').';
  }
}
export function check(res, okStatuses = [200, 201, 204, 207]) {
  if (!okStatuses.includes(res.status)) throw new DavError(res);
  return res;
}

export function myEmail() {
  const m = state.addresses.find(a => a.startsWith('mailto:'));
  return m ? m.slice(7) : '';
}
