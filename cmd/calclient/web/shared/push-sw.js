// push-sw.js — WebDAV-Push service worker core, shared by the calweb app and
// the protocol explorer (each app's sw.js does importScripts('/shared/push-sw.js')).
//
// On an incoming Web Push it relays a {type:'webdav-push', topic, syncToken}
// message to every open app window so the page can run a sync-collection REPORT
// immediately. Because the browser subscription is userVisibleOnly, it also
// shows a notification when no window is focused (required by the Push API).

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()));

self.addEventListener('push', (event) => {
  const text = event.data ? event.data.text() : '';
  // The push-message body is XML; the service worker has no DOMParser, so pull
  // the two fields out with a tolerant regex. Missing fields are fine — the
  // page treats any push as "something changed, go sync".
  const topic = matchTag(text, 'topic');
  const syncToken = matchTag(text, 'sync-token');

  event.waitUntil((async () => {
    const clients = await self.clients.matchAll({ includeUncontrolled: true, type: 'window' });
    for (const client of clients) {
      client.postMessage({ type: 'webdav-push', topic, syncToken });
    }
    // A focused window will refresh silently; otherwise surface a subtle nudge.
    if (!clients.some((c) => c.focused)) {
      await self.registration.showNotification('Calendar updated', {
        body: 'New changes are available.',
        tag: 'webdav-push',
        renotify: false,
        silent: true,
      });
    }
  })());
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  event.waitUntil((async () => {
    const clients = await self.clients.matchAll({ includeUncontrolled: true, type: 'window' });
    if (clients.length) return clients[0].focus();
    if (self.clients.openWindow) return self.clients.openWindow('/cal/');
  })());
});

function matchTag(xml, local) {
  // Match <topic>…</topic> or <P:topic>…</P:topic> (prefixed or default ns).
  const m = new RegExp('<(?:[A-Za-z0-9]+:)?' + local + '\\b[^>]*>([^<]*)</', 'i').exec(xml);
  return m ? m[1].trim() : '';
}
