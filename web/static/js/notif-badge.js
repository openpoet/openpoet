// Global Notification Badge Manager
class NotifBadge {
    constructor() {
        this.unreadCount = 0;
        this.activeCount = 0;
        this.panelVisible = false;
        this.notifications = [];

        this.init();
    }

    async init() {
        await this.fetchUnreadCount();
        await this.fetchActiveCount();

        document.getElementById('btn-mark-all-read')?.addEventListener('click', () => {
            this.markAllRead();
        });

        // Close panel on outside click
        document.addEventListener('click', (e) => {
            if (this.panelVisible &&
                !e.target.closest('#notification-panel') &&
                !e.target.closest('#nav-notifications') &&
                !e.target.closest('#mobile-nav-notifications') &&
                !e.target.closest('#notif-float-btn')) {
                this.hide();
            }
        });
    }

    async fetchUnreadCount() {
        try {
            const resp = await fetch('/api/notifications/unread-count');
            if (resp.ok) {
                const data = await resp.json();
                this.updateCount(data.unread_count);
            }
        } catch (err) {
            console.warn('[NotifBadge] Failed to fetch unread count:', err);
        }
    }

    updateCount(count) {
        this.unreadCount = count;

        const sidebarBadge = document.getElementById('notif-count-sidebar');
        if (sidebarBadge) {
            sidebarBadge.textContent = count > 99 ? '99+' : count;
            sidebarBadge.classList.toggle('hidden', count === 0);
        }

        const mobileBadge = document.getElementById('notif-count-mobile');
        if (mobileBadge) {
            mobileBadge.textContent = count > 99 ? '99+' : count;
            mobileBadge.classList.toggle('hidden', count === 0);
        }

        // Update floating button badge and visibility
        const floatBadge = document.getElementById('notif-count-float');
        if (floatBadge) {
            floatBadge.textContent = count > 99 ? '99+' : count;
            floatBadge.classList.toggle('hidden', count === 0);
        }
        this.updateFloatingBtn();
    }

    // Fetch total active notifications count (read or unread) to control floating button visibility
    async fetchActiveCount() {
        try {
            const resp = await fetch('/api/notifications/active');
            if (resp.ok) {
                const data = await resp.json();
                this.activeCount = data ? data.length : 0;
                this.updateFloatingBtn();
            }
        } catch (err) {
            console.warn('[NotifBadge] Failed to fetch active count:', err);
        }
    }

    // Show/hide the desktop floating notification button based on active notifications
    updateFloatingBtn() {
        const btn = document.getElementById('notif-float-btn');
        if (!btn) return;
        const hasNotifications = (this.activeCount > 0) || (this.unreadCount > 0);
        btn.classList.toggle('hidden', !hasNotifications);
    }

    handleCountUpdate(data) {
        this.updateCount(data.unread_count || 0);
        this.fetchActiveCount();
    }

    handleNewNotification(_data) {
        this.activeCount = (this.activeCount || 0) + 1;
        this.updateFloatingBtn();
        if (this.panelVisible) {
            this.loadNotifications();
        }
    }

    toggle() {
        if (this.panelVisible) {
            this.hide();
        } else {
            this.show();
        }
    }

    async show() {
        this.panelVisible = true;
        const panel = document.getElementById('notification-panel');
        if (panel) panel.classList.remove('hidden');
        await this.loadNotifications();
    }

    hide() {
        this.panelVisible = false;
        const panel = document.getElementById('notification-panel');
        if (panel) panel.classList.add('hidden');
    }

    async loadNotifications() {
        try {
            const resp = await fetch('/api/notifications/active');
            if (!resp.ok) return;
            this.notifications = await resp.json();
            this.activeCount = this.notifications ? this.notifications.length : 0;
            this.updateFloatingBtn();
            this.renderPanel();
        } catch (err) {
            console.warn('[NotifBadge] Failed to load notifications:', err);
        }
    }

    renderPanel() {
        const list = document.getElementById('notification-panel-list');
        if (!list) return;

        if (!this.notifications || this.notifications.length === 0) {
            list.innerHTML = '<div class="empty-state" style="padding:24px;"><p>No notifications</p></div>';
            return;
        }

        list.innerHTML = this.notifications.map(n => {
            const time = this._relativeTime(n.created_at);
            const typeIcon = this._typeIcon(n.type);
            return `
                <div class="notification-item unread" data-id="${n.id}" data-session="${n.session_id || ''}">
                    <div class="notification-item-title">${typeIcon} ${this._escapeHtml(n.title)}</div>
                    <div class="notification-item-body">${this._escapeHtml(n.body)}</div>
                    <div class="notification-item-time">${time}</div>
                </div>
            `;
        }).join('');

        list.querySelectorAll('.notification-item').forEach(el => {
            el.addEventListener('click', () => {
                const id = el.dataset.id;
                const sessionId = el.dataset.session;

                // Mark as read (dismiss this notification)
                this.markRead(parseInt(id));
                el.remove();
                this.notifications = this.notifications.filter(n => String(n.id) !== id);
                this.activeCount = this.notifications.length;
                this.updateFloatingBtn();
                if (this.notifications.length === 0) {
                    list.innerHTML = '<div class="empty-state" style="padding:24px;"><p>No notifications</p></div>';
                }

                this.hide();

                // Open session and its pending dialog (permission/question/plan)
                if (sessionId && window.hookManager) {
                    window.hookManager.openSessionWithPendingDialog(sessionId);
                } else if (sessionId && window.app) {
                    window.app.openTerminal(sessionId);
                }
            });
        });
    }

    async markRead(id) {
        try {
            await fetch(`/api/notifications/${id}/read`, { method: 'PUT' });
        } catch (err) {
            console.warn('[NotifBadge] Failed to mark read:', err);
        }
    }

    async markAllRead() {
        try {
            await fetch('/api/notifications/read-all', { method: 'PUT' });
            this.notifications.forEach(n => n.read = true);
            this.activeCount = 0;
            this.updateCount(0);
            this.renderPanel();
        } catch (err) {
            console.warn('[NotifBadge] Failed to mark all read:', err);
        }
    }

    _typeIcon(type) {
        const icons = {
            error: '<span style="color:var(--color-danger)">&#9888;</span>',
            warning: '<span style="color:var(--color-warning)">&#9888;</span>',
            question: '<span style="color:var(--color-primary)">&#10067;</span>',
            permission: '<span style="color:var(--color-warning)">&#128274;</span>',
        };
        return icons[type] || '<span style="color:var(--color-primary)">&#128276;</span>';
    }

    _escapeHtml(text) {
        const div = document.createElement('div');
        div.textContent = text || '';
        return div.innerHTML;
    }

    _relativeTime(dateStr) {
        const now = Date.now();
        const then = new Date(dateStr).getTime();
        const diff = Math.floor((now - then) / 1000);
        if (diff < 60) return 'just now';
        if (diff < 3600) return Math.floor(diff / 60) + 'm ago';
        if (diff < 86400) return Math.floor(diff / 3600) + 'h ago';
        return Math.floor(diff / 86400) + 'd ago';
    }
}

window.notifBadge = new NotifBadge();
