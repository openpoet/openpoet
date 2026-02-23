/**
 * NetworkFeedback — centralized network status and activity feedback.
 *
 * Components:
 *   1. Activity bar  — thin progress bar at viewport top (appears after 300ms of in-flight requests)
 *   2. Status banner — slides down when connectivity is degraded (offline / reconnecting / restored)
 *   3. withLoading() — utility to add loading state to buttons during async operations
 *
 * Integration:
 *   - app.api() calls requestStarted()/requestFinished() to drive the activity bar
 *   - app.updateConnectionStatus() calls onWebSocketChange() to drive the banner
 *   - Online/offline browser events are listened to automatically
 */
class NetworkFeedback {
    constructor() {
        this._inFlightCount = 0;
        this._showBarTimer = null;
        this._wsConnected = false;
        this._bannerState = 'connected'; // 'connected' | 'offline' | 'reconnecting' | 'restored'
        this._restoredTimer = null;

        this.activityBar = document.getElementById('network-activity-bar');
        this.statusBanner = document.getElementById('network-status-banner');

        window.addEventListener('online', () => this._onOnlineChange(true));
        window.addEventListener('offline', () => this._onOnlineChange(false));

        if (!navigator.onLine) {
            this._setBannerState('offline');
        }
    }

    // ---- Activity Bar ----

    requestStarted() {
        this._inFlightCount++;
        if (this._inFlightCount === 1) {
            this._showBarTimer = setTimeout(() => {
                if (this._inFlightCount > 0 && this.activityBar) {
                    this.activityBar.classList.remove('finishing');
                    this.activityBar.classList.add('active');
                }
            }, 300);
        }
    }

    requestFinished() {
        this._inFlightCount = Math.max(0, this._inFlightCount - 1);
        if (this._inFlightCount === 0) {
            clearTimeout(this._showBarTimer);
            this._showBarTimer = null;
            if (this.activityBar && this.activityBar.classList.contains('active')) {
                this.activityBar.classList.remove('active');
                this.activityBar.classList.add('finishing');
                setTimeout(() => {
                    if (this.activityBar) {
                        this.activityBar.classList.remove('finishing');
                    }
                }, 500);
            }
        }
    }

    // ---- Status Banner ----

    /** Called by app.updateConnectionStatus() */
    onWebSocketChange(connected) {
        this._wsConnected = connected;
        if (connected) {
            if (this._bannerState === 'reconnecting' || this._bannerState === 'offline') {
                this._setBannerState('restored');
            }
        } else {
            if (navigator.onLine) {
                this._setBannerState('reconnecting');
            } else {
                this._setBannerState('offline');
            }
        }
    }

    _onOnlineChange(isOnline) {
        if (!isOnline) {
            this._setBannerState('offline');
        } else if (!this._wsConnected) {
            this._setBannerState('reconnecting');
        } else {
            this._setBannerState('restored');
        }
    }

    _setBannerState(state) {
        if (state === this._bannerState && state !== 'restored') return;
        this._bannerState = state;

        clearTimeout(this._restoredTimer);

        if (!this.statusBanner) return;

        this.statusBanner.classList.remove('offline', 'reconnecting', 'restored', 'visible');

        if (state === 'connected') {
            return;
        }

        const messages = {
            offline: "You're offline &mdash; changes won't be saved",
            reconnecting: '<span class="banner-spinner"></span> Reconnecting&hellip;',
            restored: '&#10003; Connection restored',
        };

        this.statusBanner.innerHTML = messages[state];
        this.statusBanner.classList.add(state);

        // Force reflow before adding visible class (for CSS transition)
        void this.statusBanner.offsetHeight;
        this.statusBanner.classList.add('visible');

        if (state === 'restored') {
            this._restoredTimer = setTimeout(() => {
                this.statusBanner.classList.remove('visible');
                setTimeout(() => {
                    this._bannerState = 'connected';
                    this.statusBanner.classList.remove('restored');
                }, 300);
            }, 3000);
        }
    }
}

// ---- withLoading utility ----

/**
 * Run an async operation with button loading state.
 * Usage: await withLoading(saveBtn, () => app.api('PUT', '/path', data));
 */
async function withLoading(button, asyncFn) {
    if (!button || button.classList.contains('is-loading')) return;
    button.classList.add('is-loading');
    button.disabled = true;
    try {
        return await asyncFn();
    } finally {
        button.classList.remove('is-loading');
        button.disabled = false;
    }
}
window.withLoading = withLoading;

// Initialize on DOM ready (script loaded before app.js, elements already in DOM)
window.networkFeedback = new NetworkFeedback();
