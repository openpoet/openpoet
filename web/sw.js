// Service Worker for DevManager PWA

const CACHE_NAME = 'devmanager-__BUILD_VERSION__';
const OFFLINE_URL = '/';

// Install event - activate immediately
self.addEventListener('install', (event) => {
    event.waitUntil(
        caches.open(CACHE_NAME)
            .then((cache) => cache.add(OFFLINE_URL))
            .then(() => self.skipWaiting())
    );
});

// Activate event - clean up old caches and notify clients
self.addEventListener('activate', (event) => {
    event.waitUntil(
        caches.keys()
            .then((cacheNames) => {
                return Promise.all(
                    cacheNames
                        .filter((name) => name !== CACHE_NAME)
                        .map((name) => caches.delete(name))
                );
            })
            .then(() => self.clients.claim())
            .then(() => {
                // Notify all clients that a new version is active
                return self.clients.matchAll({ type: 'window' }).then((clients) => {
                    clients.forEach((client) => {
                        client.postMessage({
                            type: 'sw_updated',
                            version: CACHE_NAME
                        });
                    });
                });
            })
    );
});

// Fetch event - network-first for app assets, cache-first for vendor/images
self.addEventListener('fetch', (event) => {
    // Skip non-GET requests
    if (event.request.method !== 'GET') return;

    // Skip non-http(s) requests (like chrome-extension://)
    if (!event.request.url.startsWith('http')) return;

    // Skip WebSocket requests
    if (event.request.url.includes('/ws/')) return;

    // Skip API requests (always go to network)
    if (event.request.url.includes('/api/')) return;

    // Network-first strategy for JS/CSS files (always check for updates)
    const isStaticAsset = event.request.url.includes('/static/js/') ||
                         event.request.url.includes('/static/css/');

    if (isStaticAsset) {
        event.respondWith(
            fetch(event.request)
                .then((response) => {
                    // Cache the new version
                    if (response && response.status === 200 && response.type === 'basic') {
                        const responseToCache = response.clone();
                        caches.open(CACHE_NAME)
                            .then((cache) => cache.put(event.request, responseToCache));
                    }
                    return response;
                })
                .catch(() => {
                    // Fallback to cache if network fails
                    return caches.match(event.request);
                })
        );
        return;
    }

    // Cache-first strategy for other assets (images, fonts, vendor libs)
    event.respondWith(
        caches.match(event.request)
            .then((cachedResponse) => {
                if (cachedResponse) {
                    return cachedResponse;
                }

                return fetch(event.request)
                    .then((response) => {
                        // Don't cache non-successful responses
                        if (!response || response.status !== 200 || response.type !== 'basic') {
                            return response;
                        }

                        // Clone the response
                        const responseToCache = response.clone();

                        // Cache the fetched response
                        caches.open(CACHE_NAME)
                            .then((cache) => {
                                cache.put(event.request, responseToCache);
                            });

                        return response;
                    })
                    .catch(() => {
                        // Return offline page for navigation requests
                        if (event.request.mode === 'navigate') {
                            return caches.match(OFFLINE_URL);
                        }
                    });
            })
    );
});

// Push notification event
self.addEventListener('push', (event) => {
    let data = { title: 'DevManager', body: 'New notification' };

    if (event.data) {
        try {
            data = event.data.json();
        } catch (e) {
            data.body = event.data.text();
        }
    }

    // Handle cancel push — close existing notifications instead of showing new one
    if (data.action === 'cancel' && data.data && data.data.session_id) {
        event.waitUntil(
            self.registration.getNotifications({ tag: `session-${data.data.session_id}` })
                .then(notifications => notifications.forEach(n => n.close()))
        );
        return;
    }

    const options = {
        body: data.body,
        icon: '/static/icon-192.png',
        badge: '/static/badge-96.png',
        vibrate: [100, 50, 100],
        data: data.data || {},
        tag: (data.data && data.data.session_id) ? `session-${data.data.session_id}` : undefined,
        renotify: !!(data.data && data.data.session_id)
    };

    event.waitUntil(
        self.registration.showNotification(data.title, options)
    );
});

// Message from page — close notifications on demand
self.addEventListener('message', (event) => {
    if (event.data && event.data.type === 'close_notifications' && event.data.session_id) {
        self.registration.getNotifications({ tag: `session-${event.data.session_id}` })
            .then(notifications => notifications.forEach(n => n.close()));
    }
});

// Notification click event
self.addEventListener('notificationclick', (event) => {
    event.notification.close();

    if (event.action === 'dismiss') {
        return;
    }

    const data = event.notification.data || {};

    // Build target URL from link or fallback to session hash
    let targetUrl = '/';
    if (data.link) {
        targetUrl = '/#link=' + encodeURIComponent(data.link);
    } else if (data.session_id) {
        targetUrl = '/#session=' + data.session_id;
    }

    // Open or focus the app
    event.waitUntil(
        clients.matchAll({ type: 'window', includeUncontrolled: true })
            .then((clientList) => {
                // Focus existing window and navigate via hash (triggers hashchange listener)
                for (const client of clientList) {
                    if (client.url.includes(self.location.origin) && 'focus' in client) {
                        if ('navigate' in client) {
                            return client.navigate(targetUrl)
                                .then(c => c ? c.focus() : null)
                                .catch(() => client.focus());
                        }
                        return client.focus();
                    }
                }
                // Otherwise open new window
                if (clients.openWindow) {
                    return clients.openWindow(targetUrl);
                }
            })
    );
});
