// push.js — WebDAV-Push (Web Push) client helper. Discovers a collection's
// VAPID key + topic via PROPFIND, creates one browser Web Push subscription,
// and registers it against each collection with a push-register POST. The
// service worker (push-sw.js) relays incoming pushes to the app so it can sync
// within seconds instead of waiting out the poll interval.
//
// Only the browser needs to speak this; the server side is internal/push.

import * as dav from '/shared/dav.js';

export const PUSH_NS = 'https://bitfire.at/webdav-push';

// localStorage key mapping a registered collection path → its subscription
// resource URL (the Location the server returned), so we can DELETE it later.
const REG_KEY = 'calclient.push.registrations';

// supported reports whether this browser can do Web Push at all. Requires a
// secure context (https or localhost) for the service worker.
export function supported() {
  return 'serviceWorker' in navigator &&
    'PushManager' in window &&
    'Notification' in window &&
    window.isSecureContext;
}

function loadRegs() {
  try { return JSON.parse(localStorage.getItem(REG_KEY) || '{}'); }
  catch { return {}; }
}
function saveRegs(regs) { localStorage.setItem(REG_KEY, JSON.stringify(regs)); }

// registeredPaths lists the collection paths this browser currently has a push
// subscription for.
export function registeredPaths() { return Object.keys(loadRegs()); }
export function isRegistered(path) { return path in loadRegs(); }

// ensureServiceWorker registers (once) the service worker at swPath and returns
// its registration. Distinct apps pass their own path/scope (/cal/, /app/).
let swReg = null;
export async function ensureServiceWorker(swPath, scope) {
  if (swReg) return swReg;
  swReg = await navigator.serviceWorker.register(swPath, { scope });
  await navigator.serviceWorker.ready;
  return swReg;
}

// discover reads a collection's advertised push transport and topic. Returns
// {vapidKey, topic} or null when the server doesn't advertise push here.
export async function discover(collectionPath) {
  const body = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:P="${PUSH_NS}">
  <d:prop><P:transports/><P:topic/></d:prop>
</d:propfind>`;
  const r = await dav.req('PROPFIND', collectionPath, {
    headers: { Depth: '0', 'Content-Type': 'application/xml; charset=utf-8' },
    body,
  });
  if (!r.xml) return null;
  const keyEl = r.xml.getElementsByTagNameNS(PUSH_NS, 'vapid-public-key')[0];
  const topicEl = r.xml.getElementsByTagNameNS(PUSH_NS, 'topic')[0];
  if (!keyEl || !keyEl.textContent) return null;
  return { vapidKey: keyEl.textContent.trim(), topic: topicEl?.textContent?.trim() || '' };
}

// getSubscription returns the browser PushSubscription for this registration,
// creating it (and asking permission) if needed. applicationServerKey must
// match the collection's advertised VAPID key.
async function getSubscription(reg, vapidKey) {
  const existing = await reg.pushManager.getSubscription();
  if (existing) return existing;
  const perm = await Notification.requestPermission();
  if (perm !== 'granted') throw new Error('notification permission denied');
  return reg.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: urlB64ToUint8Array(vapidKey),
  });
}

// registerCollection subscribes this browser to change notifications for one
// collection. Returns {topic}. swPath/scope select the service worker.
export async function registerCollection(collectionPath, swPath, scope) {
  if (!supported()) throw new Error('push is not supported in this browser');
  const info = await discover(collectionPath);
  if (!info) throw new Error('server does not advertise push on this collection');
  const reg = await ensureServiceWorker(swPath, scope);
  const sub = await getSubscription(reg, info.vapidKey);
  const json = sub.toJSON(); // { endpoint, keys: { p256dh, auth } }

  const body = String(dav.xml`<?xml version="1.0" encoding="utf-8"?>
<P:push-register xmlns:P="${PUSH_NS}" xmlns:D="DAV:">
  <P:subscription><P:web-push-subscription>
    <P:push-resource>${json.endpoint}</P:push-resource>
    <P:subscription-public-key type="p256dh">${json.keys.p256dh}</P:subscription-public-key>
    <P:auth-secret>${json.keys.auth}</P:auth-secret>
  </P:web-push-subscription></P:subscription>
  <P:trigger><P:content-update><D:depth>infinity</D:depth></P:content-update></P:trigger>
</P:push-register>`);
  const r = await dav.req('POST', collectionPath, {
    headers: { 'Content-Type': 'application/xml; charset=utf-8' },
    body,
  });
  if (r.status !== 201 && r.status !== 204) {
    throw new Error(`registration failed (HTTP ${r.status})`);
  }
  const location = r.headers.get('Location');
  const regs = loadRegs();
  regs[collectionPath] = location || '';
  saveRegs(regs);
  return { topic: info.topic };
}

// unregisterCollection removes this browser's subscription for a collection
// (DELETE of the subscription resource the server handed back).
export async function unregisterCollection(collectionPath) {
  const regs = loadRegs();
  const location = regs[collectionPath];
  delete regs[collectionPath];
  saveRegs(regs);
  if (location) {
    await dav.req('DELETE', location, {}).catch(() => {});
  }
  // Drop the browser subscription entirely once nothing references it.
  if (Object.keys(regs).length === 0 && swReg) {
    const sub = await swReg.pushManager.getSubscription();
    if (sub) await sub.unsubscribe().catch(() => {});
  }
}

// onMessage invokes cb({topic, syncToken}) whenever the service worker relays
// an incoming push. Returns an unsubscribe function.
export function onMessage(cb) {
  const handler = (ev) => {
    if (ev.data && ev.data.type === 'webdav-push') cb(ev.data);
  };
  navigator.serviceWorker.addEventListener('message', handler);
  return () => navigator.serviceWorker.removeEventListener('message', handler);
}

// urlB64ToUint8Array converts a base64url VAPID key to the Uint8Array the Push
// API's applicationServerKey expects.
function urlB64ToUint8Array(base64) {
  const padding = '='.repeat((4 - (base64.length % 4)) % 4);
  const b64 = (base64 + padding).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(b64);
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}
