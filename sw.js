// tsdrop service worker: shows outbox notifications and opens the app on the
// item. No caching, no request interception.
'use strict';

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));

function sameOrigin(u) {
  try { return new URL(u, self.location.origin).origin === self.location.origin ? u : '/'; } catch (e) { return '/'; }
}

self.addEventListener('push', (e) => {
  let msg = {};
  try { msg = e.data ? e.data.json() : {}; } catch (err) { msg = { body: e.data ? e.data.text() : '' }; }
  e.waitUntil(self.registration.showNotification(msg.title || 'tsdrop', {
    body: msg.body || 'something is waiting',
    tag: msg.tag || 'tsdrop',
    icon: '/icon.png',
    data: { url: sameOrigin(msg.url || '/') },
  }));
});

self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  const url = (e.notification.data && e.notification.data.url) || '/';
  e.waitUntil((async () => {
    const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const w of wins) {
      if (new URL(w.url).origin === self.location.origin) {
        w.postMessage({ open: url });
        return w.focus();
      }
    }
    return self.clients.openWindow(url);
  })());
});
