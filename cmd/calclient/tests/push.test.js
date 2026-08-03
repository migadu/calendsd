// Unit tests for the DOM-free logic in web/shared/push.js: the VAPID key
// decoder, the capability gate, the localStorage registration bookkeeping, and
// the service-worker message filter. The subscribe/discover paths need a real
// browser (service worker + Push API + a push service) and are exercised by the
// e2e "Instant updates" spec instead.

import test from 'node:test';
import assert from 'node:assert/strict';
import { Buffer } from 'node:buffer';
import * as push from '../web/shared/push.js';

// ---- global stubs --------------------------------------------------------
// push.js and its dav dependency reach for browser globals at call time, so we
// install minimal fakes and reset them around every test.

function memStorage() {
  const m = new Map();
  return {
    getItem: k => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    removeItem: k => m.delete(k),
    clear: () => m.clear(),
    _map: m,
  };
}

// fakeSW mimics navigator.serviceWorker's EventTarget surface so onMessage can
// register a listener and we can push a relayed message at it.
function fakeSW() {
  const listeners = new Set();
  return {
    addEventListener: (_type, fn) => listeners.add(fn),
    removeEventListener: (_type, fn) => listeners.delete(fn),
    _emit: data => { for (const fn of [...listeners]) fn({ data }); },
    _count: () => listeners.size,
  };
}

let savedFetch;
test.beforeEach(() => {
  savedFetch = globalThis.fetch;
  globalThis.localStorage = memStorage();
  globalThis.sessionStorage = memStorage();
});
test.afterEach(() => {
  globalThis.fetch = savedFetch;
  delete globalThis.localStorage;
  delete globalThis.sessionStorage;
  delete globalThis.navigator;
  delete globalThis.window;
});

const REG_KEY = 'calclient.push.registrations';

// ---- urlB64ToUint8Array --------------------------------------------------

test('urlB64ToUint8Array decodes base64url, restoring - _ and padding', () => {
  // Ground-truth against Node's own base64url decoder for several lengths,
  // including keys that need 1 and 2 '=' of padding and that use the url-safe
  // '-' and '_' alphabet.
  for (const key of ['AQID', 'AQ', 'YWJj', 'a-_z', '_-__']) {
    const got = push.urlB64ToUint8Array(key);
    const want = new Uint8Array(Buffer.from(key, 'base64url'));
    assert.ok(got instanceof Uint8Array);
    assert.deepEqual([...got], [...want], `key ${key}`);
  }
});

test('urlB64ToUint8Array round-trips a realistic 65-byte VAPID key', () => {
  const raw = Uint8Array.from({ length: 65 }, (_, i) => (i * 7 + 3) & 0xff);
  const b64url = Buffer.from(raw).toString('base64url');
  assert.deepEqual([...push.urlB64ToUint8Array(b64url)], [...raw]);
});

// ---- supported() ---------------------------------------------------------

test('supported() requires SW, PushManager, Notification and a secure context', () => {
  globalThis.navigator = { serviceWorker: {} };
  globalThis.window = { PushManager: function () {}, Notification: function () {}, isSecureContext: true };
  assert.equal(push.supported(), true);

  globalThis.window.isSecureContext = false;             // insecure origin
  assert.equal(push.supported(), false);

  globalThis.window.isSecureContext = true;
  delete globalThis.window.PushManager;                  // no Push API
  assert.equal(push.supported(), false);
});

// ---- registration bookkeeping (localStorage) -----------------------------

test('registeredPaths / isRegistered reflect the stored map', () => {
  assert.deepEqual(push.registeredPaths(), []);
  assert.equal(push.isRegistered('/calendars/bob/work/'), false);

  localStorage.setItem(REG_KEY, JSON.stringify({
    '/calendars/bob/work/': '/push/subs/1',
    '/calendars/bob/home/': '/push/subs/2',
  }));
  assert.deepEqual(push.registeredPaths().sort(), ['/calendars/bob/home/', '/calendars/bob/work/']);
  assert.equal(push.isRegistered('/calendars/bob/work/'), true);
  assert.equal(push.isRegistered('/calendars/bob/nope/'), false);
});

test('registeredPaths tolerates a corrupt registrations blob', () => {
  localStorage.setItem(REG_KEY, '{not json');
  assert.deepEqual(push.registeredPaths(), []);
});

test('unregisterCollection drops the entry and DELETEs the stored subscription', async () => {
  localStorage.setItem(REG_KEY, JSON.stringify({
    '/calendars/bob/work/': '/push/subs/xyz',
    '/calendars/bob/home/': '/push/subs/abc',
  }));
  const calls = [];
  globalThis.fetch = async (url, opts) => {
    calls.push({ url, method: opts.method });
    return new Response('', { status: 204 });
  };

  await push.unregisterCollection('/calendars/bob/work/');

  // The subscription resource the server handed back is DELETEd…
  assert.deepEqual(calls, [{ url: '/push/subs/xyz', method: 'DELETE' }]);
  // …and only that collection is forgotten.
  assert.deepEqual(push.registeredPaths(), ['/calendars/bob/home/']);
});

test('unregisterCollection swallows a failed DELETE and still forgets locally', async () => {
  localStorage.setItem(REG_KEY, JSON.stringify({ '/calendars/bob/work/': '/push/subs/gone' }));
  globalThis.fetch = async () => { throw new Error('network down'); };

  await assert.doesNotReject(push.unregisterCollection('/calendars/bob/work/'));
  assert.equal(push.isRegistered('/calendars/bob/work/'), false);
});

test('unregisterCollection is a no-op DELETE when the path was never registered', async () => {
  let called = false;
  globalThis.fetch = async () => { called = true; return new Response('', { status: 204 }); };
  await push.unregisterCollection('/calendars/bob/unknown/');
  assert.equal(called, false);        // no stored Location → no DELETE
});

// ---- onMessage -----------------------------------------------------------

test('onMessage forwards only webdav-push relays and unsubscribes cleanly', () => {
  const sw = fakeSW();
  globalThis.navigator = { serviceWorker: sw };

  const seen = [];
  const off = push.onMessage(m => seen.push(m));
  assert.equal(sw._count(), 1);

  sw._emit({ type: 'webdav-push', topic: 't1', syncToken: 'sync-9' });
  sw._emit({ type: 'something-else', topic: 'ignored' });      // filtered out
  sw._emit(undefined);                                          // no data: ignored
  assert.deepEqual(seen, [{ type: 'webdav-push', topic: 't1', syncToken: 'sync-9' }]);

  off();
  assert.equal(sw._count(), 0);
  sw._emit({ type: 'webdav-push', topic: 't2' });               // after off: no delivery
  assert.equal(seen.length, 1);
});
