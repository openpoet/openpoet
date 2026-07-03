// Hook Manager - handles Claude Code hook events via WebSocket
class HookManager {
    constructor() {
        this.pendingPermission = null; // { sessionId, event, timestamp }
        this.maxToolEventsPerSession = 50;
        this.panelVisible = false;
        this.storageKeyEvents = 'openpoet_tool_events';
        this.storageKeyDismissed = 'openpoet_dismissed_requests';

        // Load persisted state from localStorage
        this.toolEventsBySession = this._loadFromStorage(this.storageKeyEvents) || {};
        this.dismissedRequests = this._loadFromStorage(this.storageKeyDismissed) || [];

        // Execution mode tracking per session
        this.sessionModes = {}; // sessionID -> 'plan_mode' | 'executing' | 'idle'

        // Persisted plan cache per session
        this.sessionPlans = {}; // sessionID -> { content, updatedAt }
        this.sessionDocs = {}; // sessionID -> [{ id, title, type, status, created_at }]

        this.setupUI();
    }

    // Auto-reopen the first pending dismissed request (called after sessions are loaded)
    // Validates with backend before reopening to avoid showing stale dialogs
    async _autoReopenDismissed() {
        this.cleanupStaleDismissedRequests();
        this.cleanupStaleToolEvents();

        if (this.dismissedRequests.length === 0) return;

        const entry = this.dismissedRequests[0];

        // Verify with backend if still pending
        try {
            const resp = await fetch(`/api/hooks/pending/${entry.sessionId}`);
            if (resp.ok) {
                const state = await resp.json();
                if (!state.pending_permission) {
                    // Stale — remove
                    this.dismissedRequests.shift();
                    this._saveDismissedRequests();
                    this.hidePendingBadge(entry.sessionId);
                    this.renderToolPanel();
                    this.updateToolBadge();
                    return;
                }
            }
        } catch (err) { /* on error, proceed with reopen */ }

        // Only reopen if the session is the active one
        if (entry.sessionId === this._getActiveSessionId()) {
            this.reopenDialog(entry.id);
        }
        // If not the active session, the badge is already there and onSessionSwitch will handle it
    }

    // ==================== PERSISTENCE ====================

    _loadFromStorage(key) {
        try {
            const data = localStorage.getItem(key);
            return data ? JSON.parse(data) : null;
        } catch (e) {
            console.warn('Failed to load from localStorage:', key, e);
            return null;
        }
    }

    _saveToolEvents(retrying) {
        try {
            localStorage.setItem(this.storageKeyEvents, JSON.stringify(this.toolEventsBySession));
        } catch (e) {
            if (retrying) {
                console.error('Still failed after pruning tool events:', e);
                return;
            }
            // Quota exceeded — aggressively prune and retry once
            console.warn('Tool events storage quota exceeded, pruning stale sessions');
            this._pruneStaleToolEvents(5);
            this._saveToolEvents(true);
        }
    }

    _saveDismissedRequests() {
        try {
            localStorage.setItem(this.storageKeyDismissed, JSON.stringify(this.dismissedRequests));
        } catch (e) {
            console.warn('Failed to save dismissed requests:', e);
        }
    }

    cleanupStaleDismissedRequests() {
        const sessions = window.app?.sessions || [];
        const activeSessionIds = new Set(
            sessions.filter(s => s.status === 'running' || s.status === 'starting').map(s => s.id)
        );

        const before = this.dismissedRequests.length;
        this.dismissedRequests = this.dismissedRequests.filter(entry => activeSessionIds.has(entry.sessionId));

        if (this.dismissedRequests.length !== before) {
            this._saveDismissedRequests();
            this.renderToolPanel();
            this.updateToolBadge();
        }
    }

    // Remove tool events for sessions that are no longer active (in-memory only, no save).
    // Keeps the most recent inactive sessions up to maxKept.
    // Returns the number of sessions removed.
    _pruneStaleToolEvents(maxKept = 20) {
        const sessions = window.app?.sessions || [];
        const activeSessionIds = new Set(
            sessions.filter(s => s.status === 'running' || s.status === 'starting').map(s => s.id)
        );

        const storedSessionIds = Object.keys(this.toolEventsBySession);
        const inactiveIds = storedSessionIds.filter(id => !activeSessionIds.has(id));

        if (inactiveIds.length <= maxKept) return 0;

        // Sort inactive sessions by most recent event timestamp (descending)
        inactiveIds.sort((a, b) => {
            const aTs = this.toolEventsBySession[a]?.[0]?.timestamp || 0;
            const bTs = this.toolEventsBySession[b]?.[0]?.timestamp || 0;
            return bTs - aTs;
        });

        // Remove the oldest inactive sessions beyond maxKept
        const toRemove = inactiveIds.slice(maxKept);
        for (const id of toRemove) {
            delete this.toolEventsBySession[id];
        }

        if (toRemove.length > 0) {
            console.log(`[HookManager] Cleaned up ${toRemove.length} stale session(s) from tool events storage`);
        }
        return toRemove.length;
    }

    // Prune stale tool events and persist
    cleanupStaleToolEvents(maxKept = 20) {
        if (this._pruneStaleToolEvents(maxKept) > 0) {
            this._saveToolEvents();
        }
    }

    // Get the currently active session ID
    _getActiveSessionId() {
        return window.terminalManager?.activeSessionId || null;
    }

    // Get a human-readable session name
    _getSessionName(sessionId) {
        const tabData = window.app?.openTabs?.get(sessionId);
        return tabData?.sessionName || sessionId?.substring(0, 8) || 'Session';
    }

    _getAgentName(event = {}) {
        if (event.backend === 'codex') return 'Codex';
        if (event.backend === 'opencode') return 'OpenCode';
        if (event.backend === 'copilot' || event.backend === 'acp') return 'Copilot';
        return 'Claude';
    }

    // Get tool events for a specific session
    _getSessionEvents(sessionId) {
        if (!sessionId) return [];
        return this.toolEventsBySession[sessionId] || [];
    }

    // Get tool events for the active session
    _getActiveSessionEvents() {
        return this._getSessionEvents(this._getActiveSessionId());
    }

    // Get dismissed requests for the active session
    _getActiveSessionDismissed() {
        const activeId = this._getActiveSessionId();
        if (!activeId) return [];
        return this.dismissedRequests.filter(e => e.sessionId === activeId);
    }

    setupUI() {
        // Permission dialog event listeners
        document.getElementById('hook-permission-allow')?.addEventListener('click', () => {
            this.respondToPermission('allow');
        });
        document.getElementById('hook-permission-allow-always')?.addEventListener('click', () => {
            this.respondToPermission('allowAlways');
        });
        document.getElementById('hook-permission-deny')?.addEventListener('click', () => {
            this.respondToPermission('deny');
        });
        document.getElementById('hook-permission-dismiss')?.addEventListener('click', () => {
            this.dismissDialog('permission');
        });

        // ExitPlanMode dialog buttons
        document.getElementById('hook-plan-submit')?.addEventListener('click', () => {
            this.submitPlanChoice();
        });
        document.getElementById('hook-plan-dismiss')?.addEventListener('click', () => {
            this.dismissDialog('exitPlan');
        });

        // Auto-select feedback radio when typing or focusing in the feedback field
        const planFeedback = document.getElementById('hook-plan-feedback');
        if (planFeedback) {
            const selectFeedbackRadio = () => {
                const radio = document.getElementById('plan-opt-4');
                if (radio) radio.checked = true;
            };
            planFeedback.addEventListener('focus', selectFeedbackRadio);
            planFeedback.addEventListener('input', selectFeedbackRadio);
        }

        // Auto-select deny reason: when typing in deny reason, no radio needed (it's the deny button)
        // but for permission dialog the field is already tied to the Deny button
        const denyReason = document.getElementById('hook-permission-deny-reason');
        if (denyReason) {
            denyReason.addEventListener('input', () => {
                // Visual hint: highlight deny button when user types a reason
                const denyBtn = document.getElementById('hook-permission-deny');
                if (denyBtn && denyReason.value.trim()) {
                    denyBtn.classList.add('btn-deny-active');
                } else if (denyBtn) {
                    denyBtn.classList.remove('btn-deny-active');
                }
            });
        }

        // AskUserQuestion dialog buttons
        document.getElementById('hook-ask-user-submit')?.addEventListener('click', () => {
            this.submitAskUserAnswers();
        });
        document.getElementById('hook-ask-user-dismiss')?.addEventListener('click', () => {
            this.dismissDialog('askUser');
        });

        // Session Documents button (was "View Plan")
        document.getElementById('btn-view-plan')?.addEventListener('click', () => {
            this.showDocuments();
        });

        // Task Loaded dialog buttons
        document.getElementById('hook-task-loaded-start')?.addEventListener('click', () => {
            this.respondToTaskLoaded('start');
        });
        document.getElementById('hook-task-loaded-add-prompt')?.addEventListener('click', () => {
            this.toggleTaskLoadedExtra();
        });
        document.getElementById('hook-task-loaded-plan')?.addEventListener('click', () => {
            this.respondToTaskLoaded('plan');
        });
        document.getElementById('hook-task-loaded-send-extra')?.addEventListener('click', () => {
            this.respondToTaskLoaded('custom');
        });
        document.getElementById('hook-task-loaded-dismiss')?.addEventListener('click', () => {
            this.respondToTaskLoaded('start');
        });

        // Tool panel toggle
        document.getElementById('btn-tool-activity')?.addEventListener('click', () => {
            this.toggleToolPanel();
        });
        document.getElementById('btn-close-tool-panel')?.addEventListener('click', () => {
            this.hideToolPanel();
        });

        // Voice buttons for static modals
        this.setupVoiceButton(
            document.getElementById('hook-permission-voice-btn'),
            document.getElementById('hook-permission-deny-reason')
        );
        this.setupVoiceButton(
            document.getElementById('hook-plan-voice-btn'),
            document.getElementById('hook-plan-feedback')
        );
        this.setupVoiceButton(
            document.getElementById('hook-task-loaded-voice-btn'),
            document.getElementById('hook-task-loaded-extra-prompt')
        );
    }

    // Setup a voice recording button linked to a text input
    setupVoiceButton(buttonEl, inputEl) {
        if (!buttonEl || !inputEl) return;
        buttonEl.addEventListener('click', (e) => {
            e.preventDefault();
            e.stopPropagation();
            if (!window.voiceInput) return;
            if (window.voiceInput.isRecording) {
                window.voiceInput.stopRecording();
                buttonEl.classList.remove('recording');
                return;
            }
            buttonEl.classList.add('recording');
            window.voiceInput.startRecordingWithCallback((text) => {
                buttonEl.classList.remove('recording');
                inputEl.value = (inputEl.value ? inputEl.value + ' ' : '') + text;
                inputEl.focus();
                inputEl.dispatchEvent(new Event('input', { bubbles: true }));
            });
        });
    }

    // Cancel any active voice recording in hook modals
    _cancelVoiceRecording() {
        if (window.voiceInput?.isRecording) {
            window.voiceInput.cancelRecording();
        }
        document.querySelectorAll('.hook-voice-btn.recording').forEach(b => b.classList.remove('recording'));
    }

    // Handle incoming WebSocket hook messages
    handleMessage(msg) {
        console.log('[HookManager] handleMessage:', msg.type, 'session:', msg.data?.session_id, 'active:', this._getActiveSessionId());

        // Dedup: hooks now arrive via both hooks:{id} and events channels.
        // If we already have a pending or dismissed entry for this session, skip duplicates.
        if ((msg.type === 'hook_permission_request' ||
             msg.type === 'hook_ask_user' ||
             msg.type === 'hook_exit_plan') &&
            msg.data?.session_id) {
            const sid = msg.data.session_id;
            if (this.pendingPermission?.sessionId === sid) {
                console.log('[HookManager] dedup: already pending for', sid);
                return;
            }
            if (this.dismissedRequests.some(e => e.sessionId === sid)) {
                console.log('[HookManager] dedup: already dismissed for', sid);
                return;
            }
        }

        switch (msg.type) {
            case 'hook_permission_request':
                this._handlePermissionRequest(msg.data);
                break;
            case 'hook_permission_resolved':
                this.hidePermissionDialog();
                this.hideAskUserDialog();
                this.hideExitPlanDialog();
                // Clean up any dismissed requests for this session
                if (msg.data && msg.data.session_id) {
                    this.dismissedRequests = this.dismissedRequests.filter(e => e.sessionId !== msg.data.session_id);
                    this._saveDismissedRequests();
                    this.hidePendingBadge(msg.data.session_id);
                    this.renderToolPanel();
                    this.updateToolBadge();
                    // Refresh sidebar session cards so the pending badge is cleared
                    if (window.app?.loadSessions) window.app.loadSessions();
                    // Close push notifications via Service Worker
                    if ('serviceWorker' in navigator && navigator.serviceWorker.controller) {
                        navigator.serviceWorker.controller.postMessage({
                            type: 'close_notifications',
                            session_id: msg.data.session_id
                        });
                    }
                }
                break;
            case 'hook_ask_user':
                this._handleAskUser(msg.data);
                break;
            case 'hook_exit_plan':
                this._handleExitPlan(msg.data);
                break;
            case 'hook_tool_start':
                this.addToolEvent(msg.data, 'running');
                this._trackModeFromToolStart(msg.data);
                break;
            case 'hook_tool_done':
                this.updateToolEvent(msg.data, 'completed');
                break;
            case 'hook_tool_failed':
                this.updateToolEvent(msg.data, 'failed');
                break;
            case 'hook_notification':
                this.handleNotification(msg.data);
                break;
            case 'hook_stop':
                this.handleStop(msg.data);
                this._setSessionMode(msg.data?.session_id, 'idle');
                break;
            case 'hook_task_loaded':
                this._handleTaskLoaded(msg.data);
                break;
            case 'session_plan_updated':
                if (msg.data?.session_id) {
                    this.fetchPlan(msg.data.session_id);
                    this.fetchSessionDocs(msg.data.session_id);
                }
                break;
        }
    }

    // ==================== SESSION-AWARE ROUTING ====================

    // Route permission request: show dialog if active session, toast if not
    _handlePermissionRequest(data) {
        const sessionId = data.session_id;
        const activeId = this._getActiveSessionId();
        console.log('[HookManager] _handlePermissionRequest session:', sessionId, 'active:', activeId, 'match:', sessionId === activeId);
        if (sessionId === activeId) {
            this.showPermissionDialog(data);
        } else {
            this._dismissToBackground(data, 'permission');
        }
    }

    _handleAskUser(data) {
        const sessionId = data.session_id;
        const activeId = this._getActiveSessionId();
        console.log('[HookManager] _handleAskUser session:', sessionId, 'active:', activeId, 'match:', sessionId === activeId);
        if (sessionId === activeId) {
            this.showAskUserDialog(data);
        } else {
            this._dismissToBackground(data, 'askUser');
        }
    }

    _handleExitPlan(data) {
        const sessionId = data.session_id;
        const activeId = this._getActiveSessionId();
        console.log('[HookManager] _handleExitPlan session:', sessionId, 'active:', activeId, 'match:', sessionId === activeId);
        if (sessionId === activeId) {
            this.showExitPlanDialog(data);
        } else {
            this._dismissToBackground(data, 'exitPlan');
        }
    }

    // Auto-dismiss to background: store as dismissed and show toast
    _dismissToBackground(data, dialogType) {
        const sessionId = data.session_id;
        const event = data.event || {};

        // Clean up old dismissed for this session
        this.dismissedRequests = this.dismissedRequests.filter(e => e.sessionId !== sessionId);

        const entry = {
            id: `dismissed-${Date.now()}`,
            sessionId: sessionId,
            event: event,
            dialogType: dialogType,
            timestamp: Date.now()
        };

        this.dismissedRequests.push(entry);
        this._saveDismissedRequests();

        // Show badge on tab
        this.showPendingBadge(sessionId);

        // Show toast
        let label = '';
        if (dialogType === 'permission') {
            label = `Permission: ${event.tool_name || 'Tool'}`;
        } else if (dialogType === 'exitPlan') {
            label = 'Plan ready for review';
        } else if (dialogType === 'askUser') {
            label = `Question from ${this._getAgentName(event)}`;
        } else if (dialogType === 'taskLoaded') {
            label = `Task loaded: ${event.task_title || 'Task'}`;
        }
        this._showHookToast(sessionId, label, entry.id);

        this.renderToolPanel();
        this.updateToolBadge();
    }

    // Show a clickable toast notification for hooks from non-active sessions
    _showHookToast(sessionId, label, entryId) {
        console.log('[HookManager] _showHookToast session:', sessionId, 'label:', label);
        const sessionName = this._getSessionName(sessionId);

        // Create toast directly on document.body with very high z-index
        // to ensure visibility even when terminal view is active
        const toast = document.createElement('div');
        toast.className = 'hook-toast-floating';
        toast.innerHTML = `
            <span class="hook-toast-session-badge">${this.escapeHtml(sessionName)}</span>
            <span class="toast-message">${this.escapeHtml(label)}</span>
            <button class="toast-close" onclick="event.stopPropagation(); this.parentElement.remove();" title="Close">
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                    <line x1="18" y1="6" x2="6" y2="18"></line>
                    <line x1="6" y1="6" x2="18" y2="18"></line>
                </svg>
            </button>
        `;

        // Click toast to switch to that session and reopen dialog simultaneously
        toast.addEventListener('click', () => {
            toast.remove();
            this.openSessionAndDialog(sessionId, entryId);
        });

        document.body.appendChild(toast);

        // Vibrate for haptic feedback on mobile
        if (navigator.vibrate) {
            navigator.vibrate([200, 100, 200]);
        }

        // Auto-remove after 15s (longer since this is actionable)
        setTimeout(() => {
            if (toast.parentElement) toast.remove();
        }, 15000);
    }

    // ==================== RESTORE FROM SERVER ====================

    // Called after WebSocket connects to restore pending hooks from backend
    restoreFromServer(state, sessionId) {
        if (!state) return;

        // Merge tool events from server (dedup by tool_use_id)
        if (state.tool_events && state.tool_events.length > 0) {
            const existingIds = new Set(
                (this.toolEventsBySession[sessionId] || []).map(e => e.id)
            );

            for (const serverEvent of state.tool_events) {
                const event = serverEvent.event || {};
                const toolId = event.tool_use_id || `server-${serverEvent.timestamp}`;
                if (existingIds.has(toolId)) continue;

                const toolName = event.tool_name || 'Unknown';
                const toolInput = event.tool_input || {};

                // Determine status from msg_type
                let status = 'running';
                if (serverEvent.msg_type === 'hook_tool_done') status = 'completed';
                else if (serverEvent.msg_type === 'hook_tool_failed') status = 'failed';

                const entry = {
                    id: toolId,
                    sessionId,
                    toolName,
                    toolInput,
                    status,
                    timestamp: new Date(serverEvent.timestamp).getTime()
                };

                if (!this.toolEventsBySession[sessionId]) {
                    this.toolEventsBySession[sessionId] = [];
                }
                this.toolEventsBySession[sessionId].push(entry);
                existingIds.add(toolId);
            }

            // Sort by timestamp descending and trim
            if (this.toolEventsBySession[sessionId]) {
                this.toolEventsBySession[sessionId].sort((a, b) => b.timestamp - a.timestamp);
                if (this.toolEventsBySession[sessionId].length > this.maxToolEventsPerSession) {
                    this.toolEventsBySession[sessionId] = this.toolEventsBySession[sessionId].slice(0, this.maxToolEventsPerSession);
                }
            }
            this._saveToolEvents();
        }

        // Restore pending permission
        if (state.pending_permission) {
            const pp = state.pending_permission;
            const activeId = this._getActiveSessionId();

            // Check if we already have this tracked
            if (this.pendingPermission?.sessionId === sessionId) return;
            if (this.dismissedRequests.some(e => e.sessionId === sessionId)) return;

            const data = {
                session_id: sessionId,
                event: pp.event
            };

            if (sessionId === activeId) {
                // Active session: show dialog directly
                if (pp.msg_type === 'permission') {
                    this.showPermissionDialog(data);
                } else if (pp.msg_type === 'askUser') {
                    this.showAskUserDialog(data);
                } else if (pp.msg_type === 'exitPlan') {
                    this.showExitPlanDialog(data);
                }
            } else {
                // Not active: dismiss to background with toast
                this._dismissToBackground(data, pp.msg_type);
            }
        }

        // Restore pending task notification (shown when session starts from a task)
        if (state.task_notification && !this.pendingPermission) {
            this.showTaskLoadedDialog(state.task_notification);
        }

        this.renderToolPanel();
        this.updateToolBadge();
    }

    // ==================== PERMISSION DIALOG ====================

    showPermissionDialog(data) {
        const dialog = document.getElementById('hook-permission-dialog');
        if (!dialog) return;

        // Clean up old dismissed requests for this session (backend cancels previous)
        this.dismissedRequests = this.dismissedRequests.filter(e => e.sessionId !== data.session_id);
        this._saveDismissedRequests();

        const event = data.event || {};
        this.pendingPermission = {
            sessionId: data.session_id,
            event: event,
            timestamp: Date.now()
        };

        // Show session label
        this._updateDialogSessionLabel('hook-permission-session-label', data.session_id);

        // Extract tool info
        const toolName = event.tool_name || 'Unknown Tool';
        const toolInput = event.tool_input || {};

        // Build tool details display
        let detailsHtml = '';
        if (toolName === 'Bash' || toolName === 'bash') {
            const command = toolInput.command || toolInput.cmd || toolInput.action?.command || '';
            detailsHtml = `<div class="hook-detail-label">Command:</div>
                <pre class="hook-detail-code">${this.escapeHtml(command)}</pre>`;
        } else if (toolName === 'Edit' || toolName === 'Write' || toolName === 'edit' || toolName === 'write' || toolName === 'FileChange') {
            const filePath = toolInput.file_path || toolInput.path || toolInput.grantRoot || toolInput.item?.path || '';
            detailsHtml = `<div class="hook-detail-label">File:</div>
                <div class="hook-detail-value">${this.escapeHtml(filePath)}</div>`;
            if (toolInput.content) {
                const preview = toolInput.content.substring(0, 500);
                detailsHtml += `<div class="hook-detail-label">Content preview:</div>
                    <pre class="hook-detail-code">${this.escapeHtml(preview)}${toolInput.content.length > 500 ? '...' : ''}</pre>`;
            }
        } else {
            // Generic tool display
            const inputStr = JSON.stringify(toolInput, null, 2);
            if (inputStr && inputStr !== '{}') {
                const preview = inputStr.substring(0, 500);
                detailsHtml = `<pre class="hook-detail-code">${this.escapeHtml(preview)}${inputStr.length > 500 ? '...' : ''}</pre>`;
            }
        }

        // Update dialog content
        const nameEl = document.getElementById('hook-permission-tool-name');
        if (nameEl) nameEl.textContent = toolName;

        const detailsEl = document.getElementById('hook-permission-details');
        if (detailsEl) detailsEl.innerHTML = detailsHtml;

        // Show dialog
        dialog.classList.remove('hidden');

        // Play sound/vibrate
        if (navigator.vibrate) {
            navigator.vibrate([100, 50, 100]);
        }

        // Show pending badge on tab
        this.showPendingBadge(data.session_id);

        // Start timeout countdown (590s)
        this.startPermissionTimeout();
    }

    hidePermissionDialog() {
        this._cancelVoiceRecording();
        const dialog = document.getElementById('hook-permission-dialog');
        if (dialog) dialog.classList.add('hidden');

        if (this.pendingPermission) {
            this.hidePendingBadge(this.pendingPermission.sessionId);
        }
        this.pendingPermission = null;

        if (this.permissionTimer) {
            clearTimeout(this.permissionTimer);
            this.permissionTimer = null;
        }
    }

    async respondToPermission(behavior) {
        if (!this.pendingPermission) return;

        const sessionId = this.pendingPermission.sessionId;
        const event = this.pendingPermission.event || {};
        const toolName = event.tool_name || '';
        const denyReason = document.getElementById('hook-permission-deny-reason');
        const message = behavior === 'deny' && denyReason ? denyReason.value : '';

        const body = { behavior, message, tool_name: toolName };

        // For allowAlways, forward permission_suggestions from the hook event
        if (behavior === 'allowAlways' && event.permission_suggestions) {
            body.permission_suggestions = event.permission_suggestions;
        }

        try {
            await fetch(`/api/hooks/permission/${sessionId}/respond`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body)
            });
        } catch (err) {
            console.error('Failed to respond to permission:', err);
        }

        this.hidePermissionDialog();
    }

    startPermissionTimeout() {
        if (this.permissionTimer) clearTimeout(this.permissionTimer);

        this.permissionTimer = setTimeout(() => {
            // Auto-deny on timeout
            if (this.pendingPermission) {
                console.log('Permission request timed out');
                this.hidePermissionDialog();
            }
        }, 590 * 1000);
    }

    showPendingBadge(sessionId) {
        const tab = document.querySelector(`.terminal-tab[data-session-id="${sessionId}"]`);
        if (tab && !tab.querySelector('.hook-pending-badge')) {
            const badge = document.createElement('span');
            badge.className = 'hook-pending-badge';
            badge.textContent = '!';
            tab.appendChild(badge);
        }
    }

    hidePendingBadge(sessionId) {
        const badge = document.querySelector(`.terminal-tab[data-session-id="${sessionId}"] .hook-pending-badge`);
        if (badge) badge.remove();
    }

    // Update session label element in a dialog
    _updateDialogSessionLabel(elementId, sessionId) {
        const el = document.getElementById(elementId);
        if (el) {
            el.textContent = this._getSessionName(sessionId);
            el.classList.remove('hidden');
        }
    }

    // ==================== EXIT PLAN MODE ====================

    showExitPlanDialog(data) {
        const dialog = document.getElementById('hook-exit-plan-dialog');
        if (!dialog) return;

        // Clean up old dismissed requests for this session
        this.dismissedRequests = this.dismissedRequests.filter(e => e.sessionId !== data.session_id);
        this._saveDismissedRequests();

        const event = data.event || {};
        const toolInput = event.tool_input || {};

        this.pendingPermission = {
            sessionId: data.session_id,
            event: event,
            timestamp: Date.now()
        };

        // Show session label
        this._updateDialogSessionLabel('hook-plan-session-label', data.session_id);

        // Show allowed prompts if available
        const promptsEl = document.getElementById('hook-plan-prompts');
        if (promptsEl) {
            const prompts = toolInput.allowedPrompts || [];
            if (prompts.length > 0) {
                promptsEl.innerHTML = prompts.map(p => {
                    const tool = p.tool || '';
                    const prompt = p.prompt || '';
                    return `<div class="plan-prompt-item">
                        <span class="plan-prompt-tool">${this.escapeHtml(tool)}</span>
                        <span class="plan-prompt-text">${this.escapeHtml(prompt)}</span>
                    </div>`;
                }).join('');
                promptsEl.parentElement.classList.remove('hidden');
            } else {
                promptsEl.parentElement.classList.add('hidden');
            }
        }

        // Clear feedback input
        const feedbackEl = document.getElementById('hook-plan-feedback');
        if (feedbackEl) feedbackEl.value = '';

        // Cache plan content for persistent access via View Plan toolbar button
        const planContent = toolInput.plan || '';
        if (planContent && data.session_id) {
            this.sessionPlans[data.session_id] = { content: planContent, updatedAt: new Date().toISOString() };
            this._updateDocsButton();
        }

        // Show View Plan button if plan content is available in the event
        const viewBtn = document.getElementById('hook-plan-view-btn');
        const viewFilename = document.getElementById('hook-plan-view-filename');
        if (viewBtn) {
            viewBtn.classList.add('hidden');
            if (planContent) {
                viewFilename.textContent = 'View Plan';
                viewBtn.classList.remove('hidden');
                viewBtn.onclick = () => {
                    if (window.fileViewer) {
                        window.fileViewer.showPlanContent(planContent, 'Plan');
                    }
                };
            }
        }

        dialog.classList.remove('hidden');

        if (navigator.vibrate) {
            navigator.vibrate([100, 50, 100]);
        }
        this.showPendingBadge(data.session_id);
    }

    hideExitPlanDialog() {
        this._cancelVoiceRecording();
        const dialog = document.getElementById('hook-exit-plan-dialog');
        if (dialog) dialog.classList.add('hidden');
    }

    async respondToPlan(behavior) {
        if (!this.pendingPermission) return;

        const sessionId = this.pendingPermission.sessionId;

        if (behavior === 'passthrough') {
            this.respondToPermission('passthrough');
            this.hideExitPlanDialog();
            return;
        }

        try {
            await fetch(`/api/hooks/permission/${sessionId}/respond`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    behavior: behavior,
                    tool_name: 'ExitPlanMode'
                })
            });
            if (behavior === 'allow') {
                this._onExitPlanApproved(sessionId);
            }
        } catch (err) {
            console.error('Failed to respond to plan:', err);
        }

        this.hideExitPlanDialog();
        if (this.pendingPermission) {
            this.hidePendingBadge(this.pendingPermission.sessionId);
        }
        this.pendingPermission = null;
    }

    async submitPlanChoice() {
        if (!this.pendingPermission) return;

        const sessionId = this.pendingPermission.sessionId;
        const selected = document.querySelector('input[name="plan-option"]:checked');
        if (!selected) return;

        const value = selected.value;

        if (value === 'feedback') {
            // Request changes: DENY the hook with feedback in decision.message
            const feedbackEl = document.getElementById('hook-plan-feedback');
            const feedback = feedbackEl ? feedbackEl.value.trim() : '';
            if (!feedback) return; // Don't submit empty feedback

            try {
                await fetch(`/api/hooks/permission/${sessionId}/respond`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({
                        behavior: 'deny',
                        tool_name: 'ExitPlanMode',
                        message: feedback
                    })
                });
            } catch (err) {
                console.error('Failed to submit plan feedback:', err);
            }
        } else {
            // Approve plan (option 1/2/3): ALLOW the hook, then choice is sent to terminal
            try {
                await fetch(`/api/hooks/permission/${sessionId}/respond`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({
                        behavior: 'allow',
                        tool_name: 'ExitPlanMode',
                        message: value // "1", "2", or "3" — backend writes to terminal after allow
                    })
                });
                this._onExitPlanApproved(sessionId);
            } catch (err) {
                console.error('Failed to submit plan choice:', err);
            }
        }

        this.hideExitPlanDialog();
        if (this.pendingPermission) {
            this.hidePendingBadge(this.pendingPermission.sessionId);
        }
        this.pendingPermission = null;
    }

    // ==================== ASK USER QUESTION ====================

    showAskUserDialog(data) {
        const dialog = document.getElementById('hook-ask-user-dialog');
        if (!dialog) return;

        // Clean up old dismissed requests for this session
        this.dismissedRequests = this.dismissedRequests.filter(e => e.sessionId !== data.session_id);
        this._saveDismissedRequests();

        const event = data.event || {};
        const toolInput = event.tool_input || {};
        const questions = toolInput.questions || [];
        const agentName = this._getAgentName(event);

        this.pendingPermission = {
            sessionId: data.session_id,
            event: event,
            timestamp: Date.now()
        };

        // Show session label
        this._updateDialogSessionLabel('hook-ask-user-session-label', data.session_id);
        const titleEl = document.getElementById('hook-ask-user-title');
        if (titleEl) titleEl.textContent = `${agentName} has a question`;

        const container = document.getElementById('hook-ask-user-questions');
        if (!container) return;

        container.innerHTML = questions.map((q, qIdx) => {
            const isMulti = q.multiSelect === true;
            const inputType = isMulti ? 'checkbox' : 'radio';
            const options = q.options || [];

            let html = `<div class="ask-user-question" data-question-idx="${qIdx}" data-question-text="${this.escapeHtml(q.question || '')}">`;
            if (q.header) {
                html += `<div class="ask-user-header">${this.escapeHtml(q.header)}</div>`;
            }
            html += `<div class="ask-user-question-text">${this.escapeHtml(q.question)}</div>`;
            html += `<div class="ask-user-options">`;

            options.forEach((opt, oIdx) => {
                const id = `ask-q${qIdx}-o${oIdx}`;
                const name = `ask-q${qIdx}`;
                html += `
                    <label class="ask-user-option" for="${id}">
                        <input type="${inputType}" id="${id}" name="${name}" value="${this.escapeHtml(opt.label)}" />
                        <div class="ask-user-option-content">
                            <span class="ask-user-option-label">${this.escapeHtml(opt.label)}</span>
                            ${opt.description ? `<span class="ask-user-option-desc">${this.escapeHtml(opt.description)}</span>` : ''}
                        </div>
                    </label>`;
            });

            // "Other" option with text input and voice button
            const otherId = `ask-q${qIdx}-other`;
            html += `
                <label class="ask-user-option" for="${otherId}">
                    <input type="${inputType}" id="${otherId}" name="ask-q${qIdx}" value="__other__" />
                    <div class="ask-user-option-content">
                        <span class="ask-user-option-label">Other</span>
                        <div class="hook-input-label-row">
                            <input type="text" class="ask-user-other-input form-input" id="${otherId}-text" placeholder="Type or record your answer..." />
                            <button class="btn-icon btn-sm hook-voice-btn ask-user-voice-btn" data-target="${otherId}-text" title="Record voice message">
                                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                                    <path d="M12 1a3 3 0 0 0-3 3v8a3 3 0 0 0 6 0V4a3 3 0 0 0-3-3z"></path>
                                    <path d="M19 10v2a7 7 0 0 1-14 0v-2"></path>
                                    <line x1="12" y1="19" x2="12" y2="23"></line>
                                    <line x1="8" y1="23" x2="16" y2="23"></line>
                                </svg>
                            </button>
                        </div>
                    </div>
                </label>`;

            html += `</div></div>`;
            return html;
        }).join('');

        // Auto-select "Other" radio/checkbox when typing in its text input
        container.querySelectorAll('.ask-user-other-input').forEach(textInput => {
            textInput.addEventListener('input', () => {
                const label = textInput.closest('.ask-user-option');
                if (label) {
                    const radio = label.querySelector('input[type="radio"], input[type="checkbox"]');
                    if (radio) radio.checked = true;
                }
            });
            textInput.addEventListener('focus', () => {
                const label = textInput.closest('.ask-user-option');
                if (label) {
                    const radio = label.querySelector('input[type="radio"], input[type="checkbox"]');
                    if (radio) radio.checked = true;
                }
            });
        });

        // Setup voice buttons for "Other" inputs
        container.querySelectorAll('.ask-user-voice-btn').forEach(btn => {
            const targetId = btn.dataset.target;
            const inputEl = document.getElementById(targetId);
            this.setupVoiceButton(btn, inputEl);
        });

        dialog.classList.remove('hidden');

        if (navigator.vibrate) {
            navigator.vibrate([100, 50, 100]);
        }
        this.showPendingBadge(data.session_id);
    }

    hideAskUserDialog() {
        this._cancelVoiceRecording();
        const dialog = document.getElementById('hook-ask-user-dialog');
        if (dialog) dialog.classList.add('hidden');
    }

    collectAskUserAnswers() {
        const container = document.getElementById('hook-ask-user-questions');
        if (!container) return {};

        const answers = {};
        const questions = container.querySelectorAll('.ask-user-question');

        questions.forEach((qEl) => {
            // Claude Code expects answers keyed by the question's TEXT (the literal
            // `question` field), with values being the chosen option label(s).
            // Multi-select labels join with ", ".
            const key = qEl.dataset.questionText || qEl.dataset.questionIdx;
            const checked = qEl.querySelectorAll('input[type="radio"]:checked, input[type="checkbox"]:checked');
            const values = [];

            checked.forEach(input => {
                if (input.value === '__other__') {
                    const textInput = qEl.querySelector('.ask-user-other-input');
                    if (textInput && textInput.value.trim()) {
                        values.push(textInput.value.trim());
                    }
                } else {
                    values.push(input.value);
                }
            });

            if (values.length > 0) {
                answers[key] = values.join(', ');
            }
        });

        return answers;
    }

    async submitAskUserAnswers() {
        if (!this.pendingPermission) return;

        const sessionId = this.pendingPermission.sessionId;
        const answers = this.collectAskUserAnswers();

        try {
            await fetch(`/api/hooks/permission/${sessionId}/respond`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    behavior: 'allow',
                    tool_name: 'AskUserQuestion',
                    answers: answers
                })
            });
        } catch (err) {
            console.error('Failed to submit answers:', err);
        }

        this.hideAskUserDialog();
        if (this.pendingPermission) {
            this.hidePendingBadge(this.pendingPermission.sessionId);
        }
        this.pendingPermission = null;
    }

    dismissAskUser() {
        this.respondToPermission('passthrough');
        this.hideAskUserDialog();
    }

    // ==================== TASK LOADED DIALOG ====================

    _handleTaskLoaded(data) {
        const sessionId = data.session_id;
        console.log('[HookManager] _handleTaskLoaded session:', sessionId);
        // Always show dialog directly — no toast/dismiss for task loaded
        this.showTaskLoadedDialog(data);
    }

    showTaskLoadedDialog(data) {
        const dialog = document.getElementById('hook-task-loaded-dialog');
        if (!dialog) return;

        const sessionId = data.session_id;

        this.pendingPermission = {
            sessionId: sessionId,
            event: data,
            dialogType: 'taskLoaded',
            timestamp: Date.now()
        };

        // Show session label
        this._updateDialogSessionLabel('hook-task-loaded-session-label', sessionId);

        // Populate task info
        const titleEl = document.getElementById('hook-task-loaded-title');
        if (titleEl) titleEl.textContent = data.task_title || 'Untitled Task';

        const descEl = document.getElementById('hook-task-loaded-description');
        const descSection = document.getElementById('hook-task-loaded-description-section');
        if (data.description) {
            if (descEl) descEl.textContent = data.description;
            if (descSection) descSection.style.display = '';
        } else {
            if (descSection) descSection.style.display = 'none';
        }

        // Reset extra prompt input
        const extraInput = document.getElementById('hook-task-loaded-extra-prompt');
        if (extraInput) extraInput.value = '';
        const extraSection = document.getElementById('hook-task-loaded-extra-section');
        if (extraSection) extraSection.classList.add('hidden');

        dialog.classList.remove('hidden');

        if (navigator.vibrate) {
            navigator.vibrate([100, 50, 100]);
        }
    }

    hideTaskLoadedDialog() {
        const dialog = document.getElementById('hook-task-loaded-dialog');
        if (dialog) dialog.classList.add('hidden');

        if (this.pendingPermission && this.pendingPermission.dialogType === 'taskLoaded') {
            this.hidePendingBadge(this.pendingPermission.sessionId);
            this.pendingPermission = null;
        }
    }

    // Toggle the extra prompt input section
    toggleTaskLoadedExtra() {
        const section = document.getElementById('hook-task-loaded-extra-section');
        if (section) {
            section.classList.toggle('hidden');
            if (!section.classList.contains('hidden')) {
                const input = document.getElementById('hook-task-loaded-extra-prompt');
                if (input) input.focus();
            }
        }
    }

    async respondToTaskLoaded(mode) {
        // mode: 'start' | 'custom' | 'plan'
        if (!this.pendingPermission || this.pendingPermission.dialogType !== 'taskLoaded') return;

        const sessionId = this.pendingPermission.sessionId;

        // Clear the notification on the backend
        try {
            await fetch(`/api/hooks/task-notification/${sessionId}/respond`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' }
            });
        } catch (err) {
            console.error('Failed to clear task notification:', err);
        }

        let prompt;
        if (mode === 'custom') {
            // User's custom prompt replaces the default entirely
            const extraInput = document.getElementById('hook-task-loaded-extra-prompt');
            prompt = extraInput ? extraInput.value.trim() : '';
            if (!prompt) prompt = 'Start working on the assigned task.';
        } else if (mode === 'plan') {
            prompt = 'Plan the implementation for the assigned task. Enter plan mode and present the plan before making any changes.';
        } else {
            prompt = 'Start working on the assigned task.';
        }

        // Use the canonical terminal input function (Ctrl+U, text, Enter with delay)
        if (window.app) {
            window.app.sendQuickCommand(prompt);
        }

        this.hideTaskLoadedDialog();
    }

    // ==================== DISMISS / REOPEN ====================

    dismissDialog(dialogType) {
        if (!this.pendingPermission) return;

        const entry = {
            id: `dismissed-${Date.now()}`,
            sessionId: this.pendingPermission.sessionId,
            event: this.pendingPermission.event,
            dialogType: dialogType,
            timestamp: this.pendingPermission.timestamp
        };

        this.dismissedRequests.push(entry);
        this._saveDismissedRequests();

        // Hide the corresponding dialog without sending anything to backend
        if (dialogType === 'permission') {
            const dialog = document.getElementById('hook-permission-dialog');
            if (dialog) dialog.classList.add('hidden');
        } else if (dialogType === 'exitPlan') {
            const dialog = document.getElementById('hook-exit-plan-dialog');
            if (dialog) dialog.classList.add('hidden');
        } else if (dialogType === 'askUser') {
            const dialog = document.getElementById('hook-ask-user-dialog');
            if (dialog) dialog.classList.add('hidden');
        }

        // Clear pending permission timer but keep the pending badge on the tab
        if (this.permissionTimer) {
            clearTimeout(this.permissionTimer);
            this.permissionTimer = null;
        }

        this.pendingPermission = null;

        // Update panel and badge
        this.renderToolPanel();
        this.updateToolBadge();

        // Auto-open tool activity panel so user sees the pending item
        if (!this.panelVisible) {
            this.toggleToolPanel();
        }
    }

    reopenDialog(entryId) {
        const idx = this.dismissedRequests.findIndex(e => e.id === entryId);
        if (idx === -1) return;

        const entry = this.dismissedRequests[idx];

        // Validate session still exists and is running
        const sessions = window.app?.sessions || [];
        const session = sessions.find(s => s.id === entry.sessionId);
        if (!session || (session.status !== 'running' && session.status !== 'starting')) {
            this.dismissedRequests.splice(idx, 1);
            this._saveDismissedRequests();
            this.renderToolPanel();
            this.updateToolBadge();
            return;
        }

        this.dismissedRequests.splice(idx, 1);
        this._saveDismissedRequests();

        // Restore as pendingPermission
        this.pendingPermission = {
            sessionId: entry.sessionId,
            event: entry.event,
            timestamp: entry.timestamp
        };

        // Show the corresponding dialog with original data
        const data = { session_id: entry.sessionId, event: entry.event };
        if (entry.dialogType === 'permission') {
            this.showPermissionDialog(data);
        } else if (entry.dialogType === 'exitPlan') {
            this.showExitPlanDialog(data);
        } else if (entry.dialogType === 'askUser') {
            this.showAskUserDialog(data);
        } else if (entry.dialogType === 'taskLoaded') {
            this.showTaskLoadedDialog(entry.event);
        }

        this.renderToolPanel();
        this.updateToolBadge();
    }

    // Open session terminal and dialog at the same time.
    // Used by toast click and notification panel click.
    openSessionAndDialog(sessionId, entryId) {
        // Open the session terminal first (non-blocking)
        if (window.terminalManager?.hasSession(sessionId)) {
            window.terminalManager.switchToSession(sessionId);
            if (window.app) {
                window.app.updateTabActiveState(sessionId);
                window.app._updateNavForTerminal();
            }
        } else if (window.app) {
            window.app.openTerminal(sessionId);
        }

        // Open the dialog immediately (no delay)
        if (entryId) {
            this.reopenDialog(entryId);
        }
    }

    // Open session and show dialog for a given session by finding its dismissed entry.
    // Called by notification panel when user clicks a notification.
    openSessionWithPendingDialog(sessionId) {
        // Open the session terminal
        if (window.terminalManager?.hasSession(sessionId)) {
            window.terminalManager.switchToSession(sessionId);
            if (window.app) {
                window.app.updateTabActiveState(sessionId);
                // Ensure terminal view is visible (user may be on a different view)
                window.app.showTerminalView();
            }
        } else if (window.app) {
            window.app.openTerminal(sessionId);
        }

        // Find and reopen any dismissed dialog for this session
        const entry = this.dismissedRequests.find(e => e.sessionId === sessionId);
        if (entry) {
            this.reopenDialog(entry.id);
            return;
        }

        // If there's a pending permission for this session, it may already be showing.
        // If not, fetch from backend and show.
        if (this.pendingPermission?.sessionId === sessionId) return;

        fetch(`/api/hooks/pending/${sessionId}`)
            .then(r => r.ok ? r.json() : null)
            .then(state => {
                if (!state?.pending_permission) return;
                const pp = state.pending_permission;
                const data = { session_id: sessionId, event: pp.event };
                this.pendingPermission = { sessionId, event: pp.event, timestamp: Date.now() };
                if (pp.msg_type === 'permission') {
                    this.showPermissionDialog(data);
                } else if (pp.msg_type === 'askUser') {
                    this.showAskUserDialog(data);
                } else if (pp.msg_type === 'exitPlan') {
                    this.showExitPlanDialog(data);
                }
            })
            .catch(() => {});
    }

    // ==================== TOOL ACTIVITY ====================

    addToolEvent(data, status) {
        const event = data.event || {};
        const toolName = event.tool_name || 'Unknown';
        const toolInput = event.tool_input || {};
        const sessionId = data.session_id;

        // Create a unique-ish key for this tool invocation
        const toolId = event.tool_use_id || `${Date.now()}-${toolName}`;

        // Deduplicate: skip if same tool was added within the last 500ms for this session
        // (hook events are broadcast to both events and hooks channels)
        if (!this.toolEventsBySession[sessionId]) {
            this.toolEventsBySession[sessionId] = [];
        }
        const now = Date.now();
        const recent = this.toolEventsBySession[sessionId].find(
            e => e.toolName === toolName && e.status === status && (now - e.timestamp) < 500
        );
        if (recent) return;

        const entry = {
            id: toolId,
            sessionId,
            toolName,
            toolInput,
            status,
            timestamp: now
        };

        this.toolEventsBySession[sessionId].unshift(entry);
        if (this.toolEventsBySession[sessionId].length > this.maxToolEventsPerSession) {
            this.toolEventsBySession[sessionId].pop();
        }

        this._saveToolEvents();
        this.renderToolPanel();
        this.updateToolBadge();
    }

    updateToolEvent(data, status) {
        const event = data.event || {};
        const toolId = event.tool_use_id;
        const sessionId = data.session_id;
        const events = this._getSessionEvents(sessionId);

        if (toolId) {
            const entry = events.find(e => e.id === toolId);
            if (entry) {
                entry.status = status;
                if (event.tool_output) {
                    entry.output = event.tool_output;
                }
            }
        } else {
            // If no tool_use_id, update the most recent matching tool
            const toolName = event.tool_name;
            const entry = events.find(e => e.toolName === toolName && e.status === 'running');
            if (entry) {
                entry.status = status;
            }
        }

        this._saveToolEvents();
        this.renderToolPanel();
        this.updateToolBadge();
    }

    toggleToolPanel() {
        this.panelVisible = !this.panelVisible;
        const panel = document.getElementById('tool-activity-panel');
        if (panel) {
            panel.classList.toggle('hidden', !this.panelVisible);
        }
        if (this.panelVisible) {
            this.renderToolPanel();
        }
    }

    hideToolPanel() {
        this.panelVisible = false;
        const panel = document.getElementById('tool-activity-panel');
        if (panel) panel.classList.add('hidden');
    }

    renderToolPanel() {
        const list = document.getElementById('tool-activity-list');
        if (!list || !this.panelVisible) return;

        const sessionDismissed = this._getActiveSessionDismissed();
        const sessionEvents = this._getActiveSessionEvents();

        let html = '';

        // Render pending (dismissed) actions section first
        if (sessionDismissed.length > 0) {
            html += '<div class="tool-panel-section-label">Pending Actions</div>';
            html += sessionDismissed.map(entry => {
                const event = entry.event || {};
                const toolName = event.tool_name || '';
                let label = '';
                if (entry.dialogType === 'permission') {
                    label = `Permission: ${toolName || 'Unknown'}`;
                } else if (entry.dialogType === 'exitPlan') {
                    label = 'Plan Review';
                } else if (entry.dialogType === 'askUser') {
                    label = `Question from ${this._getAgentName(event)}`;
                }

                const timeStr = new Date(entry.timestamp).toLocaleTimeString();

                return `
                    <div class="tool-event-item tool-status-pending">
                        <div class="tool-event-header">
                            <span class="pending-pulse"></span>
                            <span class="tool-event-name">${this.escapeHtml(label)}</span>
                            <span class="tool-event-time">${timeStr}</span>
                        </div>
                        <div class="pending-actions">
                            <button class="btn btn-sm btn-warning" onclick="window.hookManager.reopenDialog('${entry.id}')">Reopen</button>
                        </div>
                    </div>
                `;
            }).join('');
        }

        // Render tool events for the active session
        if (sessionEvents.length > 0) {
            if (sessionDismissed.length > 0) {
                html += '<div class="tool-panel-section-label">Tool Events</div>';
            }
            html += sessionEvents.map(entry => {
                const statusClass = `tool-status-${entry.status}`;
                const statusIcon = entry.status === 'running' ? '<span class="spinner-small"></span>' :
                                   entry.status === 'completed' ? '<span class="tool-check">&#10003;</span>' :
                                   '<span class="tool-x">&#10007;</span>';

                let description = '';
                if (entry.toolName === 'Bash' || entry.toolName === 'bash') {
                    description = (entry.toolInput.command || '').substring(0, 80);
                } else if (entry.toolName === 'Edit' || entry.toolName === 'Write') {
                    description = entry.toolInput.file_path || entry.toolInput.path || '';
                } else if (entry.toolName === 'Read') {
                    description = entry.toolInput.file_path || '';
                } else {
                    description = Object.keys(entry.toolInput).join(', ');
                }

                const timeStr = new Date(entry.timestamp).toLocaleTimeString();

                return `
                    <div class="tool-event-item ${statusClass}">
                        <div class="tool-event-header">
                            ${statusIcon}
                            <span class="tool-event-name">${this.escapeHtml(entry.toolName)}</span>
                            <span class="tool-event-time">${timeStr}</span>
                        </div>
                        ${description ? `<div class="tool-event-desc">${this.escapeHtml(description)}</div>` : ''}
                    </div>
                `;
            }).join('');
        }

        if (!html) {
            html = '<div class="empty-state" style="padding:24px;"><p>No tool activity yet</p></div>';
        }

        list.innerHTML = html;
    }

    updateToolBadge() {
        const btn = document.getElementById('btn-tool-activity');
        if (!btn) return;

        const sessionEvents = this._getActiveSessionEvents();
        const sessionDismissed = this._getActiveSessionDismissed();
        const runningCount = sessionEvents.filter(e => e.status === 'running').length;
        const pendingCount = sessionDismissed.length;
        const totalCount = runningCount + pendingCount;
        let badge = btn.querySelector('.tool-activity-badge');

        if (totalCount > 0) {
            if (!badge) {
                badge = document.createElement('span');
                badge.className = 'tool-activity-badge';
                btn.appendChild(badge);
            }
            badge.textContent = totalCount;
            if (pendingCount > 0) {
                badge.classList.add('badge-pending');
            } else {
                badge.classList.remove('badge-pending');
            }
        } else if (badge) {
            badge.remove();
        }
    }

    // ==================== PLAN & DOCUMENTS ====================

    async fetchPlan(sessionId) {
        if (!sessionId) return;
        try {
            const resp = await fetch(`/api/sessions/${sessionId}/plan`);
            if (!resp.ok) return;
            const data = await resp.json();
            if (data.plan_content) {
                this.sessionPlans[sessionId] = {
                    content: data.plan_content,
                    updatedAt: data.plan_updated_at
                };
            } else {
                delete this.sessionPlans[sessionId];
            }
            this._updateDocsButton();
        } catch (err) {
            console.error('[HookManager] Failed to fetch plan:', err);
        }
    }

    async fetchSessionDocs(sessionId) {
        if (!sessionId) return;
        try {
            const resp = await fetch(`/api/sessions/${sessionId}/documents`);
            if (!resp.ok) return;
            this.sessionDocs[sessionId] = await resp.json();
            this._updateDocsButton();
        } catch (err) {
            console.error('[HookManager] Failed to fetch session docs:', err);
        }
    }

    showDocuments() {
        const sessionId = this._getActiveSessionId();
        const docs = this.sessionDocs[sessionId] || [];
        if (docs.length === 0) {
            // Fallback: if only plan is cached but docs not fetched yet, show plan directly
            const plan = this.sessionPlans[sessionId];
            if (plan?.content && window.fileViewer) {
                window.fileViewer.showPlanContent(plan.content, 'Plan');
            }
            return;
        }
        if (docs.length === 1) {
            // Single document — open directly
            this._openSessionDoc(docs[0]);
            return;
        }
        this._showDocsModal(docs);
    }

    _openSessionDoc(doc) {
        if (doc.type === 'plan') {
            const sessionId = doc.id.replace('plan:', '');
            const cached = this.sessionPlans[sessionId];
            if (cached?.content && window.fileViewer) {
                window.fileViewer.showPlanContent(cached.content, doc.title);
            } else {
                // Fetch and show
                fetch(`/api/sessions/${sessionId}/plan`)
                    .then(r => r.json())
                    .then(data => {
                        if (data.plan_content && window.docViewer) {
                            window.docViewer.openWithContent(doc.title, data.plan_content);
                        }
                    });
            }
        } else if (window.docViewer) {
            window.docViewer.open(doc.id);
        }
    }

    _showDocsModal(docs) {
        // Remove existing modal if any
        document.getElementById('session-docs-modal')?.remove();

        const overlay = document.createElement('div');
        overlay.id = 'session-docs-modal';
        overlay.className = 'session-docs-modal';

        const panel = document.createElement('div');
        panel.className = 'session-docs-panel';

        // Header
        const header = document.createElement('div');
        header.className = 'session-docs-header';
        header.innerHTML = `
            <span class="session-docs-title">Session Documents</span>
            <button class="session-docs-close" title="Close">
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                    <line x1="18" y1="6" x2="6" y2="18"></line>
                    <line x1="6" y1="6" x2="18" y2="18"></line>
                </svg>
            </button>
        `;
        panel.appendChild(header);

        // List
        const list = document.createElement('div');
        list.className = 'session-docs-list';

        for (const doc of docs) {
            const row = document.createElement('div');
            row.className = 'session-docs-row';
            const icon = doc.type === 'plan'
                ? '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M4 15s1-1 4-1 5 2 8 2 4-1 4-1V3s-1 1-4 1-5-2-8-2-4 1-4 1z"/><line x1="4" y1="22" x2="4" y2="15"/></svg>'
                : '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M14 2H6a2 2 0 00-2 2v16a2 2 0 002 2h12a2 2 0 002-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>';
            const title = doc.title || 'Untitled';
            const time = doc.created_at ? new Date(doc.created_at).toLocaleTimeString() : '';
            row.innerHTML = `
                <span class="session-docs-row-icon">${icon}</span>
                <span class="session-docs-row-title">${this._escapeHtml(title)}</span>
                <span class="session-docs-row-time">${time}</span>
            `;
            row.addEventListener('click', () => {
                overlay.remove();
                this._openSessionDoc(doc);
            });
            list.appendChild(row);
        }

        panel.appendChild(list);
        overlay.appendChild(panel);

        // Close on backdrop click
        overlay.addEventListener('click', (e) => {
            if (e.target === overlay) overlay.remove();
        });
        header.querySelector('.session-docs-close').addEventListener('click', () => overlay.remove());

        document.body.appendChild(overlay);
    }

    _escapeHtml(text) {
        const div = document.createElement('div');
        div.textContent = text || '';
        return div.innerHTML;
    }

    _updateDocsButton() {
        const btn = document.getElementById('btn-view-plan');
        if (!btn) return;
        const sessionId = this._getActiveSessionId();
        const hasPlan = sessionId && this.sessionPlans[sessionId]?.content;
        const hasDocs = sessionId && this.sessionDocs[sessionId]?.length > 0;
        btn.style.display = (hasPlan || hasDocs) ? '' : 'none';
    }

    // Called when user switches session tab - refresh panel and badge for new session.
    // Auto-reopens pending dialog if the newly active session has one.
    onSessionSwitch() {
        this.renderToolPanel();
        this.updateToolBadge();

        // Fetch plan and docs for new active session if not cached
        const activeId = this._getActiveSessionId();
        if (activeId && !this.sessionPlans[activeId]) {
            this.fetchPlan(activeId);
        }
        if (activeId && !this.sessionDocs[activeId]) {
            this.fetchSessionDocs(activeId);
        }
        this._updateDocsButton();

        // Auto-reopen the first dismissed request for the newly active session
        const pendingForActive = this.dismissedRequests.find(e => e.sessionId === activeId);
        if (pendingForActive) {
            // Verify with backend before reopening
            fetch(`/api/hooks/pending/${activeId}`)
                .then(r => r.ok ? r.json() : null)
                .then(state => {
                    if (state?.pending_permission) {
                        this.reopenDialog(pendingForActive.id);
                    } else {
                        // Backend no longer has it — clean up
                        this.dismissedRequests = this.dismissedRequests.filter(
                            e => e.sessionId !== activeId
                        );
                        this._saveDismissedRequests();
                        this.hidePendingBadge(activeId);
                        this.renderToolPanel();
                        this.updateToolBadge();
                    }
                })
                .catch(() => {
                    // On error, reopen anyway (better to show than to lose)
                    this.reopenDialog(pendingForActive.id);
                });
        }
    }

    // ==================== NOTIFICATIONS & STOP ====================

    handleNotification(data) {
        const event = data.event || {};
        const message = event.message || 'Notification from Claude Code';
        if (window.app) {
            window.app.showToast('Claude Code', message, 'info');
        }
    }

    handleStop(data) {
        // Stop event fires when Claude finishes processing a turn (goes idle),
        // not when the session ends. No toast needed.
    }

    // ==================== EXECUTION MODE TRACKING ====================

    _trackModeFromToolStart(data) {
        const event = data?.event || {};
        const toolName = event.tool_name || '';
        const sessionId = data?.session_id;
        if (!sessionId) return;

        if (toolName === 'EnterPlanMode') {
            this._setSessionMode(sessionId, 'plan_mode');
        } else if (this.sessionModes[sessionId] !== 'plan_mode') {
            this._setSessionMode(sessionId, 'executing');
        }
    }

    _setSessionMode(sessionId, mode) {
        if (!sessionId || this.sessionModes[sessionId] === mode) return;
        this.sessionModes[sessionId] = mode;
        document.dispatchEvent(new CustomEvent('session-mode-changed', {
            detail: { sessionId, mode }
        }));
    }

    // Called when ExitPlanMode is approved — transitions out of plan mode
    _onExitPlanApproved(sessionId) {
        this._setSessionMode(sessionId, 'executing');
    }

    // ==================== SESSION CLEANUP ====================

    // Clear events for a specific session
    clearSession(sessionId) {
        delete this.toolEventsBySession[sessionId];
        delete this.sessionModes[sessionId];
        delete this.sessionPlans[sessionId];
        delete this.sessionDocs[sessionId];
        this._saveToolEvents();
        this._updateDocsButton();

        this.dismissedRequests = this.dismissedRequests.filter(e => e.sessionId !== sessionId);
        this._saveDismissedRequests();

        this.renderToolPanel();
        this.updateToolBadge();

        if (this.pendingPermission && this.pendingPermission.sessionId === sessionId) {
            this.hidePermissionDialog();
        }
    }

    escapeHtml(text) {
        const div = document.createElement('div');
        div.textContent = text || '';
        return div.innerHTML;
    }
}

// Initialize globally
window.hookManager = new HookManager();
