// Hook Manager - handles Claude Code hook events via WebSocket
class HookManager {
    constructor() {
        this.pendingPermission = null; // { sessionId, event, timestamp }
        this.toolEvents = []; // Recent tool call events
        this.maxToolEvents = 50;
        this.panelVisible = false;

        this.setupUI();
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
            this.respondToPermission('passthrough');
        });

        // ExitPlanMode dialog buttons
        document.getElementById('hook-plan-submit')?.addEventListener('click', () => {
            this.submitPlanChoice();
        });
        document.getElementById('hook-plan-dismiss')?.addEventListener('click', () => {
            this.respondToPlan('passthrough');
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
            this.dismissAskUser();
        });

        // Tool panel toggle
        document.getElementById('btn-tool-activity')?.addEventListener('click', () => {
            this.toggleToolPanel();
        });
        document.getElementById('btn-close-tool-panel')?.addEventListener('click', () => {
            this.hideToolPanel();
        });
    }

    // Handle incoming WebSocket hook messages
    handleMessage(msg) {
        switch (msg.type) {
            case 'hook_permission_request':
                this.showPermissionDialog(msg.data);
                break;
            case 'hook_permission_resolved':
                this.hidePermissionDialog();
                this.hideAskUserDialog();
                this.hideExitPlanDialog();
                break;
            case 'hook_ask_user':
                this.showAskUserDialog(msg.data);
                break;
            case 'hook_exit_plan':
                this.showExitPlanDialog(msg.data);
                break;
            case 'hook_tool_start':
                this.addToolEvent(msg.data, 'running');
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
                break;
        }
    }

    // ==================== PERMISSION DIALOG ====================

    showPermissionDialog(data) {
        const dialog = document.getElementById('hook-permission-dialog');
        if (!dialog) return;

        const event = data.event || {};
        this.pendingPermission = {
            sessionId: data.session_id,
            event: event,
            timestamp: Date.now()
        };

        // Extract tool info
        const toolName = event.tool_name || 'Unknown Tool';
        const toolInput = event.tool_input || {};

        // Build tool details display
        let detailsHtml = '';
        if (toolName === 'Bash' || toolName === 'bash') {
            const command = toolInput.command || toolInput.cmd || '';
            detailsHtml = `<div class="hook-detail-label">Command:</div>
                <pre class="hook-detail-code">${this.escapeHtml(command)}</pre>`;
        } else if (toolName === 'Edit' || toolName === 'Write' || toolName === 'edit' || toolName === 'write') {
            const filePath = toolInput.file_path || toolInput.path || '';
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

    // ==================== EXIT PLAN MODE ====================

    showExitPlanDialog(data) {
        const dialog = document.getElementById('hook-exit-plan-dialog');
        if (!dialog) return;

        const event = data.event || {};
        const toolInput = event.tool_input || {};

        this.pendingPermission = {
            sessionId: data.session_id,
            event: event,
            timestamp: Date.now()
        };

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

        dialog.classList.remove('hidden');

        if (navigator.vibrate) {
            navigator.vibrate([100, 50, 100]);
        }
        this.showPendingBadge(data.session_id);
    }

    hideExitPlanDialog() {
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

        const event = data.event || {};
        const toolInput = event.tool_input || {};
        const questions = toolInput.questions || [];

        this.pendingPermission = {
            sessionId: data.session_id,
            event: event,
            timestamp: Date.now()
        };

        const container = document.getElementById('hook-ask-user-questions');
        if (!container) return;

        container.innerHTML = questions.map((q, qIdx) => {
            const isMulti = q.multiSelect === true;
            const inputType = isMulti ? 'checkbox' : 'radio';
            const options = q.options || [];

            let html = `<div class="ask-user-question" data-question-idx="${qIdx}">`;
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

            // "Other" option with text input
            const otherId = `ask-q${qIdx}-other`;
            html += `
                <label class="ask-user-option" for="${otherId}">
                    <input type="${inputType}" id="${otherId}" name="ask-q${qIdx}" value="__other__" />
                    <div class="ask-user-option-content">
                        <span class="ask-user-option-label">Other</span>
                        <input type="text" class="ask-user-other-input form-input" id="${otherId}-text" placeholder="Type your answer..." />
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

        dialog.classList.remove('hidden');

        if (navigator.vibrate) {
            navigator.vibrate([100, 50, 100]);
        }
        this.showPendingBadge(data.session_id);
    }

    hideAskUserDialog() {
        const dialog = document.getElementById('hook-ask-user-dialog');
        if (dialog) dialog.classList.add('hidden');
    }

    collectAskUserAnswers() {
        const container = document.getElementById('hook-ask-user-questions');
        if (!container) return {};

        const answers = {};
        const questions = container.querySelectorAll('.ask-user-question');

        questions.forEach((qEl) => {
            const idx = qEl.dataset.questionIdx;
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
                answers[idx] = values.join(', ');
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

    // ==================== TOOL ACTIVITY ====================

    addToolEvent(data, status) {
        const event = data.event || {};
        const toolName = event.tool_name || 'Unknown';
        const toolInput = event.tool_input || {};
        const sessionId = data.session_id;

        // Create a unique-ish key for this tool invocation
        const toolId = event.tool_use_id || `${Date.now()}-${toolName}`;

        const entry = {
            id: toolId,
            sessionId,
            toolName,
            toolInput,
            status,
            timestamp: Date.now()
        };

        this.toolEvents.unshift(entry);
        if (this.toolEvents.length > this.maxToolEvents) {
            this.toolEvents.pop();
        }

        this.renderToolPanel();
        this.updateToolBadge();
    }

    updateToolEvent(data, status) {
        const event = data.event || {};
        const toolId = event.tool_use_id;

        if (toolId) {
            const entry = this.toolEvents.find(e => e.id === toolId);
            if (entry) {
                entry.status = status;
                if (event.tool_output) {
                    entry.output = event.tool_output;
                }
            }
        } else {
            // If no tool_use_id, update the most recent matching tool
            const toolName = event.tool_name;
            const entry = this.toolEvents.find(e => e.toolName === toolName && e.status === 'running');
            if (entry) {
                entry.status = status;
            }
        }

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

        if (this.toolEvents.length === 0) {
            list.innerHTML = '<div class="empty-state" style="padding:24px;"><p>No tool activity yet</p></div>';
            return;
        }

        list.innerHTML = this.toolEvents.map(entry => {
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

    updateToolBadge() {
        const btn = document.getElementById('btn-tool-activity');
        if (!btn) return;

        const runningCount = this.toolEvents.filter(e => e.status === 'running').length;
        let badge = btn.querySelector('.tool-activity-badge');

        if (runningCount > 0) {
            if (!badge) {
                badge = document.createElement('span');
                badge.className = 'tool-activity-badge';
                btn.appendChild(badge);
            }
            badge.textContent = runningCount;
        } else if (badge) {
            badge.remove();
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

    // Clear events for a specific session
    clearSession(sessionId) {
        this.toolEvents = this.toolEvents.filter(e => e.sessionId !== sessionId);
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
