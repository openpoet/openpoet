// Push Notifications using Web Push API
// Default ON — opt-out preference stored server-side (survives cache clear)
class NotificationManager {
    constructor() {
        this.permission = Notification.permission || 'default';
        this.subscription = null;
        this.vapidPublicKey = null;
        this.ready = false;
        this._optedOut = false;

        this.init();
    }

    async init() {
        if (!('serviceWorker' in navigator) || !('PushManager' in window)) {
            console.log('Push notifications not supported');
            return;
        }

        // 1. Get VAPID public key
        try {
            const response = await fetch('/api/notifications/vapid');
            if (response.ok) {
                const data = await response.json();
                this.vapidPublicKey = data.publicKey;
            }
        } catch (error) {
            console.error('Failed to get VAPID key:', error);
            return;
        }

        // 2. Register service worker
        try {
            await navigator.serviceWorker.register('/sw.js');
            console.log('Service Worker registered');
        } catch (error) {
            console.error('Service Worker registration failed:', error);
            return;
        }

        // 3. Check existing subscription
        try {
            const registration = await navigator.serviceWorker.ready;
            this.subscription = await registration.pushManager.getSubscription();
        } catch (error) {
            console.error('Failed to check push subscription:', error);
        }

        this.permission = Notification.permission || 'default';
        this.ready = true;

        // 4. Fetch server-side preference
        try {
            const resp = await fetch('/api/notifications/preference');
            if (resp.ok) {
                const data = await resp.json();
                this._optedOut = !!data.disabled;
            }
        } catch (error) {
            // Fail-open: assume not opted out
            console.warn('Failed to fetch notification preference, assuming enabled:', error);
            this._optedOut = false;
        }

        // 5. If user explicitly opted out, stop here
        if (this._optedOut) {
            console.log('Push notifications: user opted out (server preference)');
            return;
        }

        // 6. If browser denied, nothing we can do
        if (this.permission === 'denied') {
            console.log('Push notifications: blocked by browser');
            return;
        }

        // 7. Permission granted — ensure subscription exists
        if (this.permission === 'granted') {
            if (this.subscription) {
                // Re-confirm subscription with server (in case server lost it)
                await this._sendSubscriptionToServer(this.subscription);
            } else {
                // Subscription lost (e.g. after cache clear) — re-subscribe
                console.log('Permission granted but no subscription, re-subscribing...');
                await this.subscribe();
            }
            return;
        }

        // 8. Permission = 'default' (never asked) — auto-request with delay
        if (this.permission === 'default') {
            console.log('Push notifications: will auto-request permission in 2s');
            setTimeout(() => this.requestPermission(), 2000);
        }
    }

    async requestPermission() {
        if (!('Notification' in window)) {
            return false;
        }

        const permission = await Notification.requestPermission();
        this.permission = permission;

        if (permission === 'granted') {
            await this.subscribe();
            return true;
        }

        return false;
    }

    async subscribe() {
        if (!this.vapidPublicKey) {
            console.error('No VAPID key available');
            return;
        }

        try {
            const registration = await navigator.serviceWorker.ready;

            // Convert VAPID key from base64
            const applicationServerKey = this.urlBase64ToUint8Array(this.vapidPublicKey);

            this.subscription = await registration.pushManager.subscribe({
                userVisibleOnly: true,
                applicationServerKey: applicationServerKey
            });

            // Send subscription to server
            await this._sendSubscriptionToServer(this.subscription);

            // Clear opt-out on server (user is actively enabling)
            await this._setServerPreference(false);
            this._optedOut = false;

            if (typeof app !== 'undefined' && app.showToast) {
                app.showToast('Success', 'Push notifications enabled', 'success');
            }

        } catch (error) {
            console.error('Push subscription failed:', error);
            if (typeof app !== 'undefined' && app.showToast) {
                app.showToast('Error', 'Failed to enable push notifications', 'error');
            }
        }
    }

    async unsubscribe() {
        if (!this.subscription) return;

        try {
            // Unsubscribe from push manager
            await this.subscription.unsubscribe();

            // Remove from server
            await fetch('/api/notifications/subscribe', {
                method: 'DELETE',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ endpoint: this.subscription.endpoint })
            });

            this.subscription = null;

            // Save opt-out preference on server
            await this._setServerPreference(true);
            this._optedOut = true;

            if (typeof app !== 'undefined' && app.showToast) {
                app.showToast('Success', 'Push notifications disabled', 'success');
            }

        } catch (error) {
            console.error('Unsubscribe failed:', error);
        }
    }

    async _sendSubscriptionToServer(subscription) {
        try {
            const response = await fetch('/api/notifications/subscribe', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(subscription.toJSON())
            });
            if (response.ok) {
                console.log('Push subscription confirmed with server');
            }
        } catch (error) {
            console.error('Failed to send subscription to server:', error);
        }
    }

    async _setServerPreference(disabled) {
        try {
            await fetch('/api/notifications/preference', {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ disabled })
            });
        } catch (error) {
            console.error('Failed to save notification preference:', error);
        }
    }

    urlBase64ToUint8Array(base64String) {
        const padding = '='.repeat((4 - base64String.length % 4) % 4);
        const base64 = (base64String + padding)
            .replace(/-/g, '+')
            .replace(/_/g, '/');

        const rawData = window.atob(base64);
        const outputArray = new Uint8Array(rawData.length);

        for (let i = 0; i < rawData.length; ++i) {
            outputArray[i] = rawData.charCodeAt(i);
        }
        return outputArray;
    }

    isSubscribed() {
        return !!this.subscription;
    }

    isOptedOut() {
        return this._optedOut;
    }

    isSupported() {
        return 'serviceWorker' in navigator && 'PushManager' in window;
    }
}

// Initialize notification manager
window.notificationManager = new NotificationManager();
