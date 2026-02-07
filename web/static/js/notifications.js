// Push Notifications using Web Push API
class NotificationManager {
    constructor() {
        this.permission = Notification.permission || 'default';
        this.subscription = null;
        this.vapidPublicKey = null;
        this.ready = false;

        this.init();
    }

    async init() {
        if (!('serviceWorker' in navigator) || !('PushManager' in window)) {
            console.log('Push notifications not supported');
            return;
        }

        // Get VAPID public key
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

        // Register service worker
        try {
            const registration = await navigator.serviceWorker.register('/sw.js');
            console.log('Service Worker registered');

            // Check existing subscription
            this.subscription = await registration.pushManager.getSubscription();
            this.permission = Notification.permission || 'default';
            this.ready = true;

            if (this.subscription) {
                console.log('Already subscribed to push notifications');
            } else if (this.permission === 'granted') {
                // Permission was granted but subscription lost (e.g. browser cleared it)
                // Re-subscribe automatically
                console.log('Permission granted but no subscription, re-subscribing...');
                await this.subscribe();
            }
        } catch (error) {
            console.error('Service Worker registration failed:', error);
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
            const response = await fetch('/api/notifications/subscribe', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(this.subscription.toJSON())
            });

            if (response.ok) {
                console.log('Push subscription saved');
                app.showToast('Success', 'Push notifications enabled', 'success');
            }

        } catch (error) {
            console.error('Push subscription failed:', error);
            app.showToast('Error', 'Failed to enable push notifications', 'error');
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
            app.showToast('Success', 'Push notifications disabled', 'success');

        } catch (error) {
            console.error('Unsubscribe failed:', error);
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

    isSupported() {
        return 'serviceWorker' in navigator && 'PushManager' in window;
    }
}

// Initialize notification manager
window.notificationManager = new NotificationManager();
