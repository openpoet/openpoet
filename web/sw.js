// Service Worker for DevManager PWA

const CACHE_NAME = 'devmanager-v10';
const OFFLINE_URL = '/';

// Assets to cache
const PRECACHE_ASSETS = [
    '/',
    '/static/css/app.css?v=8',
    '/static/css/mobile.css?v=7',
    '/static/js/app.js?v=8',
    '/static/js/terminal.js?v=5',
    '/static/js/voice.js?v=8',
    '/static/js/files.js?v=8',
    '/static/js/notifications.js?v=2',
    '/static/js/hooks.js?v=8',
    '/static/js/macro-editor.js?v=2',
    '/static/vendor/xterm.js',
    '/static/vendor/xterm.css',
    '/static/vendor/xterm-addon-fit.js',
    '/static/vendor/xterm-addon-web-links.js',
    '/manifest.json'
];

// Install event - cache assets
self.addEventListener('install', (event) => {
    event.waitUntil(
        caches.open(CACHE_NAME)
            .then((cache) => cache.addAll(PRECACHE_ASSETS))
            .then(() => self.skipWaiting())
    );
});

// Activate event - clean up old caches
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
    );
});

// Fetch event - network-first strategy for static assets
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

    // Cache-first strategy for other assets (images, fonts, etc.)
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

    const options = {
        body: data.body,
        icon: '/static/favicon.svg',
        badge: '/static/favicon.svg',
        vibrate: [100, 50, 100],
        data: data.data || {},
        actions: [
            { action: 'open', title: 'Open' },
            { action: 'dismiss', title: 'Dismiss' }
        ]
    };

    event.waitUntil(
        self.registration.showNotification(data.title, options)
    );
});

// Notification click event
self.addEventListener('notificationclick', (event) => {
    event.notification.close();

    if (event.action === 'dismiss') {
        return;
    }

    // Build target URL - if it's a permission notification, include session_id
    const data = event.notification.data || {};
    let targetUrl = '/';
    if (data.session_id) {
        targetUrl = '/#session=' + data.session_id;
    }

    // Open or focus the app
    event.waitUntil(
        clients.matchAll({ type: 'window', includeUncontrolled: true })
            .then((clientList) => {
                // Focus existing window if found
                for (const client of clientList) {
                    if (client.url.includes(self.location.origin) && 'focus' in client) {
                        // Post message to the client to navigate to the session
                        if (data.session_id) {
                            client.postMessage({
                                type: 'navigate_to_session',
                                session_id: data.session_id
                            });
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
