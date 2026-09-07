// F33D3R Service Worker — push notification handler
const CACHE_NAME = 'f33d3r-v1';

self.addEventListener('push', event => {
  if (!event.data) return;
  let data;
  try { data = event.data.json(); } catch(e) {
    data = { title: 'F33D3R', body: event.data.text(), data: { url: '/' } };
  }
  const options = {
    body: data.body || '',
    icon: data.icon || '/static/brand/logo.png',
    badge: data.badge || '/static/brand/favicon.png',
    tag: data.tag || 'f33d3r-notification',
    data: data.data || { url: '/' },
    vibrate: data.vibrate || [200, 100, 200],
    requireInteraction: data.requireInteraction || false,
  };
  event.waitUntil(self.registration.showNotification(data.title || 'F33D3R', options));
});

self.addEventListener('notificationclick', event => {
  event.notification.close();
  const url = event.notification.data?.url || '/';
  event.waitUntil(
    clients.matchAll({ type: 'window', includeUncontrolled: true }).then(windowClients => {
      for (const client of windowClients) {
        if (client.url.includes(self.location.origin)) {
          return client.focus().then(c => c.navigate(url));
        }
      }
      return clients.openWindow(url);
    })
  );
});

self.addEventListener('pushsubscriptionchange', event => {
  event.waitUntil(
    self.registration.pushManager.subscribe(event.oldSubscription.options).then(subscription => {
      return fetch('/events', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ event_type: 'push.subscription.rotated', subscription: subscription.toJSON() })
      });
    })
  );
});
