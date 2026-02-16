// DevManager Application
class DevManager {
    constructor() {
        this.ws = null;
        this.currentView = 'projects';
        this.currentSession = null;
        this.projects = [];
        this.sessions = [];
        this.skills = [];
        this.mcpServers = [];

        // Tab management
        this.openTabs = new Map(); // sessionId -> { projectName, sessionName }
        this.tabGroups = new Map(); // projectId -> group element
        this.groupingEnabled = true; // Toggle for flat vs grouped view

        // Split-screen mode
        this.splitScreenMode = false;
        this.splitScreenSessions = []; // Max 2 sessions in split view

        // Session creation tracking (prevent race conditions)
        this.pendingSessionOpen = null; // { sessionId, timestamp }
        this.recentlyCreatedSessions = new Set(); // Track sessions created in last 5 seconds

        this.init();
    }

    init() {
        this.setupNavigation();
        this.setupWebSocket();
        this.loadInitialData();
        this.setupEventListeners();
        this.setupKeyboardShortcuts();
        this.setupStatusListener();

        // Tick elapsed times on session cards every 30s
        setInterval(() => {
            document.querySelectorAll('.session-elapsed[data-start]').forEach(el => {
                el.textContent = this.formatElapsed(el.dataset.start);
            });
        }, 30000);

        // Listen for real-time execution mode changes from HookManager
        document.addEventListener('session-mode-changed', (e) => {
            const { sessionId, mode } = e.detail;
            const badge = document.querySelector(`[data-session-mode="${sessionId}"]`);
            if (badge) {
                const labels = { plan_mode: 'Plan', executing: 'Exec', idle: 'Idle' };
                badge.className = `badge badge-mode-${mode}`;
                badge.textContent = labels[mode] || mode;
            }
        });

        // Setup all tasks filters
        this.setupAllTasksFilters();

        // Create tooltip singleton for truncated session names
        this._tooltipEl = document.createElement('div');
        this._tooltipEl.className = 'session-name-tooltip';
        document.body.appendChild(this._tooltipEl);
        this._activePopup = null;

        // Setup mobile session dropdown
        this.setupMobileSessionDropdown();

        // Setup mobile terminal input
        this.setupMobileTerminalInput();

        // Restore tabs from previous session (after a delay to ensure sessions are loaded)
        setTimeout(() => this.restoreTabsFromStorage(), 1000);

        // Load any pending AI suggestions
        setTimeout(() => this._loadPendingSuggestions(), 2000);

        // Listen for service worker messages
        if ('serviceWorker' in navigator) {
            navigator.serviceWorker.addEventListener('message', (event) => {
                if (event.data && event.data.type === 'navigate_to_session') {
                    this.openTerminal(event.data.session_id);
                } else if (event.data && event.data.type === 'sw_updated') {
                    this.showUpdateBanner();
                }
            });
        }

        // Setup auto-update version check
        this.setupVersionCheck();
    }

    // Navigation
    setupNavigation() {
        // Desktop sidebar navigation
        document.querySelectorAll('.nav-item').forEach(item => {
            item.addEventListener('click', (e) => {
                e.preventDefault();
                const view = item.dataset.view;
                if (view) this.showView(view);
            });
        });

        // Mobile navigation
        document.querySelectorAll('.mobile-nav-item').forEach(item => {
            item.addEventListener('click', (e) => {
                e.preventDefault();
                const view = item.dataset.view;
                if (view) this.showView(view);
            });
        });

        // Sidebar toggle
        document.getElementById('sidebar-toggle')?.addEventListener('click', () => {
            document.getElementById('sidebar').classList.toggle('open');
        });
    }

    showView(viewName) {
        // Hide all views
        document.querySelectorAll('.view').forEach(v => v.classList.remove('active'));

        // Show selected view
        const view = document.getElementById(`view-${viewName}`);
        if (view) view.classList.add('active');

        // Update navigation - project-detail keeps "Projects" active
        const navView = viewName === 'project-detail' ? 'projects' : viewName;
        document.querySelectorAll('.nav-item, .mobile-nav-item').forEach(item => {
            item.classList.toggle('active', item.dataset.view === navView);
        });

        this.currentView = viewName;

        // Close sidebar on mobile
        document.getElementById('sidebar')?.classList.remove('open');

        // Dismiss any active session name tooltip/popup
        this._hideSessionTooltip();
        this._hideSessionPopup();

        // Refresh view data
        this.refreshViewData(viewName);
    }

    async _goToMostRecentSession() {
        // Close sidebar on mobile
        document.getElementById('sidebar')?.classList.remove('open');

        try {
            // Fetch fresh session data
            this.sessions = await this.api('GET', '/sessions');
            const activeSessions = this.sessions.filter(s => s.status === 'running' || s.status === 'starting');

            if (activeSessions.length === 0) {
                this._showSessionsEmptyState();
                return;
            }

            // Sort by start_time descending to get most recent
            activeSessions.sort((a, b) => new Date(b.start_time) - new Date(a.start_time));
            const mostRecent = activeSessions[0];

            // If already on terminal view with this session active, just ensure nav state
            if (this.currentView === 'terminal' && this.currentSession === mostRecent.id) {
                this._updateNavForTerminal();
                return;
            }

            // Open terminal directly
            await this.openTerminal(mostRecent.id);
            this._updateNavForTerminal();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    _showSessionsEmptyState() {
        // Show the sessions view with empty state
        document.querySelectorAll('.view').forEach(v => v.classList.remove('active'));
        document.getElementById('view-sessions').classList.add('active');
        this.currentView = 'sessions-empty';

        // Highlight sessions nav
        document.querySelectorAll('.nav-item, .mobile-nav-item').forEach(item => {
            item.classList.toggle('active', item.dataset.view === 'sessions');
        });

        // Render empty state content
        const container = document.getElementById('sessions-list');
        if (container) {
            container.innerHTML = `
                <div class="empty-state">
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                        <polyline points="4 17 10 11 4 5"></polyline>
                        <line x1="12" y1="19" x2="20" y2="19"></line>
                    </svg>
                    <h3>No active sessions</h3>
                    <p>Click "New Session" to start one</p>
                </div>
            `;
        }
    }

    _updateNavForTerminal() {
        document.querySelectorAll('.nav-item, .mobile-nav-item').forEach(item => {
            item.classList.toggle('active', item.dataset.view === 'sessions');
        });
    }

    refreshViewData(viewName) {
        switch (viewName) {
            case 'projects':
                this.loadProjects();
                break;
            case 'project-detail':
                // Don't refresh on initial navigation - only on explicit refresh
                break;
            case 'sessions':
                this.loadSessions();
                break;
            case 'tasks':
                this.loadAllTasks();
                break;
            case 'config':
                this.loadConfig();
                break;
        }
    }

    // WebSocket
    setupWebSocket() {
        const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        this.ws = new WebSocket(`${protocol}//${window.location.host}/ws/events`);

        this.ws.onopen = () => {
            this.updateConnectionStatus(true);
            this.ws.send(JSON.stringify({ type: 'subscribe', channel: 'events' }));
        };

        this.ws.onclose = () => {
            this.updateConnectionStatus(false);
            setTimeout(() => this.setupWebSocket(), 3000);
        };

        this.ws.onmessage = (event) => {
            const msg = JSON.parse(event.data);
            this.handleWebSocketMessage(msg);
        };

        this.ws.onerror = (error) => {
            console.error('WebSocket error:', error);
        };
    }

    handleWebSocketMessage(msg) {
        switch (msg.type) {
            case 'state_update':
                this.handleStateUpdate(msg.data);
                break;
            case 'notification':
                this.showToast(msg.data.title, msg.data.body, msg.data.type);
                window.notifBadge?.handleNewNotification(msg.data);
                break;
            case 'notification_count':
                window.notifBadge?.handleCountUpdate(msg.data);
                break;
            case 'sync_progress':
                this.handleSyncProgress(msg.data);
                break;
            case 'ai_proactive':
                this.handleAIProactive(msg.data);
                break;
            case 'chat_doc_card':
                window.aiChat?.injectDocCardFromWS(msg.data);
                break;
            case 'ai_suggestion':
                this.handleAIProactive(this._normalizeProactive(msg.data));
                break;
            case 'ping':
                this.ws.send(JSON.stringify({ type: 'pong' }));
                break;
            case 'session_plan_updated':
                if (window.hookManager && msg.data?.session_id) {
                    window.hookManager.fetchPlan(msg.data.session_id);
                }
                break;
            default:
                // Route hook messages to HookManager
                if (msg.type && msg.type.startsWith('hook_') && window.hookManager) {
                    console.log('[EventsWS] routing hook msg to HookManager:', msg.type, 'session:', msg.data?.session_id);
                    window.hookManager.handleMessage(msg);
                }
                break;
        }
    }

    handleStateUpdate(data) {
        switch (data.entity) {
            case 'project':
                this.loadProjects().then(() => {
                    if (this.currentView === 'project-detail' && this._detailProject) {
                        this.showProjectDetail(this._detailProject.id);
                    }
                });
                break;
            case 'memory_doc':
                if (this.currentView === 'project-detail' && this._detailProject && data.data?.project_id === this._detailProject.id) {
                    this.loadMemoryDoc(this._detailProject.id);
                }
                break;
            case 'session':
                // Handle real-time mode changes without full re-fetch
                if (data.data?.action === 'mode_changed' && data.data?.session_id && data.data?.mode) {
                    const badge = document.querySelector(`[data-session-mode="${data.data.session_id}"]`);
                    if (badge) {
                        const labels = { plan_mode: 'Plan', executing: 'Exec', idle: 'Idle' };
                        badge.className = `badge badge-mode-${data.data.mode}`;
                        badge.textContent = labels[data.data.mode] || data.data.mode;
                    }
                    break;
                }
                this.loadSessions();
                // Handle session rename (e.g. when dynamically linked to a task)
                if (data.data?.action === 'renamed' && data.data?.session_id && data.data?.name) {
                    const sessionId = data.data.session_id;
                    const newName = data.data.name;
                    window.terminalManager?.renameSession(sessionId, newName);
                    const tab = document.querySelector(`.terminal-tab[data-session-id="${sessionId}"]`);
                    if (tab) {
                        const nameContainer = tab.querySelector('.terminal-tab-name');
                        if (nameContainer) {
                            const nameText = nameContainer.querySelector('.terminal-tab-name-text');
                            if (nameText) nameText.textContent = newName;
                            else nameContainer.textContent = newName;
                            nameContainer.dataset.fullName = newName;
                        }
                    }
                    // Hide link-task button if this is the active session (task was just linked)
                    if (window.terminalManager?.activeSessionId === sessionId) {
                        const linkBtn = document.getElementById('btn-link-task');
                        if (linkBtn) linkBtn.style.display = 'none';
                    }
                }
                break;
            case 'task':
                if (this.currentView === 'project-detail' && this._detailProject && data.data?.project_id === this._detailProject.id) {
                    this.loadProjectTasks(this._detailProject.id);
                }
                if (this.currentView === 'tasks') {
                    this.loadAllTasks();
                }
                break;
            case 'task_history':
                // Refresh task detail view if currently viewing the affected task
                if (this._viewingTaskDetail && data.data?.task_id === this._viewingTaskDetail.taskId) {
                    this.viewTaskDetail(this._viewingTaskDetail.projectId, this._viewingTaskDetail.taskId);
                }
                break;
            case 'skill':
            case 'mcp':
            case 'settings':
                this.loadConfig();
                break;
        }
    }

    updateConnectionStatus(connected) {
        const status = document.getElementById('connection-status');
        if (status) {
            status.classList.toggle('connected', connected);
            status.querySelector('.status-text').textContent = connected ? 'Connected' : 'Disconnected';
        }
    }

    // API calls
    async api(method, path, data = null) {
        const options = {
            method,
            headers: { 'Content-Type': 'application/json' }
        };
        if (data) options.body = JSON.stringify(data);

        const response = await fetch(`/api${path}`, options);
        if (!response.ok) {
            const error = await response.json();
            throw new Error(error.error || 'Request failed');
        }
        if (response.status === 204) return null;
        return response.json();
    }

    // Load initial data
    async loadInitialData() {
        await Promise.all([
            this.loadProjects(),
            this.loadSessions()
        ]);

        // Now that sessions are loaded, clean up stale dismissed requests and auto-reopen valid ones
        if (window.hookManager) {
            window.hookManager._autoReopenDismissed();
        }
    }

    // Projects
    async loadProjects() {
        try {
            this.projects = await this.api('GET', '/projects');
            this.renderProjects();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    renderProjects() {
        const container = document.getElementById('projects-list');
        if (!container) return;

        if (this.projects.length === 0) {
            container.innerHTML = `
                <div class="empty-state">
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                        <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"></path>
                    </svg>
                    <h3>No projects yet</h3>
                    <p>Add your first project to get started</p>
                </div>
            `;
            return;
        }

        container.innerHTML = this.projects.map(project => `
            <div class="card card-clickable" data-project-id="${project.id}" onclick="app.showProjectDetail(${project.id})">
                <div class="card-header">
                    <div>
                        <div class="card-title">${this.escapeHtml(project.name)}</div>
                        <div class="card-subtitle">${this.escapeHtml(project.path)}</div>
                    </div>
                    <span class="badge badge-${project.type}">${project.type}</span>
                </div>
                <div class="card-body">
                    ${project.type === 'remote' ? `
                        <div class="text-muted">
                            ${this.escapeHtml(project.ssh_user?.String || '')}@${this.escapeHtml(project.ssh_host?.String || '')}
                        </div>
                    ` : ''}
                </div>
                <div class="card-actions">
                    <button class="btn btn-primary btn-sm" onclick="event.stopPropagation(); app.startSession(${project.id})">
                        Start Session
                    </button>
                    <button class="btn btn-secondary btn-sm" onclick="event.stopPropagation(); app.editProject(${project.id})">
                        Edit
                    </button>
                    <button class="btn btn-secondary btn-sm" onclick="event.stopPropagation(); app.syncProjectConfig(${project.id})">
                        Sync
                    </button>
                </div>
            </div>
        `).join('');
    }

    async startSession(projectId) {
        const project = this.projects.find(p => p.id === projectId);
        const projectName = project?.name || 'Session';

        // Step 1: Check for pending tasks and optionally select one
        const taskSelection = await this.showTaskSelectModal(projectId);
        if (taskSelection === false) return; // User cancelled

        // Step 2: If reconnecting to existing active session, just open terminal
        if (taskSelection && taskSelection.reconnect) {
            this.openTerminal(taskSelection.reconnect);
            return;
        }

        // Step 2b: If reopening a stopped session, call reopen API then open terminal
        if (taskSelection && taskSelection.reopen) {
            try {
                const sess = await this.api('POST', `/sessions/${taskSelection.reopen}/reopen`);
                this.showToast('Success', 'Session reopened (continuing conversation)', 'success');
                this.openTerminal(sess.id, sess, sess.name);
            } catch (error) {
                this.showToast('Error', `Failed to reopen session: ${error.message}`, 'error');
            }
            return;
        }

        // Step 3: Determine default session name
        let defaultName;
        let taskId = null;
        if (taskSelection && taskSelection.taskId) {
            taskId = taskSelection.taskId;
            defaultName = `Task: ${taskSelection.taskTitle}`;
        } else {
            defaultName = `${projectName} (${new Date().toLocaleTimeString()})`;
        }

        // Step 4: Show session name modal
        const customName = await this.showSessionNameModal(defaultName);
        if (!customName) return;

        // Step 5: Create session with optional task_id
        try {
            const payload = { project_id: projectId, name: customName };
            if (taskId) payload.task_id = taskId;
            const session = await this.api('POST', '/sessions', payload);

            // Mark this session as recently created (protect from restoreTabsFromStorage)
            this.recentlyCreatedSessions.add(session.id);
            setTimeout(() => this.recentlyCreatedSessions.delete(session.id), 5000);

            this.showToast('Success', taskId ? 'Session started from task' : 'Session started', 'success');
            this.openTerminal(session.id, session, customName);
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    showNewSessionProjectSelect() {
        return new Promise((resolve) => {
            const modal = document.getElementById('project-select-modal');
            const listEl = document.getElementById('project-select-list');
            const cancelBtn = document.getElementById('project-select-cancel');

            let resolved = false;
            const finish = (value) => {
                if (resolved) return;
                resolved = true;
                cleanup();
                resolve(value);
            };

            const cleanup = () => {
                modal.classList.add('hidden');
                cancelBtn.removeEventListener('click', handleCancel);
                document.removeEventListener('keydown', handleKeydown);
            };

            const handleCancel = () => finish(null);
            const handleKeydown = (e) => {
                if (e.key === 'Escape') finish(null);
            };

            modal.classList.remove('hidden');
            cancelBtn.addEventListener('click', handleCancel);
            document.addEventListener('keydown', handleKeydown);

            const projects = this.projects || [];

            if (projects.length === 0) {
                listEl.innerHTML = `
                    <div class="empty-state" style="padding: 24px 0;">
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" style="width: 40px; height: 40px; margin-bottom: 8px; opacity: 0.5;">
                            <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"></path>
                        </svg>
                        <h3 style="margin: 0 0 4px;">No projects yet</h3>
                        <p style="margin: 0; color: var(--color-text-secondary, #666);">Create a project first to start a session.</p>
                    </div>`;
                return;
            }

            listEl.innerHTML = projects.map(project => {
                const typeIcon = project.type === 'remote'
                    ? '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="2" y="3" width="20" height="14" rx="2" ry="2"/><line x1="8" y1="21" x2="16" y2="21"/><line x1="12" y1="17" x2="12" y2="21"/></svg>'
                    : '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"></path></svg>';
                const pathDisplay = project.type === 'remote'
                    ? `${project.ssh_user?.String || ''}@${project.ssh_host?.String || ''}:${project.path}`
                    : project.path;
                return `
                    <div class="task-select-item" style="cursor: pointer;" onclick="(() => { document.getElementById('project-select-modal').classList.add('hidden'); app.startSession(${project.id}); })()">
                        <div class="task-select-item-body" style="gap: 2px;">
                            <div class="task-select-item-title" style="display: flex; align-items: center; gap: 6px;">
                                ${typeIcon}
                                ${this.escapeHtml(project.name)}
                            </div>
                            <div class="task-select-item-meta">
                                <span>${this.escapeHtml(pathDisplay)}</span>
                            </div>
                        </div>
                    </div>`;
            }).join('');
        });
    }

    showSessionNameModal(defaultName) {
        return new Promise((resolve) => {
            const modal = document.getElementById('session-name-modal');
            const input = document.getElementById('session-name-input');
            const startBtn = document.getElementById('session-name-start');
            const cancelBtn = document.getElementById('session-name-cancel');

            // Set default name
            input.value = defaultName;

            // Show modal (don't focus input to avoid mobile keyboard popup)
            modal.classList.remove('hidden');

            // Handle start
            const handleStart = () => {
                const name = input.value.trim() || defaultName;
                cleanup();
                resolve(name);
            };

            // Handle cancel
            const handleCancel = () => {
                cleanup();
                resolve(null);
            };

            // Handle enter key
            const handleKeydown = (e) => {
                if (e.key === 'Enter') {
                    handleStart();
                } else if (e.key === 'Escape') {
                    handleCancel();
                }
            };

            // Cleanup function
            const cleanup = () => {
                modal.classList.add('hidden');
                startBtn.removeEventListener('click', handleStart);
                cancelBtn.removeEventListener('click', handleCancel);
                input.removeEventListener('keydown', handleKeydown);
            };

            // Attach listeners
            startBtn.addEventListener('click', handleStart);
            cancelBtn.addEventListener('click', handleCancel);
            input.addEventListener('keydown', handleKeydown);
        });
    }

    /**
     * Shows task selection modal before starting a session.
     * @returns {Promise<{taskId,taskTitle}|{reconnect:string}|null|false>}
     *   - {taskId, taskTitle} = task selected
     *   - {reconnect: sessionId} = reconnect to active session
     *   - null = skip (no task)
     *   - false = cancelled
     */
    showTaskSelectModal(projectId) {
        return new Promise(async (resolve) => {
            const modal = document.getElementById('task-select-modal');
            const listEl = document.getElementById('task-select-list');
            const skipBtn = document.getElementById('task-select-skip');
            const cancelBtn = document.getElementById('task-select-cancel');

            let resolved = false;
            const finish = (value) => {
                if (resolved) return;
                resolved = true;
                cleanup();
                resolve(value);
            };

            const cleanup = () => {
                modal.classList.add('hidden');
                skipBtn.removeEventListener('click', handleSkip);
                cancelBtn.removeEventListener('click', handleCancel);
                document.removeEventListener('keydown', handleKeydown);
            };

            const handleSkip = () => finish(null);
            const handleCancel = () => finish(false);
            const handleKeydown = (e) => {
                if (e.key === 'Escape') finish(false);
            };

            // Show modal with loading state
            listEl.innerHTML = '<div class="task-select-loading"><div class="spinner"></div> Loading tasks...</div>';
            modal.classList.remove('hidden');

            skipBtn.addEventListener('click', handleSkip);
            cancelBtn.addEventListener('click', handleCancel);
            document.addEventListener('keydown', handleKeydown);

            try {
                const [allTasks, sessionSummaryRaw] = await Promise.all([
                    this.api('GET', `/projects/${projectId}/tasks`),
                    this.api('GET', `/projects/${projectId}/tasks/session-summary`).catch(() => [])
                ]);

                // Build session summary lookup
                const sessSummary = {};
                (sessionSummaryRaw || []).forEach(s => { sessSummary[s.task_id] = s; });

                // Filter to pending tasks (todo/in_progress), exclude subtasks
                const pendingTasks = (allTasks || []).filter(t =>
                    (t.status === 'todo' || t.status === 'in_progress') && !t.parent_id?.Valid
                );

                if (pendingTasks.length === 0) {
                    finish(null);
                    return;
                }

                // Render task items
                listEl.innerHTML = pendingTasks.map(task => {
                    const priorityClass = `priority-${task.priority || 'medium'}`;
                    const statusLabel = (task.status || 'todo').replace('_', ' ');
                    const descSnippet = task.description
                        ? this.escapeHtml(task.description.substring(0, 60)) + (task.description.length > 60 ? '...' : '')
                        : '';
                    const summary = sessSummary[task.id];
                    const hasActive = summary?.active_count > 0;
                    const hasStopped = !hasActive && summary?.stopped_count > 0;

                    let actionsHtml = '';
                    if (hasActive) {
                        actionsHtml = `
                            <div class="task-select-item-actions">
                                <button class="btn btn-sm btn-success task-select-reconnect" data-session-id="${this.escapeHtml(summary.latest_session)}" title="Reconnect to active session">Reconnect</button>
                                <button class="btn btn-sm btn-secondary task-select-new" data-task-id="${task.id}" data-task-title="${this.escapeHtml(task.title)}" title="Start new session">New</button>
                            </div>`;
                    } else if (hasStopped) {
                        actionsHtml = `
                            <div class="task-select-item-actions">
                                <button class="btn btn-sm btn-warning task-select-reopen" data-session-id="${this.escapeHtml(summary.latest_stopped_session)}" title="Reopen stopped session (continue conversation)">Reopen</button>
                                <button class="btn btn-sm btn-secondary task-select-new" data-task-id="${task.id}" data-task-title="${this.escapeHtml(task.title)}" title="Start fresh session">New</button>
                            </div>`;
                    }

                    return `
                        <div class="task-select-item" data-task-id="${task.id}" data-task-title="${this.escapeHtml(task.title)}" data-project-id="${task.project_id || projectId}" ${hasActive ? 'data-has-active="1"' : ''} ${hasStopped ? 'data-has-stopped="1"' : ''}>
                            <div class="task-priority-indicator ${priorityClass}"></div>
                            <div class="task-select-item-body">
                                <div class="task-select-item-title">${this.escapeHtml(task.title)}</div>
                                <div class="task-select-item-meta">
                                    <span class="task-status-badge badge-${task.status}">${statusLabel}</span>
                                    ${hasActive ? '<span class="badge badge-running">active session</span>' : ''}
                                    ${hasStopped ? '<span class="badge badge-stopped">previous session</span>' : ''}
                                    ${descSnippet ? `<span>${descSnippet}</span>` : ''}
                                </div>
                            </div>
                            <button class="btn-icon task-select-view" data-task-id="${task.id}" data-project-id="${task.project_id || projectId}" title="View task details">
                                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>
                            </button>
                            ${actionsHtml}
                        </div>`;
                }).join('');

                // Attach click handlers
                listEl.querySelectorAll('.task-select-item').forEach(item => {
                    const hasActive = item.dataset.hasActive === '1';
                    const hasStopped = item.dataset.hasStopped === '1';

                    if (!hasActive && !hasStopped) {
                        // No sessions at all: clicking the row selects the task for a new session
                        item.addEventListener('click', () => {
                            finish({ taskId: parseInt(item.dataset.taskId, 10), taskTitle: item.dataset.taskTitle });
                        });
                    }
                });

                // Reconnect buttons (active sessions)
                listEl.querySelectorAll('.task-select-reconnect').forEach(btn => {
                    btn.addEventListener('click', (e) => {
                        e.stopPropagation();
                        finish({ reconnect: btn.dataset.sessionId });
                    });
                });

                // Reopen buttons (stopped sessions)
                listEl.querySelectorAll('.task-select-reopen').forEach(btn => {
                    btn.addEventListener('click', (e) => {
                        e.stopPropagation();
                        finish({ reopen: btn.dataset.sessionId });
                    });
                });

                // New session buttons (for tasks with active or stopped sessions)
                listEl.querySelectorAll('.task-select-new').forEach(btn => {
                    btn.addEventListener('click', (e) => {
                        e.stopPropagation();
                        finish({ taskId: parseInt(btn.dataset.taskId, 10), taskTitle: btn.dataset.taskTitle });
                    });
                });

                // View task detail buttons
                listEl.querySelectorAll('.task-select-view').forEach(btn => {
                    btn.addEventListener('click', (e) => {
                        e.stopPropagation();
                        this.viewTaskDetail(parseInt(btn.dataset.projectId, 10), parseInt(btn.dataset.taskId, 10));
                    });
                });

            } catch (err) {
                console.warn('Failed to fetch tasks for task-select modal:', err);
                finish(null);
            }
        });
    }

    async _updateLinkTaskButton(sessionId) {
        const btn = document.getElementById('btn-link-task');
        if (!btn) return;

        if (!sessionId) {
            btn.style.display = 'none';
            return;
        }

        try {
            await this.api('GET', `/sessions/${sessionId}/task`);
            btn.style.display = 'none';
        } catch {
            btn.style.display = '';
        }
    }

    async showLinkTaskModal(sessionId) {
        const tabData = this.openTabs.get(sessionId);
        if (!tabData) return;

        const projectId = tabData.projectId;

        const content = `
            <div class="link-task-modal-content">
                <div class="link-task-tabs">
                    <button class="link-task-tab active" data-tab="existing">Select Existing</button>
                    <button class="link-task-tab" data-tab="create">Create New</button>
                </div>
                <div id="link-task-existing" class="link-task-panel">
                    <div id="link-task-list" class="task-select-list">
                        <div class="task-select-loading"><div class="spinner"></div> Loading tasks...</div>
                    </div>
                </div>
                <div id="link-task-create" class="link-task-panel" style="display:none;">
                    <form id="link-task-form">
                        <div style="text-align: right; margin-bottom: 12px;">
                            <button type="button" id="link-task-autofill" class="btn btn-sm btn-secondary" title="Auto-fill fields using AI session analysis">
                                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" style="margin-right: 4px; vertical-align: -2px;">
                                    <path d="M12 2L15.09 8.26L22 9.27L17 14.14L18.18 21.02L12 17.77L5.82 21.02L7 14.14L2 9.27L8.91 8.26L12 2Z"></path>
                                </svg>
                                Auto-fill with AI
                            </button>
                        </div>
                        <div class="form-group">
                            <label class="form-label">Title *</label>
                            <input type="text" class="form-input" name="title" required placeholder="Task title">
                        </div>
                        <div class="form-group">
                            <div class="hook-input-label-row">
                                <label class="form-label" style="margin-bottom:0">Description</label>
                                <button type="button" class="btn-icon btn-sm hook-voice-btn" id="link-task-desc-voice-btn" title="Voice input">
                                    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                                        <path d="M12 1a3 3 0 0 0-3 3v8a3 3 0 0 0 6 0V4a3 3 0 0 0-3-3z"></path>
                                        <path d="M19 10v2a7 7 0 0 1-14 0v-2"></path>
                                        <line x1="12" y1="19" x2="12" y2="23"></line>
                                        <line x1="8" y1="23" x2="16" y2="23"></line>
                                    </svg>
                                </button>
                            </div>
                            <textarea class="form-input" name="description" rows="3" placeholder="Optional description"></textarea>
                        </div>
                        <div class="form-group">
                            <label class="form-label">Priority</label>
                            <select class="form-input" name="priority">
                                <option value="low">Low</option>
                                <option value="medium" selected>Medium</option>
                                <option value="high">High</option>
                                <option value="urgent">Urgent</option>
                            </select>
                        </div>
                    </form>
                </div>
            </div>
        `;

        const actions = `
            <button class="btn btn-secondary" onclick="app.hideModal()">Cancel</button>
            <button id="link-task-submit" class="btn btn-primary" disabled>Link Task</button>
        `;

        this.showModal('Link Task to Session', content, actions);

        let selectedTaskId = null;
        let currentTab = 'existing';

        // Tab switching
        document.querySelectorAll('.link-task-tab').forEach(tab => {
            tab.addEventListener('click', () => {
                document.querySelectorAll('.link-task-tab').forEach(t => t.classList.remove('active'));
                tab.classList.add('active');
                currentTab = tab.dataset.tab;

                document.getElementById('link-task-existing').style.display = currentTab === 'existing' ? '' : 'none';
                document.getElementById('link-task-create').style.display = currentTab === 'create' ? '' : 'none';

                const submitBtn = document.getElementById('link-task-submit');
                if (currentTab === 'create') {
                    const titleInput = document.querySelector('#link-task-form input[name="title"]');
                    submitBtn.disabled = !titleInput?.value.trim();
                } else {
                    submitBtn.disabled = !selectedTaskId;
                }
            });
        });

        // Load tasks
        try {
            const tasks = await this.api('GET', `/projects/${projectId}/tasks`);
            const pendingTasks = (tasks || []).filter(t =>
                (t.status === 'todo' || t.status === 'in_progress') && !t.parent_id?.Valid
            );

            const listEl = document.getElementById('link-task-list');
            if (pendingTasks.length === 0) {
                listEl.innerHTML = '<p style="color: var(--color-text-secondary); text-align:center; padding: 20px;">No pending tasks. Switch to "Create New" tab.</p>';
            } else {
                listEl.innerHTML = pendingTasks.map(task => {
                    const priorityClass = `priority-${task.priority || 'medium'}`;
                    const statusLabel = (task.status || 'todo').replace('_', ' ');
                    return `
                        <div class="task-select-item link-task-item" data-task-id="${task.id}">
                            <div class="task-priority-indicator ${priorityClass}"></div>
                            <div class="task-select-item-body">
                                <div class="task-select-item-title">${this.escapeHtml(task.title)}</div>
                                <div class="task-select-item-meta">
                                    <span class="task-status-badge badge-${task.status}">${statusLabel}</span>
                                </div>
                            </div>
                        </div>`;
                }).join('');

                listEl.querySelectorAll('.link-task-item').forEach(item => {
                    item.addEventListener('click', () => {
                        listEl.querySelectorAll('.link-task-item').forEach(i => i.classList.remove('selected'));
                        item.classList.add('selected');
                        selectedTaskId = parseInt(item.dataset.taskId, 10);
                        document.getElementById('link-task-submit').disabled = false;
                    });
                });
            }
        } catch (err) {
            document.getElementById('link-task-list').innerHTML = '<p style="color: var(--color-danger);">Failed to load tasks.</p>';
        }

        // Enable submit for "create" tab based on title input
        const titleInput = document.querySelector('#link-task-form input[name="title"]');
        if (titleInput) {
            titleInput.addEventListener('input', () => {
                if (currentTab === 'create') {
                    document.getElementById('link-task-submit').disabled = !titleInput.value.trim();
                }
            });
        }

        // Auto-fill with AI button
        const autofillBtn = document.getElementById('link-task-autofill');
        const autofillOriginalHTML = autofillBtn?.innerHTML;
        if (autofillBtn) {
            autofillBtn.addEventListener('click', async () => {
                autofillBtn.disabled = true;
                autofillBtn.innerHTML = '<div class="spinner" style="width:14px;height:14px;border-width:2px;margin-right:4px;display:inline-block;vertical-align:-2px;"></div> Analyzing session...';

                try {
                    const result = await this.api('POST', `/sessions/${sessionId}/suggest-task-data`);
                    const form = document.getElementById('link-task-form');
                    if (form && result) {
                        if (result.title && titleInput) {
                            titleInput.value = result.title;
                            titleInput.dispatchEvent(new Event('input'));
                        }
                        const descInput = form.querySelector('[name="description"]');
                        if (result.description && descInput) descInput.value = result.description;
                        const prioritySelect = form.querySelector('[name="priority"]');
                        if (result.priority && prioritySelect) prioritySelect.value = result.priority;
                    }
                    autofillBtn.innerHTML = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" style="margin-right:4px;vertical-align:-2px;"><path d="M20 6L9 17l-5-5"></path></svg> Done!';
                    setTimeout(() => { autofillBtn.disabled = false; autofillBtn.innerHTML = autofillOriginalHTML; }, 2000);
                } catch (err) {
                    autofillBtn.disabled = false;
                    autofillBtn.innerHTML = autofillOriginalHTML;
                    this.showToast('Error', err.message || 'Failed to suggest task data', 'error');
                }
            });
        }

        // Voice input for Description
        const descVoiceBtn = document.getElementById('link-task-desc-voice-btn');
        const descFormInput = document.querySelector('#link-task-form textarea[name="description"]');
        if (descVoiceBtn && descFormInput && window.voiceInput) {
            descVoiceBtn.addEventListener('click', (e) => {
                e.preventDefault();
                e.stopPropagation();
                if (window.voiceInput.isRecording) {
                    window.voiceInput.stopRecording(false);
                    descVoiceBtn.classList.remove('recording');
                    return;
                }
                descVoiceBtn.classList.add('recording');
                window.voiceInput.startRecordingWithCallback((text) => {
                    descVoiceBtn.classList.remove('recording');
                    descFormInput.value = (descFormInput.value ? descFormInput.value + ' ' : '') + text;
                    descFormInput.focus();
                });
            });
        }

        // Submit handler
        document.getElementById('link-task-submit').addEventListener('click', async () => {
            const submitBtn = document.getElementById('link-task-submit');
            submitBtn.disabled = true;
            submitBtn.textContent = 'Linking...';

            try {
                let payload;
                if (currentTab === 'existing' && selectedTaskId) {
                    payload = { task_id: selectedTaskId };
                } else if (currentTab === 'create') {
                    const form = document.getElementById('link-task-form');
                    payload = {
                        task_data: {
                            title: form.querySelector('[name="title"]').value.trim(),
                            description: form.querySelector('[name="description"]').value.trim(),
                            priority: form.querySelector('[name="priority"]').value,
                        }
                    };
                }

                const result = await this.api('POST', `/sessions/${sessionId}/link-task`, payload);
                this.hideModal();
                this._updateLinkTaskButton(sessionId);
                this._showLinkTaskSuccess(result.task.title);
            } catch (error) {
                submitBtn.disabled = false;
                submitBtn.textContent = 'Link Task';
                this.showToast('Error', error.message, 'error');
            }
        });
    }

    _showLinkTaskSuccess(taskTitle) {
        const container = document.getElementById('toast-container');
        const toast = document.createElement('div');
        toast.className = 'toast toast-success';
        toast.innerHTML = `
            <span class="toast-message"><strong>Linked!</strong> Session linked to "${this.escapeHtml(taskTitle)}"</span>
        `;
        container.appendChild(toast);
        setTimeout(() => toast.remove(), 3000);
    }

    async syncProjectConfig(projectId) {
        this.showSyncModal(projectId);
        try {
            await this.api('POST', `/projects/${projectId}/sync-config`);
        } catch (error) {
            this.updateSyncStep('sync', 'error', error.message);
        }
    }

    showSyncModal(projectId) {
        this._syncProjectId = projectId;
        const steps = [
            { id: 'hooks', label: 'Hook Scripts' },
            { id: 'skills', label: 'Skills' },
            { id: 'mcps', label: 'MCP Servers' },
            { id: 'settings', label: 'Settings' },
            { id: 'memory_doc', label: 'Memory Doc' },
        ];

        const overlay = document.createElement('div');
        overlay.className = 'modal-overlay active';
        overlay.id = 'sync-modal-overlay';
        overlay.innerHTML = `
            <div class="modal sync-modal" style="max-width:400px;">
                <div class="modal-header">
                    <h3>Syncing Configuration</h3>
                </div>
                <div class="modal-body">
                    <div class="sync-modal-steps">
                        ${steps.map(s => `
                            <div class="sync-step" id="sync-step-${s.id}" data-status="pending">
                                <span class="sync-step-icon">
                                    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                                        <circle cx="12" cy="12" r="10"></circle>
                                    </svg>
                                </span>
                                <span class="sync-step-label">${s.label}</span>
                                <span class="sync-step-detail"></span>
                            </div>
                        `).join('')}
                    </div>
                </div>
                <div class="modal-footer">
                    <button class="btn btn-secondary btn-sm" id="sync-modal-close" style="display:none;" onclick="app.closeSyncModal()">Close</button>
                </div>
            </div>
        `;
        document.body.appendChild(overlay);
    }

    closeSyncModal() {
        const overlay = document.getElementById('sync-modal-overlay');
        if (overlay) overlay.remove();
        this._syncProjectId = null;
    }

    handleSyncProgress(data) {
        if (data.project_id !== this._syncProjectId) return;
        this.updateSyncStep(data.step, data.status, data.detail);
    }

    updateSyncStep(step, status, detail) {
        const stepEl = document.getElementById(`sync-step-${step}`);
        if (!stepEl) return;

        stepEl.dataset.status = status;
        const iconEl = stepEl.querySelector('.sync-step-icon');
        const detailEl = stepEl.querySelector('.sync-step-detail');

        if (detailEl) detailEl.textContent = detail || '';

        if (status === 'running') {
            iconEl.innerHTML = '<span class="sync-spinner"></span>';
        } else if (status === 'done') {
            iconEl.innerHTML = `<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="var(--color-success)" stroke-width="2.5"><path d="M20 6L9 17l-5-5"/></svg>`;
        } else if (status === 'error') {
            iconEl.innerHTML = `<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="var(--color-danger-light)" stroke-width="2.5"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>`;
        }

        // When sync finishes: auto-close on success, show close button on error
        const isFinalStep = step === 'memory_doc' || step === 'sync';
        if (status === 'done' && isFinalStep) {
            // Reload project data to update sync date in the UI
            this.loadProjects().then(() => {
                if (this._detailProject) {
                    this.showProjectDetail(this._detailProject.id);
                }
            });
            // Auto-close modal after a brief pause so the user sees completion
            setTimeout(() => this.closeSyncModal(), 800);
        }
        if (status === 'error') {
            const closeBtn = document.getElementById('sync-modal-close');
            if (closeBtn) closeBtn.style.display = '';
        }
    }

    editProject(projectId) {
        const project = this.projects.find(p => p.id === projectId);
        if (project) this.showProjectModal(project);
    }

    // Project Detail
    showProjectDetail(projectId) {
        const project = this.projects.find(p => p.id === projectId);
        if (!project) return;

        this._detailProject = project;

        // Update header
        const titleEl = document.getElementById('project-detail-title');
        const badgeEl = document.getElementById('project-detail-badge');
        if (titleEl) titleEl.textContent = project.name;
        if (badgeEl) {
            badgeEl.textContent = project.type;
            badgeEl.className = `badge badge-${project.type}`;
        }

        // Render detail content
        const container = document.getElementById('project-detail-content');
        if (!container) return;

        const syncDate = project.config_synced_at?.Valid ? this.formatTime(project.config_synced_at.Time) : 'Never synced';
        const createdDate = project.created_at ? this.formatTime(project.created_at) : '—';
        const updatedDate = project.updated_at ? this.formatTime(project.updated_at) : '—';

        let html = `
            <div class="project-detail-card">
                <div class="project-detail-section">
                    <div class="project-detail-section-title">General</div>
                    <div class="project-detail-field">
                        <span class="project-detail-label">Name</span>
                        <span class="project-detail-value">${this.escapeHtml(project.name)}</span>
                    </div>
                    <div class="project-detail-field">
                        <span class="project-detail-label">Path</span>
                        <span class="project-detail-value">${this.escapeHtml(project.path)}</span>
                    </div>
                    <div class="project-detail-field">
                        <span class="project-detail-label">Type</span>
                        <span class="project-detail-value"><span class="badge badge-${project.type}">${project.type}</span></span>
                    </div>
                </div>
        `;

        if (project.type === 'remote') {
            html += `
                <div class="project-detail-section">
                    <div class="project-detail-section-title">SSH</div>
                    <div class="project-detail-field">
                        <span class="project-detail-label">Host</span>
                        <span class="project-detail-value">${this.escapeHtml(project.ssh_host?.String || '—')}</span>
                    </div>
                    <div class="project-detail-field">
                        <span class="project-detail-label">Port</span>
                        <span class="project-detail-value">${project.ssh_port?.Int64 || 22}</span>
                    </div>
                    <div class="project-detail-field">
                        <span class="project-detail-label">User</span>
                        <span class="project-detail-value">${this.escapeHtml(project.ssh_user?.String || '—')}</span>
                    </div>
                    <div class="project-detail-field">
                        <span class="project-detail-label">Auth Type</span>
                        <span class="project-detail-value">${this.escapeHtml(project.ssh_auth_type?.String || '—')}</span>
                    </div>
                </div>
            `;
        }

        html += `
                <div class="project-detail-section">
                    <div class="project-detail-section-title">Status</div>
                    <div class="project-detail-field">
                        <span class="project-detail-label">Config Sync</span>
                        <span class="project-detail-value">${syncDate}</span>
                    </div>
                    <div class="project-detail-field">
                        <span class="project-detail-label">Created</span>
                        <span class="project-detail-value">${createdDate}</span>
                    </div>
                    <div class="project-detail-field">
                        <span class="project-detail-label">Updated</span>
                        <span class="project-detail-value">${updatedDate}</span>
                    </div>
                </div>

            </div>

            <div class="project-detail-card">
                <div class="project-detail-section">
                    <div class="project-detail-section-title" style="display:flex;align-items:center;justify-content:space-between;">
                        <span>Session Tools</span>
                        <span id="project-tools-summary" style="font-size: 11px; color: var(--color-text-muted);"></span>
                    </div>
                    <div id="project-tools-list" class="project-tools-list">
                        <div class="meta-empty">Loading...</div>
                    </div>
                </div>
            </div>

            <div class="project-detail-card">
                <div class="project-detail-section">
                    <div class="project-detail-section-title" style="display:flex;align-items:center;justify-content:space-between;">
                        <span>Memory Doc</span>
                        <button class="meta-ai-btn" onclick="app.editMemoryDocWithAI(${project.id})" title="Edit with AI">
                            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                                <path d="M12 2L2 7l10 5 10-5-10-5z"/>
                                <path d="M2 17l10 5 10-5"/>
                                <path d="M2 12l10 5 10-5"/>
                            </svg>
                            Edit with AI
                        </button>
                    </div>
                    <div id="memory-doc-content" class="memory-doc-content">
                        <div class="meta-empty">Loading...</div>
                    </div>
                </div>
            </div>

            <div class="project-detail-card">
                <div class="project-detail-section">
                    <div class="project-tasks-header">
                        <div class="project-detail-section-title">Tasks</div>
                        <div class="project-tasks-summary" id="project-tasks-summary"></div>
                    </div>
                    <div id="project-tasks-list" class="project-tasks-list">
                        <div class="task-empty">Loading...</div>
                    </div>
                    <button class="btn-add-task" onclick="app.showTaskModal(${project.id})" style="margin-top: 8px;">+ Add Task</button>
                </div>
            </div>

            <div class="project-detail-card">
                <div class="project-detail-section">
                    <div class="project-detail-section-title">Token Usage</div>
                    <div class="token-usage-period-selector">
                        <button class="token-usage-period-btn" data-days="7" onclick="app.loadProjectTokenUsage(${project.id}, 7)">7d</button>
                        <button class="token-usage-period-btn active" data-days="30" onclick="app.loadProjectTokenUsage(${project.id}, 30)">30d</button>
                        <button class="token-usage-period-btn" data-days="90" onclick="app.loadProjectTokenUsage(${project.id}, 90)">90d</button>
                    </div>
                    <div id="project-token-usage">
                        <div class="meta-empty">Loading...</div>
                    </div>
                </div>
            </div>
        `;

        container.innerHTML = html;

        // Load memory doc, tasks, tools, and token usage
        this.loadMemoryDoc(project.id);
        this.loadProjectTools(project.id);
        this.loadProjectTasks(project.id);
        this.loadProjectTokenUsage(project.id, 30);

        // Render actions in the fixed bottom bar
        const actionsBar = document.getElementById('project-detail-actions');
        if (actionsBar) {
            actionsBar.innerHTML = `
                <button class="btn btn-primary btn-sm" onclick="app.startSession(${project.id})">
                    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                        <polygon points="5 3 19 12 5 21 5 3"></polygon>
                    </svg>
                    Start Session
                </button>
                <button class="btn btn-secondary btn-sm" onclick="app.editProject(${project.id})">
                    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                        <path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"></path>
                        <path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"></path>
                    </svg>
                    Edit
                </button>
                <button class="btn btn-secondary btn-sm" onclick="app.syncProjectConfig(${project.id})">
                    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                        <polyline points="23 4 23 10 17 10"></polyline>
                        <polyline points="1 20 1 14 7 14"></polyline>
                        <path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"></path>
                    </svg>
                    Sync Config
                </button>
                <button class="btn btn-secondary btn-sm" onclick="app.duplicateProject(${project.id})">
                    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                        <rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect>
                        <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path>
                    </svg>
                    Duplicate
                </button>
                <button class="btn btn-danger btn-sm" onclick="app.deleteProject(${project.id})">
                    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                        <polyline points="3 6 5 6 21 6"></polyline>
                        <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path>
                    </svg>
                    Delete
                </button>
            `;
        }
        if (this.currentView !== 'project-detail') {
            this.showView('project-detail');
        }
    }

    async loadMemoryDoc(projectId) {
        const container = document.getElementById('memory-doc-content');
        if (!container) return;
        try {
            const meta = await this.api('GET', `/projects/${projectId}/memory-doc`);
            if (meta.exists && meta.content) {
                let rendered = meta.content;
                if (typeof marked !== 'undefined') {
                    try { rendered = marked.parse(meta.content); } catch (e) { /* fallback to raw */ }
                }
                container.innerHTML = `<div class="meta-rendered">${rendered}</div>
                    <div class="meta-info">v${meta.version} &middot; Updated by ${this.escapeHtml(meta.last_updated_by || '?')}</div>`;
            } else {
                container.innerHTML = '<div class="meta-empty">No memory doc yet. Sync the project to load its CLAUDE.md.</div>';
            }
        } catch (e) {
            container.innerHTML = '<div class="meta-empty">Failed to load memory doc.</div>';
        }
    }

    async loadProjectTools(projectId) {
        const container = document.getElementById('project-tools-list');
        const summaryEl = document.getElementById('project-tools-summary');
        if (!container) return;

        try {
            const tools = await this.api('GET', `/projects/${projectId}/tools`);
            const enabled = tools.filter(t => t.enabled);
            const project = this.projects.find(p => p.id === projectId);
            const isInheriting = !project?.tool_policy;

            if (summaryEl) {
                if (isInheriting) {
                    summaryEl.textContent = `Inheriting global (${enabled.length}/${tools.length})`;
                } else if (enabled.length === 0) {
                    summaryEl.textContent = 'No tools enabled (custom)';
                } else if (enabled.length === tools.length) {
                    summaryEl.textContent = `All ${tools.length} tools enabled (custom)`;
                } else {
                    summaryEl.textContent = `${enabled.length}/${tools.length} tools enabled (custom)`;
                }
            }

            const inheritChecked = isInheriting ? 'checked' : '';
            container.innerHTML = `
                <div style="margin-bottom: 8px; padding: 6px 8px; background: var(--color-bg-tertiary); border-radius: 6px; display: flex; align-items: center; gap: 8px;">
                    <label style="display: flex; align-items: center; gap: 6px; cursor: pointer; font-size: 12px;">
                        <input type="checkbox" id="project-tools-inherit" ${inheritChecked} onchange="app._projectInheritChanged(${projectId})">
                        <span>Inherit from global policy</span>
                    </label>
                </div>
                <div id="project-tools-custom" style="${isInheriting ? 'opacity:0.5;pointer-events:none;' : ''}">
                    <div style="margin-bottom: 6px;">
                        <label class="tp-tool-item" style="font-weight:500; cursor:pointer;">
                            <input type="checkbox" id="project-tools-toggle-all" ${enabled.length === tools.length ? 'checked' : ''} onchange="app.projectDetailSelectAll(${projectId}, this.checked)">
                            <span class="tp-tool-name" style="min-width:auto;">Toggle all</span>
                        </label>
                    </div>
                    <div class="project-tools-grid">
                        ${tools.map(t => {
                            const shortDesc = t.description.length > 80 ? t.description.slice(0, 80) + '...' : t.description;
                            return `<label class="tp-tool-item" title="${t.description.replace(/"/g, '&quot;')}">
                                <input type="checkbox" data-tool="${t.name}" ${t.enabled ? 'checked' : ''} onchange="app._projectToolChanged(${projectId})">
                                <span class="tp-tool-name">${t.name}</span>
                                <span class="tp-tool-desc">${shortDesc}</span>
                            </label>`;
                        }).join('')}
                    </div>
                </div>
                <div class="project-tools-actions" id="project-tools-actions" style="display:none;">
                    <button class="btn btn-sm btn-primary" onclick="app.saveProjectToolsFromDetail(${projectId})">Save</button>
                    <button class="btn btn-sm btn-secondary" onclick="app.loadProjectTools(${projectId})">Cancel</button>
                    <span style="font-size: 11px; color: var(--color-text-muted);" id="project-tools-dirty">Unsaved changes</span>
                </div>
            `;
            // Hide actions bar initially
            const actions = document.getElementById('project-tools-actions');
            if (actions) actions.style.display = 'none';
        } catch (e) {
            container.innerHTML = '<div class="meta-empty">Failed to load tools.</div>';
        }
    }

    _projectInheritChanged(projectId) {
        const inherit = document.getElementById('project-tools-inherit')?.checked;
        const customDiv = document.getElementById('project-tools-custom');
        if (customDiv) {
            customDiv.style.opacity = inherit ? '0.5' : '1';
            customDiv.style.pointerEvents = inherit ? 'none' : '';
        }
        this._projectToolChanged(projectId);
    }

    projectDetailSelectAll(projectId, checked) {
        document.querySelectorAll('#project-tools-list input[data-tool]').forEach(cb => cb.checked = checked);
        this._projectToolChanged(projectId);
    }

    _projectToolChanged(projectId) {
        const actions = document.getElementById('project-tools-actions');
        if (actions) actions.style.display = 'flex';
    }

    async saveProjectToolsFromDetail(projectId) {
        const inherit = document.getElementById('project-tools-inherit')?.checked;

        let policy;
        if (inherit) {
            // Inherit from global — clear project override
            policy = '';
        } else {
            // Custom project policy
            const checkboxes = document.querySelectorAll('#project-tools-list input[data-tool]');
            const all = Array.from(checkboxes);
            const checked = all.filter(cb => cb.checked);

            if (checked.length === all.length) {
                policy = JSON.stringify({ mode: 'allow_all' });
            } else if (checked.length === 0) {
                policy = JSON.stringify({ mode: 'deny_all' });
            } else if (checked.length <= all.length / 2) {
                const allowed = checked.map(cb => cb.dataset.tool);
                policy = JSON.stringify({ mode: 'deny_all', allowed });
            } else {
                const unchecked = all.filter(cb => !cb.checked);
                const denied = unchecked.map(cb => cb.dataset.tool);
                policy = JSON.stringify({ mode: 'allow_all', denied });
            }
        }

        try {
            await this.api('PUT', `/projects/${projectId}/tool-policy`, { tool_policy: policy });
            // Refresh projects list to pick up change
            await this.loadProjects();
            this.showToast('Success', 'Tool policy updated', 'success');
            // Reload to reflect saved state
            this.loadProjectTools(projectId);
        } catch (e) {
            this.showToast('Error', e.message, 'error');
        }
    }

    async loadProjectTokenUsage(projectId, days = 30) {
        const container = document.getElementById('project-token-usage');
        if (!container) return;

        // Update period selector active state
        document.querySelectorAll('.token-usage-period-btn').forEach(btn => {
            btn.classList.toggle('active', parseInt(btn.dataset.days) === days);
        });

        try {
            const data = await this.api('GET', `/token-usage/summary?project_id=${projectId}&days=${days}`);
            const s = data.summary;

            if (!s || (s.total_requests === 0)) {
                container.innerHTML = '<div class="meta-empty">No token usage data for this period.</div>';
                return;
            }

            const totalTokens = (s.total_input_tokens || 0) + (s.total_output_tokens || 0);

            let html = `
                <div class="token-usage-grid">
                    <div class="token-usage-stat">
                        <div class="label">Total Tokens</div>
                        <div class="value">${totalTokens.toLocaleString()}</div>
                    </div>
                    <div class="token-usage-stat">
                        <div class="label">Custo Estimado</div>
                        <div class="value cost">$${(s.total_cost_usd || 0).toFixed(4)}</div>
                    </div>
                    <div class="token-usage-stat">
                        <div class="label">Input Tokens</div>
                        <div class="value">${(s.total_input_tokens || 0).toLocaleString()}</div>
                    </div>
                    <div class="token-usage-stat">
                        <div class="label">Output Tokens</div>
                        <div class="value">${(s.total_output_tokens || 0).toLocaleString()}</div>
                    </div>
                </div>`;

            // By Model table
            if (data.by_model && data.by_model.length > 0) {
                html += `<div class="token-usage-table-wrap"><table class="token-usage-model-table">
                    <thead><tr><th>Model</th><th>Input</th><th>Output</th><th>Custo</th><th>Reqs</th></tr></thead>
                    <tbody>`;
                for (const m of data.by_model) {
                    const shortModel = (m.model || 'unknown').replace(/^claude-/, '').replace(/-\d{8}$/, '');
                    html += `<tr>
                        <td>${this.escapeHtml(shortModel)}</td>
                        <td>${(m.total_input_tokens || 0).toLocaleString()}</td>
                        <td>${(m.total_output_tokens || 0).toLocaleString()}</td>
                        <td>$${(m.total_cost_usd || 0).toFixed(4)}</td>
                        <td>${m.total_requests || 0}</td>
                    </tr>`;
                }
                html += '</tbody></table></div>';
            }

            // Daily bars (last N days)
            if (data.daily && data.daily.length > 0) {
                const maxTokens = Math.max(...data.daily.map(d => (d.total_input_tokens || 0) + (d.total_output_tokens || 0)));
                html += '<div style="margin-top:12px">';
                for (const d of data.daily.slice(0, 7)) {
                    const dayTokens = (d.total_input_tokens || 0) + (d.total_output_tokens || 0);
                    const pct = maxTokens > 0 ? (dayTokens / maxTokens * 100) : 0;
                    const shortDate = d.date ? d.date.slice(5) : '?';
                    html += `<div class="token-usage-bar-row">
                        <span class="bar-label">${shortDate}</span>
                        <div class="bar-track"><div class="bar-fill" style="width:${pct}%"></div></div>
                        <span class="bar-value">${dayTokens.toLocaleString()}</span>
                    </div>`;
                }
                html += '</div>';
            }

            container.innerHTML = html;
        } catch (e) {
            container.innerHTML = '<div class="meta-empty">Failed to load token usage data.</div>';
        }
    }

    async loadGlobalTokenUsage(days = 30) {
        const container = document.getElementById('token-usage-global-content');
        if (!container) return;

        container.innerHTML = '<div class="meta-empty">Loading...</div>';

        try {
            const data = await this.api('GET', `/token-usage/summary?days=${days}`);
            const s = data.summary;

            if (!s || s.total_requests === 0) {
                container.innerHTML = `
                    <div class="token-usage-period-selector" style="padding:0 16px;margin-top:12px">
                        <button class="token-usage-period-btn${days===7?' active':''}" onclick="app.loadGlobalTokenUsage(7)">7d</button>
                        <button class="token-usage-period-btn${days===30?' active':''}" onclick="app.loadGlobalTokenUsage(30)">30d</button>
                        <button class="token-usage-period-btn${days===90?' active':''}" onclick="app.loadGlobalTokenUsage(90)">90d</button>
                    </div>
                    <div class="meta-empty">No token usage data for this period.</div>`;
                return;
            }

            const totalTokens = (s.total_input_tokens || 0) + (s.total_output_tokens || 0);

            let html = `<div style="padding:0 16px">
                <div class="token-usage-period-selector" style="margin-top:12px">
                    <button class="token-usage-period-btn${days===7?' active':''}" onclick="app.loadGlobalTokenUsage(7)">7d</button>
                    <button class="token-usage-period-btn${days===30?' active':''}" onclick="app.loadGlobalTokenUsage(30)">30d</button>
                    <button class="token-usage-period-btn${days===90?' active':''}" onclick="app.loadGlobalTokenUsage(90)">90d</button>
                </div>

                <div class="token-usage-grid">
                    <div class="token-usage-stat">
                        <div class="label">Total Tokens</div>
                        <div class="value">${totalTokens.toLocaleString()}</div>
                    </div>
                    <div class="token-usage-stat">
                        <div class="label">Custo Estimado</div>
                        <div class="value cost">$${(s.total_cost_usd || 0).toFixed(4)}</div>
                    </div>
                    <div class="token-usage-stat">
                        <div class="label">Input Tokens</div>
                        <div class="value">${(s.total_input_tokens || 0).toLocaleString()}</div>
                    </div>
                    <div class="token-usage-stat">
                        <div class="label">Output Tokens</div>
                        <div class="value">${(s.total_output_tokens || 0).toLocaleString()}</div>
                    </div>
                </div>`;

            // By Model (one row per model, aggregated across all sources)
            if (data.by_model && data.by_model.length > 0) {
                html += '<h3 style="font-size:14px;margin:16px 0 8px;color:var(--color-text-secondary)">By Model</h3>';
                html += `<div class="token-usage-table-wrap"><table class="token-usage-model-table">
                    <thead><tr><th>Model</th><th>Input</th><th>Output</th><th>Custo</th><th>Reqs</th></tr></thead>
                    <tbody>`;
                for (const m of data.by_model) {
                    const shortModel = (m.model || 'unknown').replace(/^claude-/, '').replace(/-\d{8}$/, '');
                    html += `<tr>
                        <td>${this.escapeHtml(shortModel)}</td>
                        <td>${(m.total_input_tokens || 0).toLocaleString()}</td>
                        <td>${(m.total_output_tokens || 0).toLocaleString()}</td>
                        <td>$${(m.total_cost_usd || 0).toFixed(4)}</td>
                        <td>${m.total_requests || 0}</td>
                    </tr>`;
                }
                html += '</tbody></table></div>';
            }

            // AI Assistant section
            if (data.by_ai_subcategory && data.by_ai_subcategory.length > 0) {
                const subcatLabels = {
                    'chat': 'Chat',
                    'skill_generate': 'Gerar Skill',
                    'skill_validate': 'Validar Skill',
                    'meta_eval': 'Avaliar Meta (legado)',
                    'session_eval': 'Avaliar Sessão',
                    '': 'Outro',
                };
                // Calculate AI totals
                let aiInput = 0, aiOutput = 0, aiCost = 0, aiReqs = 0;
                for (const s of data.by_ai_subcategory) {
                    aiInput += s.total_input_tokens || 0;
                    aiOutput += s.total_output_tokens || 0;
                    aiCost += s.total_cost_usd || 0;
                    aiReqs += s.total_requests || 0;
                }
                html += `<h3 style="font-size:14px;margin:16px 0 8px;color:var(--color-text-secondary)">AI Assistant</h3>`;
                html += `<div class="token-project-card">
                    <div class="token-project-header">
                        <span class="token-project-name">AI Assistant</span>
                        <span class="token-project-cost">$${aiCost.toFixed(4)}</span>
                    </div>
                    <div class="token-project-stats">
                        <span>${(aiInput + aiOutput).toLocaleString()} tokens</span>
                        <span>${aiReqs} reqs</span>
                        <span>In: ${aiInput.toLocaleString()}</span>
                        <span>Out: ${aiOutput.toLocaleString()}</span>
                    </div>`;
                html += `<div class="token-usage-table-wrap"><table class="token-usage-model-table token-project-model-table">
                    <thead><tr><th>Tipo</th><th>Model</th><th>Input</th><th>Output</th><th>Custo</th><th>Reqs</th></tr></thead>
                    <tbody>`;
                for (const s of data.by_ai_subcategory) {
                    const shortModel = (s.model || 'unknown').replace(/^claude-/, '').replace(/-\d{8}$/, '');
                    const label = subcatLabels[s.subcategory] || s.subcategory || 'Outro';
                    html += `<tr>
                        <td>${this.escapeHtml(label)}</td>
                        <td>${this.escapeHtml(shortModel)}</td>
                        <td>${(s.total_input_tokens || 0).toLocaleString()}</td>
                        <td>${(s.total_output_tokens || 0).toLocaleString()}</td>
                        <td>$${(s.total_cost_usd || 0).toFixed(4)}</td>
                        <td>${s.total_requests || 0}</td>
                    </tr>`;
                }
                html += '</tbody></table></div></div>';
            }

            // By Project (with inline model breakdown)
            if (data.by_project && data.by_project.length > 0) {
                const projectModels = {};
                if (data.by_project_model) {
                    for (const pm of data.by_project_model) {
                        if (!projectModels[pm.project_id]) projectModels[pm.project_id] = [];
                        projectModels[pm.project_id].push(pm);
                    }
                }

                html += '<h3 style="font-size:14px;margin:16px 0 8px;color:var(--color-text-secondary)">By Project</h3>';

                for (const p of data.by_project) {
                    const totalTokens = (p.total_input_tokens || 0) + (p.total_output_tokens || 0);
                    html += `<div class="token-project-card">
                        <div class="token-project-header">
                            <span class="token-project-name">${this.escapeHtml(p.project_name)}</span>
                            <span class="token-project-cost">$${(p.total_cost_usd || 0).toFixed(4)}</span>
                        </div>
                        <div class="token-project-stats">
                            <span>${totalTokens.toLocaleString()} tokens</span>
                            <span>${(p.total_requests || 0)} reqs</span>
                            <span>In: ${(p.total_input_tokens || 0).toLocaleString()}</span>
                            <span>Out: ${(p.total_output_tokens || 0).toLocaleString()}</span>
                        </div>`;

                    const models = projectModels[p.project_id];
                    if (models && models.length > 0) {
                        html += `<div class="token-usage-table-wrap"><table class="token-usage-model-table token-project-model-table">
                            <thead><tr><th>Model</th><th>Input</th><th>Output</th><th>Custo</th><th>Reqs</th></tr></thead>
                            <tbody>`;
                        for (const m of models) {
                            const shortModel = (m.model || 'unknown').replace(/^claude-/, '').replace(/-\d{8}$/, '');
                            html += `<tr>
                                <td>${this.escapeHtml(shortModel)}</td>
                                <td>${(m.total_input_tokens || 0).toLocaleString()}</td>
                                <td>${(m.total_output_tokens || 0).toLocaleString()}</td>
                                <td>$${(m.total_cost_usd || 0).toFixed(4)}</td>
                                <td>${m.total_requests || 0}</td>
                            </tr>`;
                        }
                        html += '</tbody></table></div>';
                    }

                    html += '</div>';
                }
            }

            // Daily bars
            if (data.daily && data.daily.length > 0) {
                html += '<h3 style="font-size:14px;margin:16px 0 8px;color:var(--color-text-secondary)">Daily Usage</h3>';
                const maxTokens = Math.max(...data.daily.map(d => (d.total_input_tokens || 0) + (d.total_output_tokens || 0)));
                for (const d of data.daily.slice(0, 14)) {
                    const dayTokens = (d.total_input_tokens || 0) + (d.total_output_tokens || 0);
                    const pct = maxTokens > 0 ? (dayTokens / maxTokens * 100) : 0;
                    const shortDate = d.date ? d.date.slice(5) : '?';
                    html += `<div class="token-usage-bar-row">
                        <span class="bar-label">${shortDate}</span>
                        <div class="bar-track"><div class="bar-fill" style="width:${pct}%"></div></div>
                        <span class="bar-value">${dayTokens.toLocaleString()}</span>
                    </div>`;
                }
            }

            html += '</div>';
            container.innerHTML = html;
        } catch (e) {
            container.innerHTML = '<div class="meta-empty">Failed to load token usage data.</div>';
        }
    }

    clearTokenUsage() {
        showConfirmModal(
            'Limpar dados de tokens?',
            'Todos os registros de uso de tokens serão removidos permanentemente. Esta ação não pode ser desfeita.',
            async () => {
                try {
                    const result = await this.api('DELETE', '/token-usage');
                    this.showToast(`${result.deleted} registros removidos`, 'success');
                    this.loadGlobalTokenUsage(30);
                } catch (e) {
                    this.showToast('Erro ao limpar dados: ' + e.message, 'error');
                }
            },
            'Limpar'
        );
    }

    async editMemoryDocWithAI(projectId) {
        const project = this.projects.find(p => p.id === projectId) || this._detailProject;
        if (!project || !window.aiChat) return;
        try {
            const result = await this.api('POST', '/ai/initiate-memory-doc-edit', { project_id: projectId });
            if (result?.conversation_id) {
                window.aiChat.open();
                window.aiChat.loadConversation(result.conversation_id);
            }
        } catch (e) {
            this.showToast('Erro ao iniciar edicao do memory doc', 'error');
        }
    }

    duplicateProject(projectId) {
        const project = this.projects.find(p => p.id === projectId) || this._detailProject;
        if (!project) return;

        const defaultName = project.name + ' (Copy)';

        let content = `
            <form id="duplicate-project-form">
                <div class="form-group">
                    <label class="form-label">Name</label>
                    <input type="text" class="form-input" name="name" value="${this.escapeHtml(defaultName)}" required>
                </div>
                <div class="form-group">
                    <label class="form-label">Path</label>
                    <input type="text" class="form-input" name="path" value="${this.escapeHtml(project.path)}" required>
                </div>
                <div class="form-group">
                    <label class="form-label">Type</label>
                    <select class="form-select" name="type" onchange="app.toggleSSHFields(this.value)">
                        <option value="local" ${project.type === 'local' ? 'selected' : ''}>Local</option>
                        <option value="remote" ${project.type === 'remote' ? 'selected' : ''}>Remote (SSH)</option>
                    </select>
                </div>
                <div id="ssh-fields" class="${project.type !== 'remote' ? 'hidden' : ''}">
                    <div class="form-group">
                        <label class="form-label">SSH Host</label>
                        <input type="text" class="form-input" name="ssh_host" value="${this.escapeHtml(project.ssh_host?.String || '')}">
                    </div>
                    <div class="form-group">
                        <label class="form-label">SSH Port</label>
                        <input type="number" class="form-input" name="ssh_port" value="${project.ssh_port?.Int64 || 22}">
                    </div>
                    <div class="form-group">
                        <label class="form-label">SSH User</label>
                        <input type="text" class="form-input" name="ssh_user" value="${this.escapeHtml(project.ssh_user?.String || '')}">
                    </div>
                    <div class="form-group">
                        <label class="form-label">Auth Type</label>
                        <select class="form-select" name="ssh_auth_type">
                            <option value="password" ${project.ssh_auth_type?.String === 'password' ? 'selected' : ''}>Password</option>
                            <option value="key" ${project.ssh_auth_type?.String === 'key' ? 'selected' : ''}>Private Key</option>
                        </select>
                    </div>
                    <div class="form-group">
                        <label class="form-label">Credential (Password or Key)</label>
                        <textarea class="form-textarea" name="ssh_credential" placeholder="Leave empty to keep the same credential from the original project"></textarea>
                    </div>
                </div>
            </form>
        `;

        const actions = `
            <button class="btn btn-secondary" onclick="app.hideModal()">Cancel</button>
            <button class="btn btn-primary" onclick="app.executeDuplicate(${project.id})">Duplicate</button>
        `;

        this.showModal('Duplicate Project', content, actions);
    }

    async executeDuplicate(projectId) {
        const form = document.getElementById('duplicate-project-form');
        const formData = new FormData(form);
        const data = {
            name: formData.get('name'),
            path: formData.get('path'),
            type: formData.get('type'),
            ssh_host: formData.get('ssh_host') || '',
            ssh_port: parseInt(formData.get('ssh_port')) || 0,
            ssh_user: formData.get('ssh_user') || '',
            ssh_auth_type: formData.get('ssh_auth_type') || '',
            ssh_credential: formData.get('ssh_credential') || ''
        };

        try {
            await this.api('POST', `/projects/${projectId}/duplicate`, data);
            this.hideModal();
            this.showToast('Success', 'Project duplicated', 'success');
            this.loadProjects();
            this.showView('projects');
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    // Sessions
    async loadSessions() {
        try {
            const [sessions, activeDetails] = await Promise.all([
                this.api('GET', '/sessions'),
                this.api('GET', '/sessions/active-details')
            ]);
            this.sessions = sessions;
            this.activeSessionDetails = activeDetails || [];
            this.renderSessions();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    renderSessions() {
        const container = document.getElementById('sessions-list');
        if (!container) return;

        const activeSessions = this.activeSessionDetails || [];

        // Sync tab sidebar (uses full sessions list)
        const activeFromSessions = this.sessions.filter(s => s.status === 'running' || s.status === 'starting');
        this.syncSessionTabs(activeFromSessions);

        if (activeSessions.length === 0) {
            container.innerHTML = `
                <div class="empty-state">
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                        <polyline points="4 17 10 11 4 5"></polyline>
                        <line x1="12" y1="19" x2="20" y2="19"></line>
                    </svg>
                    <h3>No active sessions</h3>
                    <p>Click "New Session" to start one</p>
                </div>
            `;
            return;
        }

        container.innerHTML = activeSessions.map(session => {
            const displayName = session.name || session.project_name || 'Unknown';
            const projectLabel = session.project_name || 'Unknown';
            const hostLabel = session.project_type === 'remote' && session.project_ssh_host
                ? ` @ ${this.escapeHtml(session.project_ssh_host)}` : '';
            const elapsed = this.formatElapsed(session.start_time);
            const totalTokens = (session.total_input_tokens || 0) + (session.total_output_tokens || 0);
            const tokenLabel = totalTokens > 0 ? this.formatTokens(totalTokens) : '--';
            const costLabel = session.total_cost > 0 ? `$${session.total_cost.toFixed(3)}` : '';
            const lastActivity = session.last_activity_at?.Valid
                ? this.formatTimeAgo(session.last_activity_at.Time) : '';
            const taskLabel = session.task_title
                ? `<span class="session-card-task" title="${this.escapeHtml(session.task_title)}">${this.escapeHtml(this.truncateStr(session.task_title, 40))}</span>`
                : '';
            const pendingBadge = session.has_pending_permission
                ? '<span class="badge badge-pending-perm" title="Awaiting input">!</span>' : '';
            const mode = session.execution_mode || 'idle';
            const modeLabels = { plan_mode: 'Plan', executing: 'Exec', idle: 'Idle' };
            const modeLabel = modeLabels[mode] || mode;

            return `
                <div class="session-card" onclick="app.openTerminal('${session.id}')">
                    <div class="session-card-header">
                        <span class="badge badge-mode-${mode}" data-session-mode="${session.id}">${modeLabel}</span>
                        ${pendingBadge}
                        <span class="session-card-name">${this.escapeHtml(displayName)}</span>
                        <button class="btn btn-danger btn-sm session-card-stop" onclick="event.stopPropagation(); app.stopSession('${session.id}')">
                            Stop
                        </button>
                    </div>
                    <div class="session-card-meta">
                        <span class="session-card-project">${this.escapeHtml(projectLabel)}${hostLabel}</span>
                        ${taskLabel}
                    </div>
                    <div class="session-card-stats">
                        <span class="session-card-stat" title="Elapsed time">
                            <svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>
                            <span class="session-elapsed" data-start="${session.start_time}">${elapsed}</span>
                        </span>
                        <span class="session-card-stat" title="Tokens used">
                            <svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 2L2 7l10 5 10-5-10-5z"/><path d="M2 17l10 5 10-5"/><path d="M2 12l10 5 10-5"/></svg>
                            ${tokenLabel}${costLabel ? ` (${costLabel})` : ''}
                        </span>
                        ${lastActivity ? `<span class="session-card-stat" title="Last activity">${lastActivity}</span>` : ''}
                    </div>
                </div>
            `;
        }).join('');

        // Attach tooltip to truncated session names
        container.querySelectorAll('.session-card-name').forEach(nameEl => {
            this._attachSessionNameTooltip(nameEl, nameEl.textContent);
        });
    }

    async openTerminal(sessionId, sessionData = null, customName = null) {
        console.log('openTerminal called with:', sessionId);

        // Set pending session flag to prevent race conditions with restoreTabsFromStorage
        this.pendingSessionOpen = { sessionId, timestamp: Date.now() };

        // Dismiss any active tooltip/popup before switching views
        this._hideSessionTooltip();
        this._hideSessionPopup();

        // Find session data (use provided data or look it up)
        let session = sessionData || this.sessions.find(s => s.id === sessionId);
        if (!session) {
            console.error('Session not found:', sessionId);
            this.pendingSessionOpen = null;
            return;
        }

        // Add to sessions array if not already there
        if (!this.sessions.find(s => s.id === sessionId)) {
            this.sessions.push(session);
        }

        // Show terminal view if not already visible
        if (!document.querySelector('.terminal-view.active')) {
            document.querySelectorAll('.view').forEach(v => v.classList.remove('active'));
            document.getElementById('view-terminal').classList.add('active');
            this.currentView = 'terminal';
            this._updateNavForTerminal();
        }

        // Refresh sessions to sync tabs (this will create tab if needed)
        await this.loadSessions();

        // If custom name provided, update it
        if (customName) {
            const tabData = this.openTabs.get(sessionId);
            if (tabData) {
                tabData.sessionName = customName;
                // Update tab name in UI (preserve task subtitle)
                const tab = document.querySelector(`.terminal-tab[data-session-id="${sessionId}"]`);
                if (tab) {
                    const nameContainer = tab.querySelector('.terminal-tab-name');
                    if (nameContainer) {
                        const nameText = nameContainer.querySelector('.terminal-tab-name-text');
                        if (nameText) {
                            nameText.textContent = customName;
                        } else {
                            nameContainer.textContent = customName;
                        }
                        // Update tooltip with full task title or custom name
                        const taskSpan = nameContainer.querySelector('.terminal-tab-task');
                        nameContainer.dataset.fullName = taskSpan ? taskSpan.textContent : customName;
                    }
                }
            }
        }

        // Connect terminal for this session
        const tabData = this.openTabs.get(sessionId);
        if (tabData) {
            const tabName = tabData.sessionName;
            window.terminalManager.connect(sessionId, tabName);
            tabData.connected = true;
        }

        // Update active tab UI
        this.updateTabActiveState(sessionId);
        this.currentSession = sessionId;

        // Update tools badge for this session
        this._updateSessionToolsBadge(sessionId);
        this._updateLinkTaskButton(sessionId);

        // Close tools panel when switching sessions
        const toolsPanel = document.getElementById('session-tools-panel');
        if (toolsPanel) toolsPanel.style.display = 'none';

        // Save tabs to storage
        this.saveTabsToStorage();

        // Clear pending session flag and ensure this session remains active
        this.pendingSessionOpen = null;

        // Check if the session tab still exists (it may have been removed by
        // syncSessionTabs if the session died immediately after creation)
        if (!this.openTabs.has(sessionId)) {
            console.error(`[openTerminal] Session ${sessionId} died before terminal could connect`);
            this.showToast('Session ended unexpectedly. Check server logs for details.', 'error');
            // Navigate back to sessions view
            this.navigateTo('sessions');
            return;
        }

        // Double-check that the requested session is actually active
        // (protect against race conditions from restoreTabsFromStorage)
        if (window.terminalManager.activeSessionId !== sessionId) {
            console.warn(`[openTerminal] Active session mismatch! Requested: ${sessionId}, Active: ${window.terminalManager.activeSessionId}. Forcing switch...`);
            window.terminalManager.switchToSession(sessionId);
            this.currentSession = sessionId;
            this.updateTabActiveState(sessionId);
        }
    }

    async stopSession(sessionId) {
        console.log('stopSession called with:', sessionId);
        try {
            await this.api('DELETE', `/sessions/${sessionId}`);
            this.showToast('Success', 'Session stopped', 'success');

            // Remove tab without switching to another session
            const tabElement = document.querySelector(`.terminal-tab[data-session-id="${sessionId}"]`);
            if (tabElement) tabElement.remove();
            this.openTabs.delete(sessionId);
            window.terminalManager.disconnect(sessionId);
            this.saveTabsToStorage();

            // Always show empty state after stopping a session
            this._showSessionsEmptyState();

            await this.loadSessions();
        } catch (error) {
            console.error('stopSession error:', error);
            this.showToast('Error', error.message, 'error');
        }
    }

    // ==================== SESSION TOOLS ====================

    async toggleSessionToolsPanel() {
        const panel = document.getElementById('session-tools-panel');
        if (!panel) return;
        const visible = panel.style.display !== 'none';
        if (visible) {
            panel.style.display = 'none';
            return;
        }
        panel.style.display = '';
        this.loadSessionTools();
    }

    async loadSessionTools() {
        const list = document.getElementById('session-tools-panel-list');
        const summary = document.getElementById('session-tools-panel-summary');
        const countEl = document.getElementById('session-tools-count');
        if (!list) return;

        const sessionId = this.currentSession;
        if (!sessionId) {
            list.innerHTML = '<div class="meta-empty">No session selected</div>';
            return;
        }

        list.innerHTML = '<div class="meta-empty">Loading...</div>';

        try {
            const tools = await this.api('GET', `/sessions/${sessionId}/tools`);
            const enabled = tools.filter(t => t.enabled);

            if (summary) {
                summary.textContent = `${enabled.length}/${tools.length} enabled`;
            }
            if (countEl) {
                countEl.textContent = enabled.length;
                countEl.style.display = enabled.length > 0 ? '' : 'none';
            }

            if (tools.length === 0) {
                list.innerHTML = '<div class="meta-empty">No tools configured</div>';
                return;
            }

            list.innerHTML = tools.map(t => {
                const shortDesc = t.description.length > 70 ? t.description.slice(0, 70) + '...' : t.description;
                return `<div class="session-tool-item ${t.enabled ? '' : 'disabled'}" title="${t.description.replace(/"/g, '&quot;')}">
                    <span class="session-tool-status">${t.enabled ? '&#9679;' : '&#9675;'}</span>
                    <span class="tp-tool-name">${t.name}</span>
                    <span class="tp-tool-desc">${shortDesc}</span>
                </div>`;
            }).join('');
        } catch (e) {
            list.innerHTML = '<div class="meta-empty">Failed to load tools</div>';
        }
    }

    // Update session tools count badge when switching sessions
    async _updateSessionToolsBadge(sessionId) {
        const countEl = document.getElementById('session-tools-count');
        if (!countEl) return;
        try {
            const tools = await this.api('GET', `/sessions/${sessionId}/tools`);
            const enabled = tools.filter(t => t.enabled);
            countEl.textContent = enabled.length;
            countEl.style.display = enabled.length > 0 ? '' : 'none';
        } catch (e) {
            countEl.style.display = 'none';
        }
    }

    // ==================== TAB MANAGEMENT ====================

    createTabElement(sessionId, sessionName, session) {
        const tabsList = document.getElementById('terminal-tabs-list');
        if (!tabsList) return;

        const projectId = session.project_id;

        // Handle tab grouping
        let groupElement = this.tabGroups.get(projectId);

        if (this.groupingEnabled && !groupElement) {
            // Create new group
            groupElement = document.createElement('div');
            groupElement.className = 'terminal-tab-group';
            groupElement.dataset.projectId = projectId;

            const groupHeader = document.createElement('div');
            groupHeader.className = 'terminal-tab-group-header';

            const groupToggle = document.createElement('span');
            groupToggle.className = 'terminal-tab-group-toggle';
            groupToggle.textContent = '▼';

            const groupName = document.createElement('span');
            groupName.className = 'terminal-tab-group-name';
            groupName.textContent = this.getProjectName(projectId);

            groupHeader.appendChild(groupToggle);
            groupHeader.appendChild(groupName);

            const groupContent = document.createElement('div');
            groupContent.className = 'terminal-tab-group-content';

            groupElement.appendChild(groupHeader);
            groupElement.appendChild(groupContent);
            tabsList.appendChild(groupElement);

            this.tabGroups.set(projectId, groupElement);

            // Toggle group collapse
            groupHeader.addEventListener('click', () => {
                groupElement.classList.toggle('collapsed');
                groupToggle.textContent = groupElement.classList.contains('collapsed') ? '▶' : '▼';
            });
        }

        // Create tab element
        const tabDiv = document.createElement('div');
        tabDiv.className = 'terminal-tab';
        tabDiv.dataset.sessionId = sessionId;
        tabDiv.draggable = true;

        // Add status indicator
        const statusDot = document.createElement('span');
        statusDot.className = 'terminal-tab-status';
        statusDot.dataset.status = 'running';
        statusDot.title = 'Running';

        const nameContainer = document.createElement('div');
        nameContainer.className = 'terminal-tab-name';

        const nameText = document.createElement('span');
        nameText.className = 'terminal-tab-name-text';
        nameText.textContent = sessionName;
        nameContainer.appendChild(nameText);

        // Task title subtitle (desktop)
        const taskTitle = session.task_title || '';
        if (taskTitle) {
            const taskSpan = document.createElement('span');
            taskSpan.className = 'terminal-tab-task';
            taskSpan.textContent = taskTitle;
            nameContainer.appendChild(taskSpan);
        }

        // Tooltip: show full task title on hover
        const tooltipText = taskTitle || sessionName;
        this._attachSessionNameTooltip(nameContainer, tooltipText);

        // Close button
        const closeImg = document.createElement('img');
        closeImg.src = '/static/icons/close.svg';
        closeImg.className = 'terminal-tab-close';
        closeImg.alt = 'Close';

        tabDiv.appendChild(statusDot);
        tabDiv.appendChild(nameContainer);
        tabDiv.appendChild(closeImg);

        // Tab click - connect terminal if needed, then switch to session
        tabDiv.addEventListener('click', (e) => {
            if (e.target === closeImg) return; // Don't switch if clicking close

            // Check if terminal is connected for this session
            const tabData = this.openTabs.get(sessionId);
            if (tabData && !tabData.connected) {
                // First time clicking this tab - connect the terminal
                const session = this.sessions.find(s => s.id === sessionId);
                if (session) {
                    const tabName = tabData.sessionName;
                    window.terminalManager.connect(sessionId, tabName);
                    tabData.connected = true;
                }
            }

            if (this.splitScreenMode) {
                // In split mode, clicking a tab replaces the second pane
                if (this.splitScreenSessions.length >= 2) {
                    this.splitScreenSessions[1] = sessionId;
                } else {
                    this.splitScreenSessions.push(sessionId);
                }
                this.refreshSplitScreen();
            } else {
                if (window.terminalManager.hasSession(sessionId)) {
                    window.terminalManager.switchToSession(sessionId);
                }
            }
            this.currentSession = sessionId;
            this.updateTabActiveState(sessionId);
        });

        // Name span doesn't need special handlers anymore - clicking the tab switches sessions

        // Close button click
        closeImg.addEventListener('click', (e) => {
            e.stopPropagation();
            this.closeTab(sessionId);
        });

        // Drag-and-drop handlers
        this.setupDragHandlers(tabDiv, sessionId);

        // Add to group or directly to list
        if (this.groupingEnabled && groupElement) {
            const groupContent = groupElement.querySelector('.terminal-tab-group-content');
            groupContent.appendChild(tabDiv);
        } else {
            tabsList.appendChild(tabDiv);
        }

        return tabDiv;
    }

    setupDragHandlers(tabDiv, sessionId) {
        // Drag start - store the dragged session ID
        tabDiv.addEventListener('dragstart', (e) => {
            e.dataTransfer.effectAllowed = 'move';
            e.dataTransfer.setData('text/plain', sessionId);
            tabDiv.classList.add('dragging');
        });

        // Drag end - cleanup
        tabDiv.addEventListener('dragend', (e) => {
            tabDiv.classList.remove('dragging');
        });

        // Drag over - allow drop
        tabDiv.addEventListener('dragover', (e) => {
            e.preventDefault();
            e.dataTransfer.dropEffect = 'move';

            // Add visual indicator
            const draggingTab = document.querySelector('.terminal-tab.dragging');
            if (draggingTab && draggingTab !== tabDiv) {
                const rect = tabDiv.getBoundingClientRect();
                const midpoint = rect.top + rect.height / 2;

                if (e.clientY < midpoint) {
                    tabDiv.classList.add('drop-above');
                    tabDiv.classList.remove('drop-below');
                } else {
                    tabDiv.classList.add('drop-below');
                    tabDiv.classList.remove('drop-above');
                }
            }
        });

        // Drag leave - remove visual indicator
        tabDiv.addEventListener('dragleave', (e) => {
            tabDiv.classList.remove('drop-above', 'drop-below');
        });

        // Drop - reorder tabs
        tabDiv.addEventListener('drop', (e) => {
            e.preventDefault();
            tabDiv.classList.remove('drop-above', 'drop-below');

            const draggedSessionId = e.dataTransfer.getData('text/plain');
            if (draggedSessionId === sessionId) return;

            const draggedTab = document.querySelector(`.terminal-tab[data-session-id="${draggedSessionId}"]`);
            const targetTab = tabDiv;
            const parent = targetTab.parentElement;

            // Determine drop position
            const rect = targetTab.getBoundingClientRect();
            const midpoint = rect.top + rect.height / 2;

            if (e.clientY < midpoint) {
                // Insert before target
                parent.insertBefore(draggedTab, targetTab);
            } else {
                // Insert after target
                parent.insertBefore(draggedTab, targetTab.nextSibling);
            }

            // Save tab order to localStorage
            this.saveTabOrder();
        });
    }

    closeTab(sessionId) {
        console.log('Closing tab:', sessionId);

        // Remove tab UI
        const tabElement = document.querySelector(`.terminal-tab[data-session-id="${sessionId}"]`);
        if (tabElement) {
            tabElement.remove();
        }

        // Remove from open tabs
        this.openTabs.delete(sessionId);

        // Disconnect terminal
        window.terminalManager.disconnect(sessionId);

        // Save tabs to storage
        this.saveTabsToStorage();

        // Update mobile dropdown
        if (this.openTabs.size > 0) {
            const nextSessionId = Array.from(this.openTabs.keys())[0];
            this.updateMobileSessionTrigger(nextSessionId);
        }

        // If no tabs left, show empty sessions state
        if (this.openTabs.size === 0) {
            this._showSessionsEmptyState();
        }
    }

    syncSessionTabs(activeSessions) {
        // This function ensures the tab sidebar shows ALL running sessions
        // Create tabs for sessions that don't have tabs yet
        activeSessions.forEach(session => {
            // Skip if tab already exists
            const existingTab = document.querySelector(`.terminal-tab[data-session-id="${session.id}"]`);
            if (existingTab) return;

            // Create tab for this session
            const project = this.projects.find(p => p.id === session.project_id);
            const projectName = project?.name || 'Unknown';
            const tabName = session.name || `${projectName} (${new Date(session.start_time).toLocaleTimeString()})`;

            // Add to tracking (but terminal not connected yet)
            if (!this.openTabs.has(session.id)) {
                this.openTabs.set(session.id, {
                    projectName,
                    sessionName: tabName,
                    projectId: session.project_id,
                    connected: false // Track if terminal is connected
                });
            }

            // Create tab UI element
            this.createTabElement(session.id, tabName, session);
        });

        // Remove tabs for sessions that are no longer running
        let tabsRemoved = false;
        document.querySelectorAll('.terminal-tab').forEach(tab => {
            const sessionId = tab.dataset.sessionId;
            const sessionExists = activeSessions.find(s => s.id === sessionId);
            if (!sessionExists) {
                // Session ended - close the tab
                const termData = this.openTabs.get(sessionId);
                if (termData && termData.connected) {
                    // Disconnect terminal if it was connected
                    window.terminalManager.disconnect(sessionId);
                }
                this.openTabs.delete(sessionId);
                tab.remove();
                tabsRemoved = true;
            }
        });

        // If tabs were removed, show empty sessions state
        if (tabsRemoved) {
            this._showSessionsEmptyState();
        }
    }

    updateTabActiveState(activeSessionId) {
        document.querySelectorAll('.terminal-tab').forEach(tab => {
            if (tab.dataset.sessionId === activeSessionId) {
                tab.classList.add('active');
            } else {
                tab.classList.remove('active');
            }
        });

        // Update mobile dropdown trigger
        this.updateMobileSessionTrigger(activeSessionId);

        // Update mobile dropdown items
        document.querySelectorAll('.mobile-session-item').forEach(item => {
            if (item.dataset.sessionId === activeSessionId) {
                item.classList.add('active');
            } else {
                item.classList.remove('active');
            }
        });
    }

    updateTabStatus(sessionId, status) {
        const tab = document.querySelector(`.terminal-tab[data-session-id="${sessionId}"]`);
        if (!tab) return;

        const statusDot = tab.querySelector('.terminal-tab-status');
        if (statusDot) {
            statusDot.dataset.status = status;

            // Update tooltip
            const statusText = {
                'running': 'Running',
                'disconnected': 'Disconnected',
                'error': 'Error',
                'stopped': 'Stopped',
                'connecting': 'Connecting'
            };
            statusDot.title = statusText[status] || status;
        }

        // Update mobile dropdown trigger if this is the active session
        if (sessionId === window.terminalManager?.activeSessionId) {
            const mobileStatus = document.querySelector('.mobile-session-status');
            if (mobileStatus) {
                mobileStatus.dataset.status = status;
            }
        }

        // Update mobile dropdown item
        const mobileItem = document.querySelector(`.mobile-session-item[data-session-id="${sessionId}"]`);
        if (mobileItem) {
            const mobileStatusDot = mobileItem.querySelector('.mobile-session-item-status');
            if (mobileStatusDot) {
                mobileStatusDot.dataset.status = status;
            }
        }
    }

    getProjectName(projectId) {
        const project = this.projects.find(p => p.id === projectId);
        return project ? project.name : 'Unknown';
    }

    // ==================== STATUS LISTENER ====================

    setupStatusListener() {
        // Listen for session status changes from terminal manager
        window.addEventListener('session-status-changed', (e) => {
            const { sessionId, status } = e.detail;
            this.updateTabStatus(sessionId, status);

            // When session ends, close its tab and navigate away
            if (status === 'completed' || status === 'stopped' || status === 'error' || status === 'disconnected') {
                setTimeout(() => {
                    if (this.openTabs.has(sessionId)) {
                        this.closeTab(sessionId);
                        this.loadSessions();
                    }
                }, 1500);
            }
        });
    }

    // ==================== MOBILE SESSION DROPDOWN ====================

    setupMobileSessionDropdown() {
        const trigger = document.getElementById('mobile-session-trigger');
        const menu = document.getElementById('mobile-session-menu');
        const closeBtn = document.getElementById('mobile-session-close');

        if (!trigger || !menu) return;

        // Toggle dropdown
        trigger.addEventListener('click', (e) => {
            e.stopPropagation();
            const isOpen = !menu.classList.contains('hidden');

            if (isOpen) {
                this.closeMobileSessionMenu();
            } else {
                this.openMobileSessionMenu();
            }
        });

        // Close button
        closeBtn?.addEventListener('click', (e) => {
            e.stopPropagation();
            this.closeMobileSessionMenu();
        });

        // Close on outside click
        document.addEventListener('click', (e) => {
            if (!menu.classList.contains('hidden') &&
                !trigger.contains(e.target) &&
                !menu.contains(e.target)) {
                this.closeMobileSessionMenu();
            }
        });

        // Close on escape key
        document.addEventListener('keydown', (e) => {
            if (e.key === 'Escape' && !menu.classList.contains('hidden')) {
                this.closeMobileSessionMenu();
            }
        });
    }

    openMobileSessionMenu() {
        const trigger = document.getElementById('mobile-session-trigger');
        const menu = document.getElementById('mobile-session-menu');

        menu.classList.remove('hidden');
        trigger.classList.add('open');

        // Populate menu with current sessions
        this.renderMobileSessionList();
    }

    closeMobileSessionMenu() {
        const trigger = document.getElementById('mobile-session-trigger');
        const menu = document.getElementById('mobile-session-menu');

        menu.classList.add('hidden');
        trigger.classList.remove('open');
    }

    renderMobileSessionList() {
        const listContainer = document.getElementById('mobile-session-list');
        if (!listContainer) return;

        listContainer.innerHTML = '';

        if (this.openTabs.size === 0) {
            listContainer.innerHTML = '<div class="empty-state">No active sessions</div>';
            return;
        }

        // Group sessions by project
        const sessionsByProject = new Map();

        this.openTabs.forEach((tabData, sessionId) => {
            const projectId = tabData.projectId || 'default';
            if (!sessionsByProject.has(projectId)) {
                sessionsByProject.set(projectId, []);
            }
            sessionsByProject.get(projectId).push({ sessionId, ...tabData });
        });

        // Render groups
        sessionsByProject.forEach((sessions, projectId) => {
            const projectName = this.getProjectName(projectId);

            if (this.groupingEnabled && sessionsByProject.size > 1) {
                // Create group
                const groupDiv = document.createElement('div');
                groupDiv.className = 'mobile-session-group';

                const groupHeader = document.createElement('div');
                groupHeader.className = 'mobile-session-group-header';

                const groupToggle = document.createElement('span');
                groupToggle.className = 'mobile-session-group-toggle';
                groupToggle.textContent = '▼';

                const groupName = document.createElement('span');
                groupName.className = 'mobile-session-group-name';
                groupName.textContent = projectName;

                groupHeader.appendChild(groupToggle);
                groupHeader.appendChild(groupName);

                const groupContent = document.createElement('div');
                groupContent.className = 'mobile-session-group-content';

                // Toggle collapse
                groupHeader.addEventListener('click', (e) => {
                    e.stopPropagation();
                    groupDiv.classList.toggle('collapsed');
                    groupToggle.textContent = groupDiv.classList.contains('collapsed') ? '▶' : '▼';
                });

                // Add sessions to group
                sessions.forEach(session => {
                    const item = this.createMobileSessionItem(session.sessionId, session);
                    groupContent.appendChild(item);
                });

                groupDiv.appendChild(groupHeader);
                groupDiv.appendChild(groupContent);
                listContainer.appendChild(groupDiv);
            } else {
                // Flat list
                sessions.forEach(session => {
                    const item = this.createMobileSessionItem(session.sessionId, session);
                    listContainer.appendChild(item);
                });
            }
        });
    }

    createMobileSessionItem(sessionId, sessionData) {
        const itemDiv = document.createElement('div');
        itemDiv.className = 'mobile-session-item';
        itemDiv.dataset.sessionId = sessionId;

        // Active state
        if (sessionId === window.terminalManager?.activeSessionId) {
            itemDiv.classList.add('active');
        }

        // Status indicator
        const statusDot = document.createElement('span');
        statusDot.className = 'mobile-session-item-status';

        // Get current status from tab
        const tab = document.querySelector(`.terminal-tab[data-session-id="${sessionId}"]`);
        const currentStatus = tab?.querySelector('.terminal-tab-status')?.dataset.status || 'running';
        statusDot.dataset.status = currentStatus;

        // Session name
        const nameSpan = document.createElement('span');
        nameSpan.className = 'mobile-session-item-name';
        nameSpan.textContent = sessionData.sessionName;

        // Close button
        const closeBtn = document.createElement('div');
        closeBtn.className = 'mobile-session-item-close';
        closeBtn.innerHTML = '<img src="/static/icons/close.svg" width="16" height="16" alt="Close" />';

        // Click to switch session
        itemDiv.addEventListener('click', (e) => {
            if (e.target.closest('.mobile-session-item-close')) {
                return; // Don't switch if clicking close
            }

            // Switch to session
            const tabData = this.openTabs.get(sessionId);
            if (tabData && !tabData.connected) {
                // Connect terminal if not connected
                const session = this.sessions.find(s => s.id === sessionId);
                if (session) {
                    window.terminalManager.connect(sessionId, tabData.sessionName);
                    tabData.connected = true;
                }
            }

            if (window.terminalManager.hasSession(sessionId)) {
                window.terminalManager.switchToSession(sessionId);
            }

            this.updateTabActiveState(sessionId);
            this.updateMobileSessionTrigger(sessionId);
            this.closeMobileSessionMenu();
        });

        // Close button handler
        closeBtn.addEventListener('click', (e) => {
            e.stopPropagation();
            this.closeTab(sessionId);
            this.renderMobileSessionList();
        });

        itemDiv.appendChild(statusDot);
        itemDiv.appendChild(nameSpan);
        itemDiv.appendChild(closeBtn);

        return itemDiv;
    }

    updateMobileSessionTrigger(sessionId) {
        const trigger = document.getElementById('mobile-session-name');
        const statusDot = document.querySelector('.mobile-session-status');

        if (!trigger || !statusDot) return;

        const tabData = this.openTabs.get(sessionId);
        if (tabData) {
            trigger.textContent = tabData.sessionName;

            // Update status
            const tab = document.querySelector(`.terminal-tab[data-session-id="${sessionId}"]`);
            const currentStatus = tab?.querySelector('.terminal-tab-status')?.dataset.status || 'running';
            statusDot.dataset.status = currentStatus;
        }
    }

    getProjectName(projectId) {
        const project = this.projects.find(p => p.id === projectId);
        return project?.name || 'Unknown Project';
    }

    // ==================== MOBILE TERMINAL INPUT ====================

    setupMobileTerminalInput() {
        const input = document.getElementById('mobile-terminal-input');
        const sendBtn = document.getElementById('mobile-terminal-send');

        if (!input || !sendBtn) return;

        // Send input on Enter key
        input.addEventListener('keydown', (e) => {
            if (e.key === 'Enter') {
                e.preventDefault();
                this.sendMobileTerminalInput();
            }
        });

        // Send input on button click
        sendBtn.addEventListener('click', () => {
            this.sendMobileTerminalInput();
        });

        // Expand button opens full-screen editor
        const expandBtn = document.getElementById('btn-mobile-expand-editor');
        expandBtn?.addEventListener('click', () => {
            this.openMobileEditor();
        });

        // Setup mobile editor buttons
        this.setupMobileEditor();

        // Setup mobile special keys bar
        this.setupMobileSpecialKeys();

        // Setup quick commands popup
        this.setupQuickCommands();
    }

    setupMobileEditor() {
        const closeBtn = document.getElementById('mobile-editor-close');
        const sendBtn = document.getElementById('mobile-editor-send');
        const voiceBtn = document.getElementById('mobile-editor-voice');

        closeBtn?.addEventListener('click', () => {
            this.closeMobileEditor();
        });

        sendBtn?.addEventListener('click', () => {
            const textarea = document.getElementById('mobile-editor-textarea');
            const input = document.getElementById('mobile-terminal-input');
            if (!textarea) return;

            const text = textarea.value.trim();
            if (text) {
                // Copy to inline input and use the canonical send method
                if (input) input.value = text;
                this.sendMobileTerminalInput();
            }

            // Clear and close
            textarea.value = '';
            if (input) input.value = '';
            const editor = document.getElementById('mobile-text-editor');
            if (editor) editor.classList.add('hidden');
        });

        voiceBtn?.addEventListener('click', () => {
            if (window.voiceInput) {
                const textarea = document.getElementById('mobile-editor-textarea');
                window.voiceInput.startRecordingWithCallback((text) => {
                    if (textarea) {
                        const current = textarea.value;
                        textarea.value = current.length > 0 ? current + ' ' + text : text;
                    }
                });
            }
        });
    }

    openMobileEditor() {
        const editor = document.getElementById('mobile-text-editor');
        const textarea = document.getElementById('mobile-editor-textarea');
        const input = document.getElementById('mobile-terminal-input');

        if (!editor || !textarea || !input) return;

        // Copy text from input to textarea
        textarea.value = input.value;
        editor.classList.remove('hidden');

        // Focus textarea and move cursor to end
        setTimeout(() => {
            textarea.focus();
            textarea.selectionStart = textarea.value.length;
            textarea.selectionEnd = textarea.value.length;
        }, 50);
    }

    closeMobileEditor() {
        const editor = document.getElementById('mobile-text-editor');
        const textarea = document.getElementById('mobile-editor-textarea');
        const input = document.getElementById('mobile-terminal-input');

        if (!editor || !textarea || !input) return;

        // Preserve text back into inline input
        input.value = textarea.value;
        editor.classList.add('hidden');
    }

    setupMobileSpecialKeys() {
        const keysBar = document.getElementById('mobile-terminal-keys-bar');
        if (!keysBar) return;

        const keyMap = {
            'shift-tab': '\x1b[Z',
            'esc': '\x1b',
            'up': '\x1b[A',
            'down': '\x1b[B',
            'left': '\x1b[D',
            'right': '\x1b[C',
            'ctrl-c': '\x03',
        };

        keysBar.addEventListener('click', (e) => {
            const btn = e.target.closest('.mobile-key-btn');
            if (!btn) return;

            const key = btn.dataset.key;
            const sequence = keyMap[key];
            if (sequence && window.terminalManager) {
                const tm = window.terminalManager;
                const termData = tm.activeSessionId ? tm.terminals.get(tm.activeSessionId) : null;
                const savedViewportY = termData?.terminal?.buffer?.active?.viewportY ?? 0;

                tm.sendInput(sequence);

                // Claude Code's first mode switch emits ~28 \r\n that push content
                // into scrollback. Detect viewport displacement and restore position.
                if (termData?.terminal) {
                    const term = termData.terminal;
                    const disp = term.onWriteParsed(() => {
                        const currentY = term.buffer.active.viewportY;
                        if (currentY > savedViewportY) {
                            term.scrollLines(-(currentY - savedViewportY));
                        }
                    });
                    setTimeout(() => disp.dispose(), 2000);
                }
            }
        });
    }

    setupQuickCommands() {
        const btn = document.getElementById('btn-quick-commands');
        const popup = document.getElementById('quick-commands-popup');
        const closeBtn = document.getElementById('quick-commands-close');
        if (!btn || !popup) return;

        // Toggle popup on button click
        btn.addEventListener('click', () => {
            popup.classList.toggle('hidden');
        });

        // Close popup
        closeBtn?.addEventListener('click', () => {
            popup.classList.add('hidden');
        });

        // Handle command clicks
        popup.addEventListener('click', (e) => {
            const item = e.target.closest('.quick-cmd-item');
            if (!item) return;

            const cmd = item.dataset.cmd;
            if (cmd) {
                this.sendQuickCommand(cmd);
                popup.classList.add('hidden');
            }
        });

        // Close popup when tapping outside
        document.addEventListener('click', (e) => {
            if (!popup.classList.contains('hidden') &&
                !popup.contains(e.target) &&
                !btn.contains(e.target)) {
                popup.classList.add('hidden');
            }
        });
    }

    sendQuickCommand(command) {
        if (!window.terminalManager) return;

        // Use the canonical three-step mobile terminal input sequence
        window.terminalManager.sendInput('\x15'); // Ctrl+U - clear current line
        window.terminalManager.sendInput(command);
        setTimeout(() => {
            window.terminalManager.sendInput('\r'); // Enter after 700ms
        }, 700);
    }

    sendMobileTerminalInput() {
        const input = document.getElementById('mobile-terminal-input');
        if (!input) return;

        const text = input.value.trim();
        if (text) {
            if (window.terminalManager) {
                // Clear current line, send text, then Enter after delay.
                // This is the exact sequence that works with voice Enviar.
                window.terminalManager.sendInput('\x15'); // Ctrl+U
                window.terminalManager.sendInput(text);
                setTimeout(() => {
                    window.terminalManager.sendInput('\r');
                }, 700);
            }
            input.value = '';
        } else {
            // Empty input: send Enter to the terminal (for navigation, confirming prompts, etc.)
            if (window.terminalManager) {
                window.terminalManager.sendInput('\r');
            }
        }
    }

    // ==================== SPLIT-SCREEN MODE ====================

    toggleSplitScreen() {
        this.splitScreenMode = !this.splitScreenMode;
        const wrapper = document.getElementById('terminal-containers-wrapper');

        if (this.splitScreenMode) {
            wrapper.classList.add('split-mode');

            // Show two most recent tabs side-by-side
            const openSessions = Array.from(this.openTabs.keys());
            this.splitScreenSessions = openSessions.slice(0, 2);

            this.refreshSplitScreen();
        } else {
            wrapper.classList.remove('split-mode');
            this.splitScreenSessions = [];

            // Restore single active terminal
            const containers = wrapper.querySelectorAll('.terminal-container');
            containers.forEach(container => {
                container.classList.remove('split-visible', 'split-0', 'split-1');

                if (container.id === `terminal-container-${window.terminalManager.activeSessionId}`) {
                    container.classList.add('active');
                }
            });

            // Re-fit active terminal
            setTimeout(() => {
                const activeSessionId = window.terminalManager.activeSessionId;
                if (activeSessionId) {
                    const termData = window.terminalManager.terminals.get(activeSessionId);
                    if (termData) {
                        termData.fitAddon.fit();
                    }
                }
            }, 0);
        }
    }

    refreshSplitScreen() {
        const wrapper = document.getElementById('terminal-containers-wrapper');
        const containers = wrapper.querySelectorAll('.terminal-container');

        containers.forEach(container => {
            const sessionId = container.id.replace('terminal-container-', '');

            if (this.splitScreenSessions.includes(sessionId)) {
                const index = this.splitScreenSessions.indexOf(sessionId);
                container.classList.add('split-visible', `split-${index}`);
                container.classList.remove('active');
            } else {
                container.classList.remove('split-visible', 'split-0', 'split-1', 'active');
            }
        });

        // Re-fit all visible terminals
        setTimeout(() => {
            this.splitScreenSessions.forEach(sessionId => {
                const termData = window.terminalManager.terminals.get(sessionId);
                if (termData) {
                    termData.fitAddon.fit();
                }
            });
        }, 0);
    }

    // ==================== LOCALSTORAGE PERSISTENCE ====================

    saveTabOrder() {
        const tabs = Array.from(document.querySelectorAll('.terminal-tab'));
        const order = tabs.map(tab => tab.dataset.sessionId);
        localStorage.setItem('devmanager-tab-order', JSON.stringify(order));
    }

    restoreTabOrder() {
        const orderJson = localStorage.getItem('devmanager-tab-order');
        if (!orderJson) return;

        try {
            const order = JSON.parse(orderJson);
            const tabsList = document.getElementById('terminal-tabs-list');

            order.forEach(sessionId => {
                const tab = document.querySelector(`.terminal-tab[data-session-id="${sessionId}"]`);
                if (tab) {
                    tabsList.appendChild(tab);
                }
            });
        } catch (e) {
            console.error('Failed to restore tab order:', e);
        }
    }

    saveTabsToStorage() {
        const tabsState = [];

        this.openTabs.forEach((tabData, sessionId) => {
            const customName = window.terminalManager.getSessionName(sessionId);
            tabsState.push({
                sessionId,
                projectName: tabData.projectName,
                sessionName: customName
            });
        });

        localStorage.setItem('devmanager-tabs-state', JSON.stringify({
            tabs: tabsState,
            activeSessionId: window.terminalManager.activeSessionId,
            timestamp: Date.now()
        }));
    }

    async restoreTabsFromStorage() {
        const stateJson = localStorage.getItem('devmanager-tabs-state');
        if (!stateJson) return;

        try {
            const state = JSON.parse(stateJson);

            // Check if state is not too old (24 hours)
            const maxAge = 24 * 60 * 60 * 1000;
            if (Date.now() - state.timestamp > maxAge) {
                localStorage.removeItem('devmanager-tabs-state');
                return;
            }

            // Check if a session is currently being opened manually
            if (this.pendingSessionOpen) {
                const elapsed = Date.now() - this.pendingSessionOpen.timestamp;
                if (elapsed < 5000) {
                    console.log('[restoreTabsFromStorage] Skipping - session being opened manually:', this.pendingSessionOpen.sessionId);
                    return;
                }
            }

            // Restore each tab if session still exists
            for (const tabData of state.tabs) {
                const session = this.sessions.find(s => s.id === tabData.sessionId);

                // Skip recently created sessions (they're already being opened)
                if (this.recentlyCreatedSessions.has(tabData.sessionId)) {
                    console.log('[restoreTabsFromStorage] Skipping recently created session:', tabData.sessionId);
                    continue;
                }

                // Only restore if session is still running
                if (session && session.status === 'running') {
                    await this.openTerminal(tabData.sessionId);

                    // Restore custom name if different
                    if (tabData.sessionName) {
                        window.terminalManager.renameSession(tabData.sessionId, tabData.sessionName);
                        const tab = document.querySelector(`.terminal-tab[data-session-id="${tabData.sessionId}"]`);
                        if (tab) {
                            const nameContainer = tab.querySelector('.terminal-tab-name');
                            if (nameContainer) {
                                const nameText = nameContainer.querySelector('.terminal-tab-name-text');
                                if (nameText) nameText.textContent = tabData.sessionName;
                                else nameContainer.textContent = tabData.sessionName;
                            }
                        }
                    }
                }
            }

            // Restore active tab (but respect pending session open)
            if (state.activeSessionId && this.openTabs.has(state.activeSessionId)) {
                // Don't override if a session is currently being opened
                if (this.pendingSessionOpen && this.pendingSessionOpen.sessionId !== state.activeSessionId) {
                    console.log('[restoreTabsFromStorage] Not restoring active tab - session being opened:', this.pendingSessionOpen.sessionId);
                } else if (!this.recentlyCreatedSessions.has(state.activeSessionId)) {
                    window.terminalManager.switchToSession(state.activeSessionId);
                    this.currentSession = state.activeSessionId;
                    this.updateTabActiveState(state.activeSessionId);
                }
            }

            // Restore tab order
            this.restoreTabOrder();

            // If tabs were restored, switch to terminal view
            if (this.openTabs.size > 0) {
                document.querySelectorAll('.view').forEach(v => v.classList.remove('active'));
                document.getElementById('view-terminal').classList.add('active');
                this.currentView = 'terminal';
                this._updateNavForTerminal();
            }

        } catch (e) {
            console.error('Failed to restore tabs from storage:', e);
            localStorage.removeItem('devmanager-tabs-state');
        }
    }

    // ==================== KEYBOARD SHORTCUTS ====================

    setupKeyboardShortcuts() {
        document.addEventListener('keydown', (e) => {
            // Only handle shortcuts when terminal view is active
            const terminalView = document.getElementById('view-terminal');
            if (!terminalView?.classList.contains('active')) return;

            // Don't handle shortcuts when editing tab name
            if (e.target.classList.contains('terminal-tab-name-input')) return;

            // Ctrl/Cmd + Tab - Next tab
            if ((e.ctrlKey || e.metaKey) && e.key === 'Tab' && !e.shiftKey) {
                e.preventDefault();
                this.switchToNextTab();
            }

            // Ctrl/Cmd + Shift + Tab - Previous tab
            if ((e.ctrlKey || e.metaKey) && e.key === 'Tab' && e.shiftKey) {
                e.preventDefault();
                this.switchToPreviousTab();
            }

            // Ctrl/Cmd + W - Close current tab
            if ((e.ctrlKey || e.metaKey) && e.key === 'w') {
                e.preventDefault();
                const activeSessionId = window.terminalManager.activeSessionId;
                if (activeSessionId) {
                    this.closeTab(activeSessionId);
                }
            }

            // Ctrl/Cmd + 1-9 - Switch to tab by number
            if ((e.ctrlKey || e.metaKey) && e.key >= '1' && e.key <= '9') {
                e.preventDefault();
                const tabIndex = parseInt(e.key) - 1;
                this.switchToTabByIndex(tabIndex);
            }

            // Ctrl/Cmd + Shift + F - Toggle split screen
            if ((e.ctrlKey || e.metaKey) && e.shiftKey && e.key === 'F') {
                e.preventDefault();
                this.toggleSplitScreen();
            }

            // Ctrl/Cmd + Shift + L - Link Task to session
            if ((e.ctrlKey || e.metaKey) && e.shiftKey && e.key === 'L') {
                e.preventDefault();
                const activeSessionId = window.terminalManager?.activeSessionId;
                if (activeSessionId) {
                    const linkBtn = document.getElementById('btn-link-task');
                    if (linkBtn && linkBtn.style.display !== 'none') {
                        this.showLinkTaskModal(activeSessionId);
                    }
                }
            }
        });
    }

    switchToNextTab() {
        const sessionIds = Array.from(this.openTabs.keys());
        if (sessionIds.length === 0) return;

        const currentIndex = sessionIds.indexOf(window.terminalManager.activeSessionId);
        const nextIndex = (currentIndex + 1) % sessionIds.length;

        window.terminalManager.switchToSession(sessionIds[nextIndex]);
        this.currentSession = sessionIds[nextIndex];
        this.updateTabActiveState(sessionIds[nextIndex]);
    }

    switchToPreviousTab() {
        const sessionIds = Array.from(this.openTabs.keys());
        if (sessionIds.length === 0) return;

        const currentIndex = sessionIds.indexOf(window.terminalManager.activeSessionId);
        const prevIndex = (currentIndex - 1 + sessionIds.length) % sessionIds.length;

        window.terminalManager.switchToSession(sessionIds[prevIndex]);
        this.currentSession = sessionIds[prevIndex];
        this.updateTabActiveState(sessionIds[prevIndex]);
    }

    switchToTabByIndex(index) {
        const sessionIds = Array.from(this.openTabs.keys());
        if (index >= 0 && index < sessionIds.length) {
            window.terminalManager.switchToSession(sessionIds[index]);
            this.currentSession = sessionIds[index];
            this.updateTabActiveState(sessionIds[index]);
        }
    }

    // Config
    async loadConfig() {
        try {
            this.skills = await this.api('GET', '/config/skills');
            this.mcpServers = await this.api('GET', '/config/mcps');
            this.settings = await this.api('GET', '/config/settings');
            this.renderConfig();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    renderConfig() {
        const container = document.getElementById('config-content');
        if (!container) return;

        const activeTab = document.querySelector('.tab-btn.active')?.dataset.tab || 'skills';

        let content = '';
        switch (activeTab) {
            case 'skills':
                content = this.renderSkillsConfig();
                break;
            case 'mcps':
                content = this.renderMCPsConfig();
                break;
            case 'settings':
                content = this.renderSettingsConfig();
                break;
            case 'tokens':
                content = `<div style="display:flex;align-items:center;justify-content:space-between;padding:0 4px;margin-bottom:12px">
                    <h2 style="margin:0;font-size:1.2rem">Token Usage</h2>
                    <button class="btn btn-danger-outline btn-sm" onclick="app.clearTokenUsage()" title="Limpar dados de uso de tokens">
                        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 6h18"/><path d="M19 6v14a2 2 0 01-2 2H7a2 2 0 01-2-2V6"/><path d="M8 6V4a2 2 0 012-2h4a2 2 0 012 2v2"/></svg>
                        Limpar
                    </button>
                </div>
                <div id="token-usage-global-content"></div>`;
                break;
        }

        container.innerHTML = content;
        if (activeTab === 'tokens') {
            this.loadGlobalTokenUsage(30);
        }
    }

    renderSkillsConfig() {
        // Filter skills by search and category
        const searchTerm = (this._skillSearch || '').toLowerCase();
        const filterCategory = this._skillFilterCategory || '';
        const filterStatus = this._skillFilterStatus || '';

        let filtered = this.skills;
        if (searchTerm) {
            filtered = filtered.filter(s => s.name.toLowerCase().includes(searchTerm) || s.content.toLowerCase().includes(searchTerm));
        }
        if (filterCategory) {
            filtered = filtered.filter(s => s.category === filterCategory);
        }
        if (filterStatus === 'enabled') {
            filtered = filtered.filter(s => s.enabled);
        } else if (filterStatus === 'disabled') {
            filtered = filtered.filter(s => !s.enabled);
        }

        // Get unique categories
        const categories = [...new Set(this.skills.map(s => s.category).filter(c => c))];

        const skillCards = filtered.length === 0
            ? `<div class="skills-empty-state">
                <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" style="opacity:0.4; margin-bottom: 12px;">
                    <path d="M12 6.253v13m0-13C10.832 5.477 9.246 5 7.5 5S4.168 5.477 3 6.253v13C4.168 18.477 5.754 18 7.5 18s3.332.477 4.5 1.253m0-13C13.168 5.477 14.754 5 16.5 5c1.747 0 3.332.477 4.5 1.253v13C19.832 18.477 18.247 18 16.5 18c-1.746 0-3.332.477-4.5 1.253"/>
                </svg>
                <div style="font-weight: 600; margin-bottom: 4px;">No skills found</div>
                <div style="font-size: 12px; opacity: 0.6;">Skills are markdown instructions that are synced to your projects and available to Claude during sessions.</div>
                <button class="btn btn-primary btn-sm" style="margin-top: 12px;" onclick="app.showSkillModal()">Create your first skill</button>
              </div>`
            : `<div class="card-grid">
                ${filtered.map(skill => `
                    <div class="card ${!skill.enabled ? 'card-disabled' : ''}">
                        <div class="card-header">
                            <div>
                                <div class="card-title">${this.escapeHtml(skill.name)}</div>
                                ${skill.category ? `<span class="skill-badge">${this.escapeHtml(skill.category)}</span>` : ''}
                            </div>
                            <label class="toggle-switch" title="${skill.enabled ? 'Enabled' : 'Disabled'}">
                                <input type="checkbox" ${skill.enabled ? 'checked' : ''}
                                    onchange="app.toggleSkill(${skill.id}, this.checked)">
                                <span class="toggle-slider"></span>
                            </label>
                        </div>
                        <div class="card-body">
                            <pre class="skill-preview">${this.escapeHtml(skill.content.substring(0, 200))}${skill.content.length > 200 ? '...' : ''}</pre>
                        </div>
                        <div class="card-footer-info">
                            ${skill.sync_count > 0 ? `<span class="skill-stat" title="Times synced">Synced ${skill.sync_count}x</span>` : ''}
                        </div>
                        <div class="card-actions">
                            <button class="btn btn-secondary btn-sm" onclick="app.editSkill(${skill.id})">Edit</button>
                            <button class="btn btn-secondary btn-sm" onclick="app.duplicateSkill(${skill.id})">Duplicate</button>
                            <button class="btn btn-secondary btn-sm" onclick="app.showSkillVersions(${skill.id})">History</button>
                            <button class="btn btn-danger btn-sm" onclick="app.deleteSkill(${skill.id})">Delete</button>
                        </div>
                    </div>
                `).join('')}
              </div>`;

        return `
            <div class="skills-toolbar">
                <div class="skills-toolbar-left">
                    <button class="btn btn-primary" onclick="app.showSkillModal()">Add Skill</button>
                    <button class="btn btn-secondary" onclick="app.showAISkillCreator()" title="Generate a skill using AI">Criar com IA</button>
                    <button class="btn btn-secondary" onclick="app.syncAllConfig()" title="Sync skills to all projects">Sync Now</button>
                </div>
                <div class="skills-toolbar-right">
                    <input type="text" class="form-input skills-search" placeholder="Search skills..."
                        value="${this.escapeHtml(this._skillSearch || '')}"
                        oninput="app._skillSearch = this.value; app.renderConfig()">
                    ${categories.length > 0 ? `
                        <select class="form-select skills-filter" onchange="app._skillFilterCategory = this.value; app.renderConfig()">
                            <option value="">All categories</option>
                            ${categories.map(c => `<option value="${this.escapeHtml(c)}" ${filterCategory === c ? 'selected' : ''}>${this.escapeHtml(c)}</option>`).join('')}
                        </select>
                    ` : ''}
                    <select class="form-select skills-filter" onchange="app._skillFilterStatus = this.value; app.renderConfig()">
                        <option value="" ${!filterStatus ? 'selected' : ''}>All status</option>
                        <option value="enabled" ${filterStatus === 'enabled' ? 'selected' : ''}>Enabled</option>
                        <option value="disabled" ${filterStatus === 'disabled' ? 'selected' : ''}>Disabled</option>
                    </select>
                    <button class="btn btn-secondary btn-sm" onclick="app.exportSkills()" title="Export all skills as JSON">Export</button>
                    <button class="btn btn-secondary btn-sm" onclick="app.importSkillsDialog()" title="Import skills from JSON">Import</button>
                </div>
            </div>
            ${skillCards}
        `;
    }

    renderMCPsConfig() {
        return `
            <div class="mb-4">
                <button class="btn btn-primary" onclick="app.showMCPModal()">Add MCP Server</button>
            </div>
            <div class="card-grid">
                ${this.mcpServers.map(mcp => `
                    <div class="card">
                        <div class="card-header">
                            <div class="card-title">${this.escapeHtml(mcp.name)}</div>
                            <input type="checkbox" ${mcp.enabled ? 'checked' : ''}
                                onchange="app.toggleMCP(${mcp.id}, this.checked)">
                        </div>
                        <div class="card-body">
                            <code>${this.escapeHtml(mcp.command)}</code>
                        </div>
                        <div class="card-actions">
                            <button class="btn btn-secondary btn-sm" onclick="app.editMCP(${mcp.id})">Edit</button>
                            <button class="btn btn-danger btn-sm" onclick="app.deleteMCP(${mcp.id})">Delete</button>
                        </div>
                    </div>
                `).join('')}
            </div>
        `;
    }

    renderSettingsConfig() {
        const nm = window.notificationManager;
        const pushSupported = nm && nm.isSupported();
        const pushSubscribed = nm && nm.isSubscribed();
        const pushPermission = nm ? nm.permission : 'default';
        const pushOptedOut = nm && nm.isOptedOut && nm.isOptedOut();

        let pushStatusText = '';
        let pushBtnText = '';
        let pushBtnClass = '';
        let pushBtnAction = '';

        if (!pushSupported) {
            pushStatusText = 'Push notifications are not supported on this browser.';
        } else if (pushPermission === 'denied') {
            pushStatusText = 'Notifications blocked by the browser. Unblock in your browser/OS settings.';
        } else if (pushOptedOut) {
            pushStatusText = 'Push notifications disabled by you.';
            pushBtnText = 'Enable Push Notifications';
            pushBtnClass = 'btn btn-primary btn-sm';
            pushBtnAction = 'app.togglePushNotifications(true)';
        } else if (pushSubscribed) {
            pushStatusText = 'Push notifications are enabled.';
            pushBtnText = 'Disable Push Notifications';
            pushBtnClass = 'btn btn-danger btn-sm';
            pushBtnAction = 'app.togglePushNotifications(false)';
        } else {
            pushStatusText = 'Push notifications are disabled. Enable them to receive alerts when the screen is off.';
            pushBtnText = 'Enable Push Notifications';
            pushBtnClass = 'btn btn-primary btn-sm';
            pushBtnAction = 'app.togglePushNotifications(true)';
        }

        // Build theme picker
        const themes = window.devManagerTheme ? window.devManagerTheme.THEMES : {};
        const currentTheme = window.devManagerTheme ? window.devManagerTheme.getCurrentThemeId() : 'dark';
        let themeSwatches = '';
        for (const [id, theme] of Object.entries(themes)) {
            const active = id === currentTheme ? ' active' : '';
            themeSwatches += `
                <button class="theme-swatch${active}" data-theme="${id}" onclick="window.devManagerTheme.applyTheme('${id}')" title="${theme.name}">
                    <div class="theme-swatch-preview">
                        <div class="swatch-bar" style="background:${theme.preview[0]}"></div>
                        <div class="swatch-bar" style="background:${theme.preview[1]}"></div>
                        <div class="swatch-bar" style="background:${theme.preview[2]}"></div>
                    </div>
                    <span class="theme-swatch-label">${theme.name}</span>
                </button>`;
        }

        const html = `
            <div class="card" style="margin-bottom: 16px;">
                <div class="card-header">
                    <div class="card-title">Theme</div>
                </div>
                <div class="card-body">
                    <div class="theme-picker">
                        ${themeSwatches}
                    </div>
                </div>
            </div>
            <div class="card" style="margin-bottom: 16px;">
                <div class="card-header">
                    <div class="card-title">Push Notifications</div>
                </div>
                <div class="card-body">
                    <p style="margin-bottom: 12px; color: var(--text-secondary, #999); font-size: 13px;">
                        Receive push notifications for permission requests, plan approvals, and questions even when the app is in the background or the screen is off.
                    </p>
                    <div id="push-status" style="margin-bottom: 12px; font-size: 13px;">
                        ${pushStatusText}
                    </div>
                    ${pushBtnText ? `<button class="${pushBtnClass}" onclick="${pushBtnAction}">${pushBtnText}</button>` : ''}
                </div>
            </div>
            <div class="card" style="margin-bottom: 16px;">
                <div class="card-header">
                    <div class="card-title">Task Auto-Evaluation</div>
                </div>
                <div class="card-body">
                    <p style="margin-bottom: 12px; color: var(--text-secondary, #999); font-size: 13px;">
                        When enabled, the AI assistant automatically evaluates sessions on start, end, prompt submit, and plan acceptance to suggest task actions (create, link, update, complete).
                    </p>
                    <label style="display: flex; align-items: center; gap: 8px; cursor: pointer;">
                        <input type="checkbox" id="task-auto-eval" onchange="app.saveTaskAutoEval(this.checked)">
                        <span style="font-size: 13px;">Enable automatic task evaluation</span>
                    </label>
                </div>
            </div>
            <div class="card" style="margin-bottom: 16px;">
                <div class="card-header">
                    <div class="card-title">Task Auto-Update</div>
                </div>
                <div class="card-body">
                    <p style="margin-bottom: 12px; color: var(--text-secondary, #999); font-size: 13px;">
                        When enabled, automatically applies task status changes detected from session output (e.g., marks task as done when session completes successfully). Requires Auto-Evaluation to be enabled.
                    </p>
                    <label style="display: flex; align-items: center; gap: 8px; cursor: pointer;">
                        <input type="checkbox" id="task-auto-update" onchange="app.saveTaskAutoUpdate(this.checked)">
                        <span style="font-size: 13px;">Enable automatic task updates</span>
                    </label>
                </div>
            </div>
            <div class="card">
                <div class="card-header">
                    <div class="card-title">Voice Transcription</div>
                </div>
                <div class="card-body">
                    <div class="form-group">
                        <label class="form-label">Provider</label>
                        <select class="form-input" id="whisper-provider">
                            <option value="openai">OpenAI Whisper</option>
                            <option value="groq">Groq Whisper (Faster)</option>
                        </select>
                    </div>
                    <div class="form-group">
                        <label class="form-label">OpenAI API Key</label>
                        <input type="password" class="form-input" id="openai-key" placeholder="sk-...">
                    </div>
                    <div class="form-group">
                        <label class="form-label">Groq API Key</label>
                        <input type="password" class="form-input" id="groq-key" placeholder="gsk_...">
                    </div>
                    <button class="btn btn-primary btn-sm" onclick="app.saveWhisperSettings()">Save</button>
                </div>
            </div>
            <div class="card" style="margin-top: 16px;">
                <div class="card-header">
                    <div class="card-title">AI Assistant (Anthropic)</div>
                </div>
                <div class="card-body">
                    <p style="margin-bottom: 12px; color: var(--text-secondary, #999); font-size: 13px;">
                        Configure the AI provider for the chat assistant, skill generation, and auto-configuration features.
                    </p>
                    <div class="form-group">
                        <label class="form-label">Provider</label>
                        <select class="form-input" id="ai-provider" onchange="app.onAIProviderChange()">
                            <option value="">Auto-detect</option>
                            <option value="gosdk">Agent SDK (Claude CLI)</option>
                            <option value="ollama">Ollama (Direct API)</option>
                            <option value="ollama-sdk">Ollama (via Claude CLI)</option>
                            <option value="nodesdk">Agent SDK (Node.js)</option>
                            <option value="apikey">Anthropic API Key</option>
                        </select>
                    </div>
                    <div id="ollama-settings" style="display: none; margin-top: 12px; padding-top: 12px; border-top: 1px solid var(--border-color, #333);">
                        <div class="form-group">
                            <label class="form-label">Ollama Base URL</label>
                            <input type="text" class="form-input" id="ollama-base-url" placeholder="http://localhost:11434" value="http://localhost:11434">
                            <small class="text-muted">Remote Ollama server URL</small>
                        </div>
                        <div class="form-group">
                            <label class="form-label">Ollama API Key</label>
                            <input type="password" class="form-input" id="ollama-api-key" placeholder="ollama-key">
                            <small class="text-muted">API key for remote Ollama (optional for local)</small>
                        </div>
                        <div class="form-group">
                            <label class="form-label">Model</label>
                            <input type="text" class="form-input" id="ollama-model" placeholder="qwen3-coder" value="qwen3-coder">
                            <small class="text-muted">Ollama model name (e.g., qwen3-coder, llama3.2)</small>
                        </div>
                    </div>
                    <div class="form-group" id="anthropic-key-group">
                        <label class="form-label">Anthropic API Key</label>
                        <input type="password" class="form-input" id="anthropic-key" placeholder="sk-ant-...">
                        <small class="text-muted">Only needed for API Key provider.</small>
                    </div>
                    <div class="form-group" id="ai-model-group">
                        <label class="form-label">Model</label>
                        <select class="form-input" id="ai-model">
                            <option value="claude-sonnet-4-5-20250929">Claude Sonnet 4.5</option>
                            <option value="claude-haiku-4-5-20251001">Claude Haiku 4.5 (Faster)</option>
                            <option value="claude-opus-4-6">Claude Opus 4.6</option>
                        </select>
                    </div>
                    <div style="display: flex; gap: 8px;">
                        <button class="btn btn-primary btn-sm" onclick="app.saveAISettings()">Save</button>
                        <button class="btn btn-secondary btn-sm" onclick="app.testAIConnection()">Test Connection</button>
                    </div>
                    <div id="ai-test-result" style="margin-top: 8px; font-size: 12px;"></div>
                </div>
            </div>
            <div class="card" style="margin-top: 16px;">
                <div class="card-header">
                    <div class="card-title">MCP HTTP Endpoint</div>
                </div>
                <div class="card-body">
                    <p style="margin-bottom: 12px; color: var(--text-secondary, #999); font-size: 13px;">
                        Expose DevManager tools over HTTP for external AI systems (e.g. OpenClaw). Restart required after enabling.
                    </p>
                    <div class="form-checkbox" style="margin-bottom: 12px;">
                        <input type="checkbox" id="mcp-http-enabled">
                        <label>Enable MCP HTTP endpoint (/mcp)</label>
                    </div>
                    <div style="margin-bottom: 12px;">
                        <label class="form-label">API Key (for remote access)</label>
                        <div style="display: flex; gap: 8px; align-items: center;">
                            <span id="mcp-api-key-status" style="font-size: 12px; color: var(--color-text-muted);">Checking...</span>
                            <button class="btn btn-primary btn-sm" onclick="app.generateMCPAPIKey()">Generate</button>
                            <button class="btn btn-danger btn-sm" onclick="app.revokeMCPAPIKey()">Revoke</button>
                        </div>
                    </div>
                    <button class="btn btn-primary btn-sm" onclick="app.saveMCPHTTPSettings()">Save</button>
                </div>
            </div>
            <div class="card" style="margin-top: 16px;">
                <div class="card-header">
                    <div class="card-title">Tool Policies</div>
                </div>
                <div class="card-body">
                    <p style="margin-bottom: 12px; color: var(--text-secondary, #999); font-size: 13px;">
                        Control which DevManager tools are available in each context. Uncheck tools to deny them.
                    </p>
                    <div id="tp-contexts" style="display: flex; flex-direction: column; gap: 8px;"></div>
                    <button class="btn btn-primary btn-sm" style="margin-top: 12px;" onclick="app.saveToolPolicies()">Save</button>
                </div>
            </div>
        `;

        // Populate settings after render
        setTimeout(() => {
            if (this.settings) {
                const autoEvalCheckbox = document.getElementById('task-auto-eval');
                if (autoEvalCheckbox) {
                    autoEvalCheckbox.checked = this.settings.task_auto_eval_enabled === 'true';
                }
                const autoUpdateCheckbox = document.getElementById('task-auto-update');
                if (autoUpdateCheckbox) {
                    autoUpdateCheckbox.checked = this.settings.task_auto_update_enabled === 'true';
                }
                const providerSelect = document.getElementById('whisper-provider');
                if (providerSelect && this.settings.whisper_provider) {
                    providerSelect.value = this.settings.whisper_provider;
                }
                const aiProviderSelect = document.getElementById('ai-provider');
                if (aiProviderSelect && this.settings.ai_provider) {
                    aiProviderSelect.value = this.settings.ai_provider;
                }
                const aiModelSelect = document.getElementById('ai-model');
                if (aiModelSelect && this.settings.ai_model) {
                    aiModelSelect.value = this.settings.ai_model;
                }
                const ollamaURLInput = document.getElementById('ollama-base-url');
                if (ollamaURLInput && this.settings.ollama_base_url) {
                    ollamaURLInput.value = this.settings.ollama_base_url;
                }
                const ollamaModelInput = document.getElementById('ollama-model');
                if (ollamaModelInput && this.settings.ollama_model) {
                    ollamaModelInput.value = this.settings.ollama_model;
                }
                if (aiProviderSelect) {
                    this.onAIProviderChange();
                }
                const mcpHttpCheckbox = document.getElementById('mcp-http-enabled');
                if (mcpHttpCheckbox) {
                    mcpHttpCheckbox.checked = this.settings.mcp_http_enabled === 'true';
                }

                // Show API key previews
                const apiKeyFields = [
                    { id: 'anthropic-key', setting: 'anthropic_api_key_preview' },
                    { id: 'openai-key', setting: 'openai_api_key_preview' },
                    { id: 'groq-key', setting: 'groq_api_key_preview' },
                    { id: 'ollama-api-key', setting: 'ollama_api_key_preview' },
                ];
                for (const field of apiKeyFields) {
                    const input = document.getElementById(field.id);
                    const preview = this.settings[field.setting];
                    if (input && preview) {
                        input.placeholder = preview;
                        const existing = input.parentNode.querySelector('.api-key-set');
                        if (!existing) {
                            const indicator = document.createElement('small');
                            indicator.className = 'api-key-set';
                            indicator.textContent = `Key configured: ${preview}`;
                            indicator.style.cssText = 'display:block;margin-top:4px;font-size:12px;color:var(--color-success, #4caf50);';
                            input.parentNode.appendChild(indicator);
                        }
                    }
                }
            }
            this.loadMCPAPIKeyStatus();
            this.loadToolPolicies();
        }, 0);

        return html;
    }

    async togglePushNotifications(enable) {
        const nm = window.notificationManager;
        if (!nm) return;

        if (enable) {
            const granted = await nm.requestPermission();
            if (!granted) {
                this.showToast('Warning', 'Notification permission was denied. Check your browser settings.', 'warning');
            }
        } else {
            await nm.unsubscribe();
        }

        // Re-render settings to update the button state
        this.renderConfig();
    }

    // Modals
    showModal(title, content, actions = '') {
        const overlay = document.getElementById('modal-overlay');
        const container = document.getElementById('modal-container');

        container.innerHTML = `
            <div class="modal-header">
                <h3 class="modal-title">${title}</h3>
                <button class="btn-icon" onclick="app.hideModal()">
                    <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                        <line x1="18" y1="6" x2="6" y2="18"></line>
                        <line x1="6" y1="6" x2="18" y2="18"></line>
                    </svg>
                </button>
            </div>
            <div class="modal-body">${content}</div>
            ${actions ? `<div class="modal-footer">${actions}</div>` : ''}
        `;

        overlay.classList.remove('hidden');
    }

    hideModal() {
        document.getElementById('modal-overlay').classList.add('hidden');
    }

    showProjectModal(project = null) {
        const isEdit = project && project.id;
        const content = `
            <form id="project-form">
                <div class="form-group">
                    <label class="form-label">Name</label>
                    <input type="text" class="form-input" name="name" value="${project?.name || ''}" required>
                </div>
                <div class="form-group">
                    <label class="form-label">Path</label>
                    <input type="text" class="form-input" name="path" value="${project?.path || ''}" required>
                </div>
                <div class="form-group">
                    <label class="form-label">Type</label>
                    <select class="form-select" name="type" onchange="app.toggleSSHFields(this.value)">
                        <option value="local" ${project?.type === 'local' ? 'selected' : ''}>Local</option>
                        <option value="remote" ${project?.type === 'remote' ? 'selected' : ''}>Remote (SSH)</option>
                    </select>
                </div>
                <div class="form-group">
                    <label class="form-label" style="display: flex; align-items: center; gap: 8px;">
                        Tool Policy
                        <span style="font-size: 11px; font-weight: normal; color: var(--color-text-muted);">(click to expand)</span>
                    </label>
                    <div class="tp-context-section" style="margin-bottom: 4px;">
                        <div class="tp-context-header" onclick="app.toggleProjectToolPolicy()">
                            <span class="tp-context-arrow" id="tp-project-arrow">&#9654;</span>
                            <span>Override global policy for this project</span>
                            <span class="tp-context-summary" id="tp-project-summary" style="font-size: 11px;">Inheriting global</span>
                        </div>
                        <div id="tp-project-body" style="display:none;">
                            <div style="padding: 8px 12px; border-top: 1px solid var(--color-border);">
                                <div class="form-checkbox" style="margin-bottom: 8px;">
                                    <input type="checkbox" id="tp-project-override" onchange="app.onProjectPolicyOverrideChange()">
                                    <label>Enable per-project override</label>
                                </div>
                                <div id="tp-project-tools" style="display:none;">
                                    <div style="display: flex; gap: 8px; margin-bottom: 6px;">
                                        <button class="btn btn-sm btn-secondary" onclick="app.tpProjectSelectAll(true)">Select All</button>
                                        <button class="btn btn-sm btn-secondary" onclick="app.tpProjectSelectAll(false)">Deselect All</button>
                                    </div>
                                    <div class="tp-tool-grid" id="tp-project-tool-grid"></div>
                                </div>
                            </div>
                        </div>
                    </div>
                    <input type="hidden" name="tool_policy" id="project-tool-policy-value" value="${project?.tool_policy || ''}">
                </div>
                <div id="ssh-fields" class="${project?.type !== 'remote' ? 'hidden' : ''}">
                    <div class="form-group">
                        <label class="form-label">SSH Host</label>
                        <input type="text" class="form-input" name="ssh_host" value="${project?.ssh_host?.String || ''}">
                    </div>
                    <div class="form-group">
                        <label class="form-label">SSH Port</label>
                        <input type="number" class="form-input" name="ssh_port" value="${project?.ssh_port?.Int64 || 22}">
                    </div>
                    <div class="form-group">
                        <label class="form-label">SSH User</label>
                        <input type="text" class="form-input" name="ssh_user" value="${project?.ssh_user?.String || ''}">
                    </div>
                    <div class="form-group">
                        <label class="form-label">Auth Type</label>
                        <select class="form-select" name="ssh_auth_type">
                            <option value="password" ${project?.ssh_auth_type?.String === 'password' ? 'selected' : ''}>Password</option>
                            <option value="key" ${project?.ssh_auth_type?.String === 'key' ? 'selected' : ''}>Private Key</option>
                        </select>
                    </div>
                    <div class="form-group">
                        <label class="form-label">Credential (Password or Key)</label>
                        <textarea class="form-textarea" name="ssh_credential" placeholder="Enter password or paste private key"></textarea>
                    </div>
                </div>
            </form>
        `;

        const actions = `
            <button class="btn btn-secondary" onclick="app.hideModal()">Cancel</button>
            ${isEdit ? `<button class="btn btn-danger" onclick="app.deleteProject(${project.id})">Delete</button>` : ''}
            <button class="btn btn-primary" onclick="app.saveProject(${project?.id || 'null'})">${isEdit ? 'Save' : 'Create'}</button>
        `;

        this.showModal(isEdit ? 'Edit Project' : 'New Project', content, actions);
        this._populateProjectToolPolicy(project?.tool_policy || '');
    }

    async _populateProjectToolPolicy(policyJson) {
        // Load tools if not cached
        if (!this._tpTools) {
            try { this._tpTools = await this.api('GET', '/config/mcp-tools'); } catch(e) { return; }
        }
        const grid = document.getElementById('tp-project-tool-grid');
        if (!grid) return;

        let policy = null;
        try { if (policyJson) policy = JSON.parse(policyJson); } catch(e) {}
        const hasOverride = policy && policy.mode;
        const denied = new Set(policy?.denied || []);
        const allowed = new Set(policy?.allowed || []);
        const mode = policy?.mode || 'allow_all';

        grid.innerHTML = this._tpTools.map(t => {
            let checked;
            if (mode === 'deny_all') {
                checked = allowed.has(t.name);
            } else {
                checked = !denied.has(t.name);
            }
            const shortDesc = t.description.length > 60 ? t.description.slice(0, 60) + '...' : t.description;
            return `<label class="tp-tool-item" title="${t.description.replace(/"/g, '&quot;')}">
                <input type="checkbox" data-ctx="project" data-tool="${t.name}" ${checked ? 'checked' : ''}>
                <span class="tp-tool-name">${t.name}</span>
                <span class="tp-tool-desc">${shortDesc}</span>
            </label>`;
        }).join('');

        const overrideCb = document.getElementById('tp-project-override');
        const toolsDiv = document.getElementById('tp-project-tools');
        const summaryEl = document.getElementById('tp-project-summary');
        if (overrideCb) overrideCb.checked = hasOverride;
        if (toolsDiv) toolsDiv.style.display = hasOverride ? '' : 'none';
        if (summaryEl) summaryEl.textContent = hasOverride ? `Custom (${mode})` : 'Inheriting global';

        // Listen for changes to update hidden field
        grid.addEventListener('change', () => this._syncProjectPolicyHidden());
    }

    toggleProjectToolPolicy() {
        const body = document.getElementById('tp-project-body');
        const arrow = document.getElementById('tp-project-arrow');
        if (!body) return;
        const open = body.style.display !== 'none';
        body.style.display = open ? 'none' : '';
        if (arrow) arrow.innerHTML = open ? '&#9654;' : '&#9660;';
    }

    onProjectPolicyOverrideChange() {
        const enabled = document.getElementById('tp-project-override')?.checked;
        const toolsDiv = document.getElementById('tp-project-tools');
        const summaryEl = document.getElementById('tp-project-summary');
        if (toolsDiv) toolsDiv.style.display = enabled ? '' : 'none';
        if (summaryEl) summaryEl.textContent = enabled ? 'Custom' : 'Inheriting global';
        this._syncProjectPolicyHidden();
    }

    tpProjectSelectAll(checked) {
        document.querySelectorAll('input[data-ctx="project"]').forEach(cb => cb.checked = checked);
        this._syncProjectPolicyHidden();
    }

    _syncProjectPolicyHidden() {
        const hidden = document.getElementById('project-tool-policy-value');
        if (!hidden) return;

        const override = document.getElementById('tp-project-override')?.checked;
        if (!override) {
            hidden.value = '';
            return;
        }

        const all = document.querySelectorAll('input[data-ctx="project"]');
        const checked = document.querySelectorAll('input[data-ctx="project"]:checked');

        if (checked.length === all.length) {
            hidden.value = JSON.stringify({ mode: 'allow_all' });
        } else if (checked.length === 0) {
            hidden.value = JSON.stringify({ mode: 'deny_all' });
        } else if (checked.length <= all.length / 2) {
            const allowed = Array.from(checked).map(cb => cb.dataset.tool);
            hidden.value = JSON.stringify({ mode: 'deny_all', allowed });
        } else {
            const unchecked = document.querySelectorAll('input[data-ctx="project"]:not(:checked)');
            const denied = Array.from(unchecked).map(cb => cb.dataset.tool);
            hidden.value = JSON.stringify({ mode: 'allow_all', denied });
        }
    }

    toggleSSHFields(type) {
        document.getElementById('ssh-fields').classList.toggle('hidden', type !== 'remote');
    }

    async saveProject(projectId) {
        const form = document.getElementById('project-form');
        const formData = new FormData(form);
        const data = {
            name: formData.get('name'),
            path: formData.get('path'),
            type: formData.get('type'),
            ssh_host: formData.get('ssh_host'),
            ssh_port: parseInt(formData.get('ssh_port')) || 22,
            ssh_user: formData.get('ssh_user'),
            ssh_auth_type: formData.get('ssh_auth_type'),
            ssh_credential: formData.get('ssh_credential'),
            tool_policy: formData.get('tool_policy') || ''
        };

        try {
            if (projectId) {
                await this.api('PUT', `/projects/${projectId}`, data);
            } else {
                await this.api('POST', '/projects', data);
            }
            this.hideModal();
            this.showToast('Success', 'Project saved', 'success');
            this.loadProjects();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    deleteProject(projectId) {
        showConfirmModal(
            'Excluir projeto?',
            'O projeto será removido permanentemente. Esta ação não pode ser desfeita.',
            async () => {
                try {
                    await this.api('DELETE', `/projects/${projectId}`);
                    this.hideModal();
                    this.showToast('Success', 'Project deleted', 'success');
                    if (this.currentView === 'project-detail') {
                        this._detailProject = null;
                        this.showView('projects');
                    } else {
                        this.loadProjects();
                    }
                } catch (error) {
                    this.showToast('Error', error.message, 'error');
                }
            },
            'Excluir'
        );
    }

    // Auto-update version check
    setupVersionCheck() {
        this.appVersion = document.querySelector('meta[name="app-version"]')?.content || '';
        this._updateBannerShown = false;

        // Check for updates every 60 seconds
        setInterval(() => this.checkForUpdate(), 60000);

        // Check when page becomes visible (user returns to tab/app)
        document.addEventListener('visibilitychange', () => {
            if (document.visibilityState === 'visible') {
                this.checkForUpdate();
            }
        });
    }

    async checkForUpdate() {
        if (this._updateBannerShown || !this.appVersion) return;
        try {
            const resp = await fetch('/api/version');
            if (!resp.ok) return;
            const data = await resp.json();
            if (data.version && data.version !== this.appVersion) {
                this.showUpdateBanner();
            }
        } catch (e) {
            // Network error — ignore
        }
    }

    showUpdateBanner() {
        if (this._updateBannerShown) return;
        this._updateBannerShown = true;

        const banner = document.createElement('div');
        banner.id = 'update-banner';
        banner.style.cssText = 'position:fixed;top:0;left:0;right:0;z-index:10000;background:linear-gradient(135deg,var(--color-gradient-start,#667eea),var(--color-gradient-end,#764ba2));color:#fff;padding:12px 20px;display:flex;align-items:center;justify-content:center;gap:12px;font-size:14px;font-weight:500;box-shadow:0 2px 12px rgba(0,0,0,0.3);';
        banner.innerHTML = `
            <span>Nova vers\u00e3o dispon\u00edvel</span>
            <button onclick="window.location.reload()" style="background:#fff;color:var(--color-gradient-end,#764ba2);border:none;padding:6px 16px;border-radius:6px;font-weight:600;cursor:pointer;font-size:13px;">Atualizar</button>
            <button onclick="this.parentElement.remove()" style="background:transparent;color:#fff;border:1px solid rgba(255,255,255,0.4);padding:6px 12px;border-radius:6px;cursor:pointer;font-size:13px;">Depois</button>
        `;
        document.body.prepend(banner);
    }

    // Toast notifications
    showToast(title, message, type = 'info') {
        // Only show toasts for errors and warnings
        if (type !== 'error' && type !== 'warning') return;

        const container = document.getElementById('toast-container');
        const toast = document.createElement('div');
        toast.className = `toast toast-${type}`;
        toast.innerHTML = `
            <span class="toast-message"><strong>${title}</strong> ${message}</span>
            <button class="toast-close" onclick="this.parentElement.remove()">
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                    <line x1="18" y1="6" x2="6" y2="18"></line>
                    <line x1="6" y1="6" x2="18" y2="18"></line>
                </svg>
            </button>
        `;
        container.appendChild(toast);

        setTimeout(() => toast.remove(), 5000);
    }

    // Utility
    escapeHtml(text) {
        const div = document.createElement('div');
        div.textContent = text || '';
        return div.innerHTML;
    }

    formatTime(timestamp) {
        const date = new Date(timestamp);
        return date.toLocaleString();
    }

    formatElapsed(startTime) {
        const start = new Date(startTime);
        const now = new Date();
        const diffMs = now - start;
        const diffMin = Math.floor(diffMs / 60000);
        if (diffMin < 1) return '<1m';
        if (diffMin < 60) return `${diffMin}m`;
        const hours = Math.floor(diffMin / 60);
        const mins = diffMin % 60;
        if (hours < 24) return `${hours}h ${mins}m`;
        const days = Math.floor(hours / 24);
        return `${days}d ${hours % 24}h`;
    }

    formatTimeAgo(timestamp) {
        if (!timestamp) return '';
        const date = new Date(timestamp);
        if (isNaN(date.getTime())) return '';
        const now = new Date();
        const diffMs = now - date;
        const diffSec = Math.floor(diffMs / 1000);
        if (diffSec < 60) return `${diffSec}s ago`;
        const diffMin = Math.floor(diffSec / 60);
        if (diffMin < 60) return `${diffMin}m ago`;
        const hours = Math.floor(diffMin / 60);
        return `${hours}h ago`;
    }

    formatTokens(count) {
        if (count >= 1000000) return (count / 1000000).toFixed(1) + 'M';
        if (count >= 1000) return (count / 1000).toFixed(1) + 'k';
        return count.toString();
    }

    truncateStr(str, maxLen) {
        if (!str || str.length <= maxLen) return str;
        return str.substring(0, maxLen - 1) + '\u2026';
    }

    // ==================== SESSION NAME TOOLTIP ====================

    _isTextTruncated(el) {
        if (el.scrollWidth > el.clientWidth) return true;
        // Check children too (for container divs with truncated child spans)
        for (const child of el.children) {
            if (child.scrollWidth > child.clientWidth) return true;
        }
        return false;
    }

    _showSessionTooltip(el, text) {
        if (!this._isTextTruncated(el)) return;

        const rect = el.getBoundingClientRect();
        const tooltip = this._tooltipEl;
        tooltip.textContent = text;

        // Position below the element, left-aligned
        tooltip.style.left = `${rect.left}px`;
        tooltip.style.top = `${rect.bottom + 6}px`;
        tooltip.classList.add('visible');

        // Clamp to viewport
        const tipRect = tooltip.getBoundingClientRect();
        if (tipRect.right > window.innerWidth - 8) {
            tooltip.style.left = `${window.innerWidth - tipRect.width - 8}px`;
        }
        if (tipRect.bottom > window.innerHeight - 8) {
            tooltip.style.top = `${rect.top - tipRect.height - 6}px`;
        }
    }

    _hideSessionTooltip() {
        this._tooltipEl.classList.remove('visible');
    }

    _showSessionPopup(el, text) {
        if (!this._isTextTruncated(el)) return;
        this._hideSessionPopup();

        const rect = el.getBoundingClientRect();

        const overlay = document.createElement('div');
        overlay.className = 'session-name-popup-overlay';
        overlay.addEventListener('click', () => this._hideSessionPopup());

        const popup = document.createElement('div');
        popup.className = 'session-name-popup';
        popup.textContent = text;

        document.body.appendChild(overlay);
        document.body.appendChild(popup);

        // Position centered above the element
        const popupRect = popup.getBoundingClientRect();
        let left = rect.left + (rect.width / 2) - (popupRect.width / 2);
        left = Math.max(16, Math.min(left, window.innerWidth - popupRect.width - 16));
        let top = rect.top - popupRect.height - 8;
        if (top < 16) top = rect.bottom + 8;

        popup.style.left = `${left}px`;
        popup.style.top = `${top}px`;

        this._activePopup = { overlay, popup };
    }

    _hideSessionPopup() {
        if (this._activePopup) {
            this._activePopup.overlay.remove();
            this._activePopup.popup.remove();
            this._activePopup = null;
        }
    }

    _attachSessionNameTooltip(el, fullText) {
        el.dataset.fullName = fullText;

        if (el._tooltipAttached) return;
        el._tooltipAttached = true;

        // Desktop: hover
        el.addEventListener('mouseenter', () => {
            this._showSessionTooltip(el, el.dataset.fullName);
        });
        el.addEventListener('mouseleave', () => this._hideSessionTooltip());

        // Mobile: long-press
        let pressTimer = null;
        el.addEventListener('touchstart', (e) => {
            pressTimer = setTimeout(() => {
                this._showSessionPopup(el, el.dataset.fullName);
                pressTimer = null;
            }, 500);
        }, { passive: true });
        el.addEventListener('touchend', () => {
            if (pressTimer) { clearTimeout(pressTimer); pressTimer = null; }
        });
        el.addEventListener('touchmove', () => {
            if (pressTimer) { clearTimeout(pressTimer); pressTimer = null; }
        });
    }

    // Event listeners
    setupEventListeners() {
        // Add project button
        document.getElementById('btn-add-project')?.addEventListener('click', () => {
            this.showProjectModal();
        });

        // Config tabs
        document.querySelectorAll('.tab-btn').forEach(btn => {
            btn.addEventListener('click', () => {
                document.querySelectorAll('.tab-btn').forEach(b => b.classList.remove('active'));
                btn.classList.add('active');
                this.renderConfig();
            });
        });

        // Sync all button
        document.getElementById('btn-sync-all')?.addEventListener('click', async () => {
            try {
                await this.api('POST', '/config/sync-all');
                this.showToast('Success', 'Config synced to all projects', 'success');
            } catch (error) {
                this.showToast('Error', error.message, 'error');
            }
        });

        // Project detail back button
        document.getElementById('btn-back-projects')?.addEventListener('click', () => {
            this.showView('projects');
        });

        // Terminal back button
        document.getElementById('btn-back-sessions')?.addEventListener('click', () => {
            this.showView('projects');
        });

        // Link Task button
        document.getElementById('btn-link-task')?.addEventListener('click', () => {
            const sessionId = window.terminalManager?.activeSessionId || this.currentSession;
            if (sessionId) this.showLinkTaskModal(sessionId);
        });

        // Stop session button
        document.getElementById('btn-stop-session')?.addEventListener('click', () => {
            const activeSessionId = window.terminalManager?.activeSessionId || this.currentSession;
            if (activeSessionId) {
                this.stopSession(activeSessionId);
            }
        });

        // Split-screen toggle button
        document.getElementById('split-screen-toggle')?.addEventListener('click', () => {
            this.toggleSplitScreen();
        });

        // Modal overlay click to close
        document.getElementById('modal-overlay')?.addEventListener('click', (e) => {
            if (e.target.id === 'modal-overlay') {
                this.hideModal();
            }
        });

        // Keyboard shortcuts
        document.addEventListener('keydown', (e) => {
            if (e.key === 'Escape') {
                this.hideModal();
            }
        });
    }

    showSkillModal(skill = null) {
        const isEdit = !!skill;
        this._skillModalDirty = false;
        const content = `
            <form id="skill-form" oninput="app._skillModalDirty = true">
                <div class="form-group">
                    <label class="form-label">Name</label>
                    <input type="text" class="form-input" name="name" value="${this.escapeHtml(skill?.name || '')}" required
                        placeholder="e.g. coding-style, deploy-guide">
                </div>
                <div class="form-group">
                    <label class="form-label">Category <span style="opacity:0.5; font-weight:normal;">(optional)</span></label>
                    <input type="text" class="form-input" name="category" value="${this.escapeHtml(skill?.category || '')}"
                        placeholder="e.g. Python, Git, Deploy">
                </div>
                <div class="form-group">
                    <label class="form-label">
                        Content
                        <div class="skill-editor-tabs">
                            <button type="button" class="skill-tab active" onclick="app.switchSkillEditorTab('edit', this)">Edit</button>
                            <button type="button" class="skill-tab" onclick="app.switchSkillEditorTab('preview', this)">Preview</button>
                        </div>
                    </label>
                    <div id="skill-editor-edit">
                        <textarea class="form-textarea skill-content-editor" name="content" rows="14" required
                            placeholder="# Skill Title&#10;&#10;Write markdown instructions for Claude...">${this.escapeHtml(skill?.content || '')}</textarea>
                    </div>
                    <div id="skill-editor-preview" class="skill-preview-pane" style="display:none;"></div>
                </div>
                <div class="form-checkbox">
                    <input type="checkbox" name="enabled" ${skill?.enabled !== false ? 'checked' : ''}>
                    <label>Enabled</label>
                </div>
            </form>
        `;

        const actions = `
            <button class="btn btn-secondary" onclick="app.closeSkillModal()">Cancel</button>
            <button class="btn btn-primary" onclick="app.saveSkill(${skill?.id || 'null'})">${isEdit ? 'Save' : 'Create'}</button>
        `;

        this.showModal(isEdit ? 'Edit Skill' : 'New Skill', content, actions);
    }

    closeSkillModal() {
        if (this._skillModalDirty) {
            showConfirmModal(
                'Descartar alterações?',
                'Você tem alterações não salvas. Deseja descartá-las?',
                () => { this.hideModal(); },
                'Descartar'
            );
            return;
        }
        this.hideModal();
    }

    switchSkillEditorTab(tab, btn) {
        const editPane = document.getElementById('skill-editor-edit');
        const previewPane = document.getElementById('skill-editor-preview');
        if (!editPane || !previewPane) return;

        // Update tab buttons
        btn.parentElement.querySelectorAll('.skill-tab').forEach(t => t.classList.remove('active'));
        btn.classList.add('active');

        if (tab === 'preview') {
            const content = document.querySelector('textarea[name="content"]')?.value || '';
            previewPane.innerHTML = this.renderMarkdownPreview(content);
            editPane.style.display = 'none';
            previewPane.style.display = 'block';
        } else {
            editPane.style.display = 'block';
            previewPane.style.display = 'none';
        }
    }

    renderMarkdownPreview(md) {
        // Simple markdown to HTML renderer
        let html = this.escapeHtml(md);
        // Headers
        html = html.replace(/^### (.+)$/gm, '<h3>$1</h3>');
        html = html.replace(/^## (.+)$/gm, '<h2>$1</h2>');
        html = html.replace(/^# (.+)$/gm, '<h1>$1</h1>');
        // Bold and italic
        html = html.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>');
        html = html.replace(/\*(.+?)\*/g, '<em>$1</em>');
        // Code blocks
        html = html.replace(/```(\w*)\n([\s\S]*?)```/g, '<pre><code>$2</code></pre>');
        // Inline code
        html = html.replace(/`([^`]+)`/g, '<code>$1</code>');
        // Lists
        html = html.replace(/^- (.+)$/gm, '<li>$1</li>');
        html = html.replace(/(<li>.*<\/li>\n?)+/g, '<ul>$&</ul>');
        // Line breaks
        html = html.replace(/\n\n/g, '</p><p>');
        html = '<p>' + html + '</p>';
        return html;
    }

    async saveSkill(skillId) {
        const form = document.getElementById('skill-form');
        const name = form.querySelector('input[name="name"]').value.trim();
        const content = form.querySelector('textarea[name="content"]').value.trim();
        const category = form.querySelector('input[name="category"]').value.trim();

        if (!name) {
            this.showToast('Error', 'Name is required', 'error');
            return;
        }
        if (!content) {
            this.showToast('Error', 'Content is required', 'error');
            return;
        }
        if (/[/\\]/.test(name) || name.includes('..')) {
            this.showToast('Error', 'Name cannot contain / \\ or ..', 'error');
            return;
        }

        const data = {
            name,
            content,
            category,
            enabled: form.querySelector('input[name="enabled"]').checked
        };

        // Preserve sort_order if editing
        if (skillId) {
            const existing = this.skills.find(s => s.id === skillId);
            if (existing) data.sort_order = existing.sort_order;
        }

        try {
            if (skillId) {
                await this.api('PUT', `/config/skills/${skillId}`, data);
            } else {
                await this.api('POST', '/config/skills', data);
            }
            this._skillModalDirty = false;
            this.hideModal();
            this.showToast('Success', 'Skill saved', 'success');
            this.loadConfig();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    editSkill(skillId) {
        const skill = this.skills.find(s => s.id === skillId);
        if (skill) this.showSkillModal(skill);
    }

    deleteSkill(skillId) {
        showConfirmModal(
            'Excluir skill?',
            'A skill será removida permanentemente. Esta ação não pode ser desfeita.',
            async () => {
                try {
                    await this.api('DELETE', `/config/skills/${skillId}`);
                    this.showToast('Success', 'Skill deleted', 'success');
                    this.loadConfig();
                } catch (error) {
                    this.showToast('Error', error.message, 'error');
                }
            },
            'Excluir'
        );
    }

    async toggleSkill(skillId, enabled) {
        const skill = this.skills.find(s => s.id === skillId);
        if (!skill) return;
        try {
            await this.api('PUT', `/config/skills/${skillId}`, { ...skill, enabled });
            this.showToast('Success', enabled ? 'Skill enabled' : 'Skill disabled', 'success');
        } catch (error) {
            this.showToast('Error', error.message, 'error');
            // Revert checkbox
            this.loadConfig();
        }
    }

    async duplicateSkill(skillId) {
        try {
            await this.api('POST', `/config/skills/${skillId}/duplicate`);
            this.showToast('Success', 'Skill duplicated', 'success');
            this.loadConfig();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    async syncAllConfig() {
        try {
            this.showToast('Info', 'Syncing skills to all projects...', 'info');
            await this.api('POST', '/config/sync-all');
            this.showToast('Success', 'Skills synced to all projects', 'success');
            this.loadConfig();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    async exportSkills() {
        try {
            const skills = await this.api('GET', '/config/skills/export');
            const blob = new Blob([JSON.stringify(skills, null, 2)], { type: 'application/json' });
            const url = URL.createObjectURL(blob);
            const a = document.createElement('a');
            a.href = url;
            a.download = 'devmanager-skills.json';
            a.click();
            URL.revokeObjectURL(url);
            this.showToast('Success', 'Skills exported', 'success');
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    importSkillsDialog() {
        const input = document.createElement('input');
        input.type = 'file';
        input.accept = '.json';
        input.onchange = async (e) => {
            const file = e.target.files[0];
            if (!file) return;
            try {
                const text = await file.text();
                const skills = JSON.parse(text);
                const result = await this.api('POST', '/config/skills/import', skills);
                this.showToast('Success', `Imported ${result.imported} skills (${result.skipped} skipped)`, 'success');
                this.loadConfig();
            } catch (error) {
                this.showToast('Error', error.message || 'Invalid JSON file', 'error');
            }
        };
        input.click();
    }

    async showSkillVersions(skillId) {
        try {
            const versions = await this.api('GET', `/config/skills/${skillId}/versions`);
            const skill = this.skills.find(s => s.id === skillId);
            if (!versions.length) {
                this.showToast('Info', 'No version history yet', 'info');
                return;
            }

            const content = `
                <div class="skill-versions-list">
                    ${versions.map(v => `
                        <div class="skill-version-item">
                            <div class="skill-version-info">
                                <strong>v${v.version}</strong> - ${this.escapeHtml(v.name)}
                                <div class="skill-version-date">${new Date(v.created_at).toLocaleString()}</div>
                                <pre class="skill-preview" style="margin-top: 4px;">${this.escapeHtml(v.content.substring(0, 150))}${v.content.length > 150 ? '...' : ''}</pre>
                            </div>
                            <button class="btn btn-secondary btn-sm" onclick="app.restoreSkillVersion(${skillId}, ${v.id})">Restore</button>
                        </div>
                    `).join('')}
                </div>
            `;

            this.showModal(`Version History: ${this.escapeHtml(skill?.name || '')}`, content, `
                <button class="btn btn-secondary" onclick="app.hideModal()">Close</button>
            `);
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    restoreSkillVersion(skillId, versionId) {
        showConfirmModal(
            'Restaurar versão?',
            'O conteúdo atual será salvo como uma nova versão antes de restaurar.',
            async () => {
                try {
                    await this.api('POST', `/config/skills/${skillId}/versions/${versionId}/restore`);
                    this.hideModal();
                    this.showToast('Success', 'Version restored', 'success');
                    this.loadConfig();
                } catch (error) {
                    this.showToast('Error', error.message, 'error');
                }
            },
            'Restaurar'
        );
    }

    showMCPModal(mcp = null) {
        const isEdit = !!mcp;
        const content = `
            <form id="mcp-form">
                <div class="form-group">
                    <label class="form-label">Name</label>
                    <input type="text" class="form-input" name="name" value="${mcp?.name || ''}" required>
                </div>
                <div class="form-group">
                    <label class="form-label">Command</label>
                    <input type="text" class="form-input" name="command" value="${mcp?.command || ''}" required>
                </div>
                <div class="form-group">
                    <label class="form-label">Arguments (JSON array)</label>
                    <input type="text" class="form-input" name="args" value="${mcp?.args || '[]'}">
                </div>
                <div class="form-group">
                    <label class="form-label">Environment (JSON object)</label>
                    <input type="text" class="form-input" name="env" value="${mcp?.env || '{}'}">
                </div>
                <div class="form-checkbox">
                    <input type="checkbox" name="enabled" ${mcp?.enabled !== false ? 'checked' : ''}>
                    <label>Enabled</label>
                </div>
            </form>
        `;

        const actions = `
            <button class="btn btn-secondary" onclick="app.hideModal()">Cancel</button>
            <button class="btn btn-primary" onclick="app.saveMCP(${mcp?.id || 'null'})">${isEdit ? 'Save' : 'Create'}</button>
        `;

        this.showModal(isEdit ? 'Edit MCP Server' : 'New MCP Server', content, actions);
    }

    async saveMCP(mcpId) {
        const form = document.getElementById('mcp-form');
        const data = {
            name: form.querySelector('input[name="name"]').value,
            command: form.querySelector('input[name="command"]').value,
            args: form.querySelector('input[name="args"]').value,
            env: form.querySelector('input[name="env"]').value,
            enabled: form.querySelector('input[name="enabled"]').checked
        };

        try {
            if (mcpId) {
                await this.api('PUT', `/config/mcps/${mcpId}`, data);
            } else {
                await this.api('POST', '/config/mcps', data);
            }
            this.hideModal();
            this.showToast('Success', 'MCP server saved', 'success');
            this.loadConfig();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    editMCP(mcpId) {
        const mcp = this.mcpServers.find(m => m.id === mcpId);
        if (mcp) this.showMCPModal(mcp);
    }

    deleteMCP(mcpId) {
        showConfirmModal(
            'Excluir servidor MCP?',
            'O servidor MCP será removido permanentemente.',
            async () => {
                try {
                    await this.api('DELETE', `/config/mcps/${mcpId}`);
                    this.showToast('Success', 'MCP server deleted', 'success');
                    this.loadConfig();
                } catch (error) {
                    this.showToast('Error', error.message, 'error');
                }
            },
            'Excluir'
        );
    }

    async saveTaskAutoUpdate(enabled) {
        try {
            await this.api('PUT', '/config/settings', {
                task_auto_update_enabled: enabled ? 'true' : 'false'
            });
            this.showToast('Success', `Task auto-update ${enabled ? 'enabled' : 'disabled'}`, 'success');
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    async toggleMCP(mcpId, enabled) {
        const mcp = this.mcpServers.find(m => m.id === mcpId);
        if (mcp) {
            await this.api('PUT', `/config/mcps/${mcpId}`, { ...mcp, enabled });
        }
    }

    async saveTaskAutoEval(enabled) {
        try {
            await this.api('PUT', '/config/settings', {
                task_auto_eval_enabled: enabled ? 'true' : 'false'
            });
            this.showToast('Success', `Task auto-evaluation ${enabled ? 'enabled' : 'disabled'}`, 'success');
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    async saveWhisperSettings() {
        const provider = document.getElementById('whisper-provider').value;
        const openaiKey = document.getElementById('openai-key').value;
        const groqKey = document.getElementById('groq-key').value;

        const settings = {
            whisper_provider: provider
        };
        if (openaiKey) settings.openai_api_key = openaiKey;
        if (groqKey) settings.groq_api_key = groqKey;

        try {
            await this.api('PUT', '/config/settings', settings);
            this.showToast('Success', 'Settings saved', 'success');
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    async saveAISettings() {
        const aiProvider = document.getElementById('ai-provider').value;
        const anthropicKey = document.getElementById('anthropic-key').value;
        const aiModel = document.getElementById('ai-model').value;
        const ollamaURL = document.getElementById('ollama-base-url')?.value;
        const ollamaKey = document.getElementById('ollama-api-key')?.value;
        const ollamaModel = document.getElementById('ollama-model')?.value;

        const settings = {};
        if (aiProvider) settings.ai_provider = aiProvider;
        if (anthropicKey) settings.anthropic_api_key = anthropicKey;
        if (aiModel) settings.ai_model = aiModel;
        if (ollamaURL) settings.ollama_base_url = ollamaURL;
        if (ollamaKey) settings.ollama_api_key = ollamaKey;
        if (ollamaModel) settings.ollama_model = ollamaModel;

        try {
            await this.api('PUT', '/config/settings', settings);
            this.showToast('Success', 'AI settings saved. Restart the service for changes to take effect.', 'success');
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    toggleAISettings() {
        const provider = document.getElementById('ai-provider')?.value;
        const ollamaSettings = document.getElementById('ollama-settings');
        const anthropicKeyGroup = document.getElementById('anthropic-key-group');
        const aiModelGroup = document.getElementById('ai-model-group');

        const isOllama = provider === 'ollama' || provider === 'ollama-sdk';
        if (ollamaSettings) {
            ollamaSettings.style.display = isOllama ? 'block' : 'none';
        }
        if (anthropicKeyGroup) {
            anthropicKeyGroup.style.display = provider === 'apikey' ? 'block' : 'none';
        }
        if (aiModelGroup) {
            aiModelGroup.style.display = isOllama || provider === 'apikey' ? 'none' : 'block';
        }
    }

    onAIProviderChange() {
        this.toggleAISettings();
    }

    async saveMCPHTTPSettings() {
        const enabled = document.getElementById('mcp-http-enabled')?.checked;
        try {
            await this.api('PUT', '/config/settings', {
                mcp_http_enabled: enabled ? 'true' : 'false'
            });
            this.showToast('Success', 'MCP HTTP settings saved. Restart required.', 'success');
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    async loadMCPAPIKeyStatus() {
        try {
            const data = await this.api('GET', '/config/mcp-api-key/status');
            const el = document.getElementById('mcp-api-key-status');
            if (el) {
                el.textContent = data.exists ? `Active: ${data.prefix}` : 'No key configured';
                el.style.color = data.exists ? 'var(--color-success)' : 'var(--color-text-muted)';
            }
        } catch (e) { /* ignore */ }
    }

    async generateMCPAPIKey() {
        try {
            const data = await this.api('POST', '/config/mcp-api-key/generate');
            this.showToast('API Key Generated', `Key: ${data.key}`, 'success');
            this.loadMCPAPIKeyStatus();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    async revokeMCPAPIKey() {
        try {
            await this.api('DELETE', '/config/mcp-api-key');
            this.showToast('Success', 'API key revoked', 'success');
            this.loadMCPAPIKeyStatus();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    async loadToolPolicies() {
        const container = document.getElementById('tp-contexts');
        if (!container) return;

        try {
            const contexts = [
                { key: 'session', label: 'Session (Claude Code sessions)' },
                { key: 'chat', label: 'Chat (AI Assistant)' },
                { key: 'http', label: 'HTTP (External MCP)' },
            ];

            // Fetch tools per context and policies in parallel
            const [mcpTools, chatTools, policies] = await Promise.all([
                this.api('GET', '/config/mcp-tools'),
                this.api('GET', '/config/mcp-tools?context=chat'),
                this.api('GET', '/config/tool-policies'),
            ]);

            this._tpTools = mcpTools; // for project modal (uses MCP tools)
            const toolsByContext = { session: mcpTools, chat: chatTools, http: mcpTools };

            container.innerHTML = contexts.map(ctx => {
                // Sessions default to deny_all (no tools), chat/http default to allow_all
                const defaultMode = ctx.key === 'session' ? 'deny_all' : 'allow_all';
                let policy = { mode: defaultMode, denied: [], allowed: [] };
                try { if (policies[ctx.key]) policy = JSON.parse(policies[ctx.key]); } catch(e) {}
                const denied = new Set(policy.denied || []);
                const allowed = new Set(policy.allowed || []);
                const mode = policy.mode || 'allow_all';
                const tools = toolsByContext[ctx.key];

                const toolCheckboxes = tools.map(t => {
                    let checked;
                    if (mode === 'deny_all') {
                        checked = allowed.has(t.name);
                    } else {
                        checked = !denied.has(t.name);
                    }
                    const shortDesc = t.description.length > 60 ? t.description.slice(0, 60) + '...' : t.description;
                    return `<label class="tp-tool-item" title="${t.description.replace(/"/g, '&quot;')}">
                        <input type="checkbox" data-ctx="${ctx.key}" data-tool="${t.name}" ${checked ? 'checked' : ''}>
                        <span class="tp-tool-name">${t.name}</span>
                        <span class="tp-tool-desc">${shortDesc}</span>
                    </label>`;
                }).join('');

                return `<div class="tp-context-section">
                    <div class="tp-context-header" onclick="app.toggleTPContext('${ctx.key}')">
                        <span class="tp-context-arrow" id="tp-arrow-${ctx.key}">&#9654;</span>
                        <strong>${ctx.label}</strong>
                        <span class="tp-context-summary" id="tp-summary-${ctx.key}"></span>
                    </div>
                    <div class="tp-context-body" id="tp-body-${ctx.key}" style="display:none;">
                        <div style="display: flex; gap: 8px; margin-bottom: 8px; align-items: center;">
                            <button class="btn btn-sm btn-secondary" onclick="app.tpSelectAll('${ctx.key}', true)">Select All</button>
                            <button class="btn btn-sm btn-secondary" onclick="app.tpSelectAll('${ctx.key}', false)">Deselect All</button>
                        </div>
                        <div class="tp-tool-grid">${toolCheckboxes}</div>
                    </div>
                </div>`;
            }).join('');

            // Update summaries
            for (const ctx of contexts) {
                this._updateTPSummary(ctx.key);
            }

            // Listen for checkbox changes to update summary
            container.addEventListener('change', (e) => {
                if (e.target.dataset.ctx) this._updateTPSummary(e.target.dataset.ctx);
            });
        } catch (e) {
            container.innerHTML = '<span style="color: var(--color-text-muted); font-size: 12px;">Failed to load tool policies</span>';
        }
    }

    _updateTPSummary(ctx) {
        const all = document.querySelectorAll(`input[data-ctx="${ctx}"]`);
        const checked = document.querySelectorAll(`input[data-ctx="${ctx}"]:checked`);
        const el = document.getElementById(`tp-summary-${ctx}`);
        if (el) {
            if (checked.length === all.length) {
                el.textContent = 'All tools enabled';
                el.style.color = 'var(--color-success)';
            } else if (checked.length === 0) {
                el.textContent = 'All tools disabled';
                el.style.color = 'var(--color-danger, #e74c3c)';
            } else {
                el.textContent = `${checked.length}/${all.length} tools enabled`;
                el.style.color = 'var(--color-warning, #f39c12)';
            }
        }
    }

    toggleTPContext(ctx) {
        const body = document.getElementById(`tp-body-${ctx}`);
        const arrow = document.getElementById(`tp-arrow-${ctx}`);
        if (!body) return;
        const open = body.style.display !== 'none';
        body.style.display = open ? 'none' : '';
        if (arrow) arrow.innerHTML = open ? '&#9654;' : '&#9660;';
    }

    tpSelectAll(ctx, checked) {
        document.querySelectorAll(`input[data-ctx="${ctx}"]`).forEach(cb => cb.checked = checked);
        this._updateTPSummary(ctx);
    }

    async saveToolPolicies() {
        const policies = {};
        for (const ctx of ['session', 'chat', 'http']) {
            const all = document.querySelectorAll(`input[data-ctx="${ctx}"]`);
            const checked = document.querySelectorAll(`input[data-ctx="${ctx}"]:checked`);

            if (checked.length === all.length) {
                policies[ctx] = JSON.stringify({ mode: 'allow_all' });
            } else if (checked.length === 0) {
                policies[ctx] = JSON.stringify({ mode: 'deny_all' });
            } else {
                // Use the smaller list for efficiency
                if (checked.length <= all.length / 2) {
                    // More denied than allowed -> deny_all + allowed list
                    const allowed = Array.from(checked).map(cb => cb.dataset.tool);
                    policies[ctx] = JSON.stringify({ mode: 'deny_all', allowed });
                } else {
                    // More allowed than denied -> allow_all + denied list
                    const unchecked = document.querySelectorAll(`input[data-ctx="${ctx}"]:not(:checked)`);
                    const denied = Array.from(unchecked).map(cb => cb.dataset.tool);
                    policies[ctx] = JSON.stringify({ mode: 'allow_all', denied });
                }
            }
        }
        try {
            await this.api('PUT', '/config/tool-policies', policies);
            this.showToast('Success', 'Tool policies saved', 'success');
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    async testAIConnection() {
        const resultEl = document.getElementById('ai-test-result');
        if (resultEl) resultEl.innerHTML = '<span style="color: var(--color-text-muted);">Testing...</span>';

        try {
            // Send current form values for testing without saving
            const params = {
                ai_provider: document.getElementById('ai-provider')?.value || '',
                anthropic_api_key: document.getElementById('anthropic-key')?.value || '',
                ai_model: document.getElementById('ai-model')?.value || '',
                ollama_base_url: document.getElementById('ollama-base-url')?.value || '',
                ollama_api_key: document.getElementById('ollama-api-key')?.value || '',
                ollama_model: document.getElementById('ollama-model')?.value || '',
            };
            const resp = await fetch('/api/ai/test-connection', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(params),
            });
            const data = await resp.json();
            if (data.configured) {
                const providerNames = { apikey: 'API Key', gosdk: 'Agent SDK (Go)', nodesdk: 'Agent SDK (Node.js)', claudecode: 'Claude CLI', ollama: 'Ollama (Direct)', 'ollama-sdk': 'Ollama (CLI)' };
                const provider = providerNames[data.provider] || data.provider;
                if (data.error) {
                    if (resultEl) resultEl.innerHTML = `<span style="color: var(--color-danger);">Error: ${data.error}</span>`;
                } else {
                    if (resultEl) resultEl.innerHTML = `<span style="color: var(--color-success);">Connected (${provider}, model: ${data.model})</span>`;
                }
            } else {
                if (resultEl) resultEl.innerHTML = `<span style="color: var(--color-danger);">${data.error || 'Not configured. Select a provider and configure it.'}</span>`;
            }
        } catch (e) {
            if (resultEl) resultEl.innerHTML = `<span style="color: var(--color-danger);">Error: ${e.message}</span>`;
        }
    }

    showAISkillCreator() {
        const modalContent = `
            <div class="form-group">
                <label class="form-label">Describe the skill you want to create</label>
                <textarea id="ai-skill-description" class="form-textarea" rows="4"
                    placeholder="e.g., A skill that enforces Python best practices, including type hints, docstrings, and PEP 8 compliance..."></textarea>
            </div>
            <div id="ai-skill-progress" class="hidden" style="margin-top: 12px;">
                <div style="display: flex; align-items: center; gap: 8px; margin-bottom: 8px;">
                    <div class="spinner-small" style="width: 16px; height: 16px; border: 2px solid var(--color-border); border-top-color: var(--color-primary); border-radius: 50%; animation: spin 0.8s linear infinite;"></div>
                    <span style="font-size: 13px; color: var(--color-text-secondary);">Generating skill...</span>
                </div>
                <div id="ai-skill-preview" style="background: var(--color-bg); border-radius: 8px; padding: 12px; font-size: 12px; max-height: 300px; overflow-y: auto; white-space: pre-wrap; font-family: monospace;"></div>
            </div>
        `;

        this.showModal('Create Skill with AI', modalContent, `
            <button class="btn btn-secondary" onclick="app.closeModal()">Cancel</button>
            <button id="ai-skill-generate-btn" class="btn btn-primary" onclick="app.generateAISkill()">Generate</button>
            <button id="ai-skill-save-btn" class="btn btn-success hidden" onclick="app.saveAIGeneratedSkill()">Save Skill</button>
        `);
    }

    async generateAISkill() {
        const description = document.getElementById('ai-skill-description')?.value?.trim();
        if (!description) {
            this.showToast('Error', 'Please enter a description', 'error');
            return;
        }

        const progressEl = document.getElementById('ai-skill-progress');
        const previewEl = document.getElementById('ai-skill-preview');
        const genBtn = document.getElementById('ai-skill-generate-btn');
        const saveBtn = document.getElementById('ai-skill-save-btn');

        if (progressEl) progressEl.classList.remove('hidden');
        if (genBtn) genBtn.disabled = true;
        if (previewEl) previewEl.textContent = '';

        let fullText = '';

        try {
            const resp = await fetch('/api/ai/generate-skill', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ description }),
            });

            if (!resp.ok) {
                const err = await resp.json();
                throw new Error(err.error || 'Generation failed');
            }

            const reader = resp.body.getReader();
            const decoder = new TextDecoder();
            let buffer = '';

            while (true) {
                const { done, value } = await reader.read();
                if (done) break;

                buffer += decoder.decode(value, { stream: true });
                const lines = buffer.split('\n');
                buffer = lines.pop();

                for (const line of lines) {
                    if (!line.startsWith('data: ')) continue;
                    try {
                        const event = JSON.parse(line.slice(6));
                        if (event.type === 'text') {
                            fullText += event.data.text;
                            if (previewEl) previewEl.textContent = fullText;
                        }
                    } catch (e) { /* skip */ }
                }
            }

            // Store for save
            this._aiGeneratedSkill = fullText;
            if (saveBtn) saveBtn.classList.remove('hidden');
        } catch (e) {
            this.showToast('Error', e.message, 'error');
        } finally {
            if (genBtn) genBtn.disabled = false;
        }
    }

    async saveAIGeneratedSkill() {
        if (!this._aiGeneratedSkill) return;

        try {
            // Try to parse as JSON (the model should return a JSON object)
            let skillData;
            try {
                skillData = JSON.parse(this._aiGeneratedSkill);
            } catch (e) {
                // If not JSON, use raw text as content
                skillData = {
                    name: 'AI Generated Skill',
                    content: this._aiGeneratedSkill,
                    category: 'ai-generated',
                };
            }

            await this.api('POST', '/config/skills', {
                name: skillData.name || 'AI Generated Skill',
                content: skillData.content || this._aiGeneratedSkill,
                enabled: true,
                category: skillData.category || 'ai-generated',
            });

            this.showToast('Success', `Skill "${skillData.name}" created`, 'success');
            this.closeModal();
            this.loadConfig();
        } catch (e) {
            this.showToast('Error', e.message, 'error');
        }
    }
    // ============ Project Tasks ============

    async loadProjectTasks(projectId) {
        const container = document.getElementById('project-tasks-list');
        const summaryEl = document.getElementById('project-tasks-summary');
        if (!container) return;

        try {
            const [tasks, sessionSummaryRaw] = await Promise.all([
                this.api('GET', `/projects/${projectId}/tasks`),
                this.api('GET', `/projects/${projectId}/tasks/session-summary`).catch(() => [])
            ]);
            // Build lookup: taskID -> {session_count, active_count, latest_session}
            this._taskSessionSummary = {};
            (sessionSummaryRaw || []).forEach(s => { this._taskSessionSummary[s.task_id] = s; });

            // Build summary (exclude umbrella tasks — only count leaf tasks)
            if (summaryEl) {
                const parentIds = new Set();
                tasks.filter(t => t.parent_id?.Valid).forEach(t => parentIds.add(t.parent_id.Int64));
                const counts = { todo: 0, in_progress: 0, done: 0, blocked: 0 };
                tasks.filter(t => !parentIds.has(t.id)).forEach(t => { if (counts[t.status] !== undefined) counts[t.status]++; });
                summaryEl.innerHTML = `
                    <span class="summary-item"><span class="badge-todo task-status-badge" style="cursor:default">${counts.todo}</span> todo</span>
                    <span class="summary-item"><span class="badge-in_progress task-status-badge" style="cursor:default">${counts.in_progress}</span> prog</span>
                    <span class="summary-item"><span class="badge-done task-status-badge" style="cursor:default">${counts.done}</span> done</span>
                    ${counts.blocked > 0 ? `<span class="summary-item"><span class="badge-blocked task-status-badge" style="cursor:default">${counts.blocked}</span> block</span>` : ''}
                `;
            }

            if (tasks.length === 0) {
                container.innerHTML = '<div class="task-empty">No tasks yet. Click "+ Add Task" to create one.</div>';
                return;
            }

            // Group by parent
            const topLevel = tasks.filter(t => !t.parent_id?.Valid);
            const children = {};
            tasks.filter(t => t.parent_id?.Valid).forEach(t => {
                const pid = t.parent_id.Int64;
                if (!children[pid]) children[pid] = [];
                children[pid].push(t);
            });

            // Separate active and done top-level tasks
            const activeTasks = topLevel.filter(t => t.status !== 'done');
            const doneTasks = topLevel.filter(t => t.status === 'done');

            let html = '';
            for (const task of activeTasks) {
                const kids = children[task.id] || [];
                const hasChildren = kids.length > 0;
                html += this.renderTaskCard(task, projectId, hasChildren, 0, 0, kids);
                if (hasChildren) {
                    const collapsed = this._collapsedTasks?.has(task.id) ? ' collapsed' : '';
                    html += `<div class="task-subtasks${collapsed}" data-parent-id="${task.id}">`;
                    let subOrder = 1;
                    for (const sub of children[task.id]) {
                        const subIdx = sub.status === 'done' ? 0 : subOrder++;
                        html += this.renderTaskCard(sub, projectId, false, subIdx, task.sort_order);
                    }
                    html += `<button class="btn-add-task" onclick="app.showTaskModal(${projectId}, null, ${task.id})" style="margin-top:4px;padding:4px 8px;font-size:11px;">+ Subtask</button>`;
                    html += '</div>';
                }
            }

            // Done section at the bottom
            if (doneTasks.length > 0) {
                const doneCollapsed = this._doneCollapsed ? ' collapsed' : '';
                html += `
                    <div class="done-section">
                        <div class="done-section-header" onclick="app.toggleDoneSection()">
                            <svg class="done-section-chevron${this._doneCollapsed ? ' collapsed' : ''}" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="6 9 12 15 18 9"/></svg>
                            <span class="done-section-title">Concluídas</span>
                            <span class="done-section-count">${doneTasks.length}</span>
                        </div>
                        <div class="done-section-body${doneCollapsed}">`;
                for (const task of doneTasks) {
                    const kids = children[task.id] || [];
                    const hasChildren = kids.length > 0;
                    html += this.renderTaskCard(task, projectId, hasChildren, 0, 0, kids);
                    if (hasChildren) {
                        const collapsed = this._collapsedTasks?.has(task.id) ? ' collapsed' : '';
                        html += `<div class="task-subtasks${collapsed}" data-parent-id="${task.id}">`;
                        let subOrder = 1;
                        for (const sub of children[task.id]) {
                            html += this.renderTaskCard(sub, projectId, false, subOrder++);
                        }
                        html += '</div>';
                    }
                }
                html += `
                        </div>
                    </div>`;
            }

            container.innerHTML = html;
            this.setupTaskDragAndDrop(projectId);
        } catch (e) {
            container.innerHTML = '<div class="task-empty">Failed to load tasks.</div>';
        }
    }

    renderTaskCard(task, projectId, hasChildren = false, subIndex = 0, parentOrder = 0, childrenList = []) {
        const now = new Date();
        const isOverdue = task.due_date?.Valid && new Date(task.due_date.Time) < now && task.status !== 'done';
        const isDone = task.status === 'done';
        const isSubtask = task.parent_id?.Valid;

        const priorityClass = `priority-${task.priority}`;
        const overdueClass = isOverdue ? ' task-overdue' : '';
        const doneClass = isDone ? ' task-done' : '';

        let dueDateHtml = '';
        if (task.due_date?.Valid) {
            const d = new Date(task.due_date.Time);
            const dateStr = d.toLocaleDateString('pt-BR', { day: '2-digit', month: '2-digit' });
            const timeStr = d.toLocaleTimeString('pt-BR', { hour: '2-digit', minute: '2-digit' });
            const overdueTag = isOverdue ? ' overdue' : '';
            dueDateHtml = `<span class="task-due${overdueTag}">
                <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/></svg>
                ${dateStr} ${timeStr}
            </span>`;
        }

        const descPreview = task.description ? `<span class="task-description-preview">${this.escapeHtml(task.description.substring(0, 80))}</span>` : '';

        // Session indicator
        const sessSummary = this._taskSessionSummary?.[task.id];
        let sessionIndicatorHtml = '';
        if (sessSummary?.active_count > 0) {
            sessionIndicatorHtml = `<span class="task-session-indicator active" title="${sessSummary.active_count} active session(s)">
                <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="4 17 10 11 4 5"/><line x1="12" y1="19" x2="20" y2="19"/></svg>
            </span>`;
        } else if (sessSummary?.session_count > 0) {
            sessionIndicatorHtml = `<span class="task-session-indicator past" title="${sessSummary.session_count} past session(s)">
                <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/></svg>
            </span>`;
        }

        // Start session button (only for non-done tasks)
        const startSessionBtn = !isDone ? `
            <button class="btn-icon btn-start-session" onclick="event.stopPropagation();app.startSessionFromTask(${projectId}, ${task.id})" title="Start Session">
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polygon points="5 3 19 12 5 21 5 3"/></svg>
            </button>` : '';

        // Order number badge (done tasks and subtasks never show order number)
        const hasSubIndex = subIndex > 0 && parentOrder > 0;
        const orderNum = (!isDone && !hasSubIndex && task.sort_order > 0) ? `<span class="task-order-num">${task.sort_order}</span>` : '';
        const subIndexLabel = hasSubIndex ? `<span class="task-sub-index">${parentOrder}.${subIndex}</span>` : '';

        // Collapse button for parent tasks with children
        const collapseBtn = hasChildren ? `
            <button class="btn-icon task-collapse-btn${this._collapsedTasks?.has(task.id) ? ' collapsed' : ''}" onclick="event.stopPropagation();app.toggleTaskCollapse(${task.id})" title="Collapse/Expand">
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="6 9 12 15 18 9"/></svg>
            </button>` : '';

        // Umbrella task badge and progress
        let umbrellaHtml = '';
        if (hasChildren && childrenList.length > 0) {
            const total = childrenList.length;
            const done = childrenList.filter(c => c.status === 'done').length;
            const inProg = childrenList.filter(c => c.status === 'in_progress').length;
            const pct = Math.round((done / total) * 100);
            umbrellaHtml = `<span class="task-umbrella-badge" title="${done}/${total} concluídas, ${inProg} em andamento">${done}/${total}</span>
                <span class="task-umbrella-progress"><span class="task-umbrella-progress-bar" style="width:${pct}%"></span></span>`;
        }

        // Drag handle (desktop) - hidden for done tasks
        const dragHandle = isDone ? '' : `
            <div class="task-drag-handle" title="Drag to reorder">
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                    <circle cx="8" cy="6" r="1.5"/><circle cx="16" cy="6" r="1.5"/>
                    <circle cx="8" cy="12" r="1.5"/><circle cx="16" cy="12" r="1.5"/>
                    <circle cx="8" cy="18" r="1.5"/><circle cx="16" cy="18" r="1.5"/>
                </svg>
            </div>`;

        const umbrellaClass = hasChildren ? ' task-umbrella' : '';

        return `
            <div class="task-card${overdueClass}${doneClass}${umbrellaClass}" data-task-id="${task.id}" data-sort-order="${task.sort_order || 0}" data-project-id="${projectId}">
                ${dragHandle}
                <div class="task-priority-indicator ${priorityClass}"></div>
                <div class="task-card-body">
                    <div class="task-card-top">
                        ${subIndexLabel}
                        ${orderNum}
                        ${collapseBtn}
                        <span class="task-title" onclick="app.viewTaskDetail(${projectId}, ${task.id})" style="cursor:pointer;">${this.escapeHtml(task.title)}</span>
                        ${umbrellaHtml}
                        ${sessionIndicatorHtml}
                        <span class="task-status-badge badge-${task.status}" onclick="app.cycleTaskStatus(${projectId}, ${task.id}, '${task.status}')">${task.status.replace('_', ' ')}</span>
                    </div>
                    <div class="task-card-meta">
                        ${dueDateHtml}
                        ${descPreview}
                    </div>
                </div>
                <div class="task-card-actions">
                    ${startSessionBtn}
                    <button class="btn-icon" onclick="app.showTaskModal(${projectId}, ${task.id})" title="Edit">
                        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/></svg>
                    </button>
                    <button class="btn-icon" onclick="app.duplicateTask(${projectId}, ${task.id})" title="Duplicate">
                        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>
                    </button>
                    <button class="btn-icon" onclick="app.deleteTask(${projectId}, ${task.id})" title="Delete">
                        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>
                    </button>
                </div>
            </div>
        `;
    }

    // Move a task up or down by swapping with its sibling, then persist via API.
    async moveTask(projectId, taskId, direction) {
        // Find the card in any visible container (project or global view)
        const card = document.querySelector(`.task-card[data-task-id="${taskId}"]`);
        if (!card) return;

        const parent = card.parentElement;
        const siblings = Array.from(parent.querySelectorAll(':scope > .task-card'));
        const idx = siblings.indexOf(card);

        let swapCard = null;
        if (direction === 'up' && idx > 0) {
            swapCard = siblings[idx - 1];
            // Move card (and its subtask container) before swap target
            const subtasks = parent.querySelector(`.task-subtasks[data-parent-id="${taskId}"]`);
            const swapSubtasks = parent.querySelector(`.task-subtasks[data-parent-id="${swapCard.dataset.taskId}"]`);
            parent.insertBefore(card, swapCard);
            if (subtasks) parent.insertBefore(subtasks, swapCard);
        } else if (direction === 'down' && idx < siblings.length - 1) {
            swapCard = siblings[idx + 1];
            const subtasks = parent.querySelector(`.task-subtasks[data-parent-id="${taskId}"]`);
            const swapSubtasks = parent.querySelector(`.task-subtasks[data-parent-id="${swapCard.dataset.taskId}"]`);
            // Insert after swap target (skip its subtask container)
            let insertRef = swapCard.nextSibling;
            if (insertRef?.classList?.contains('task-subtasks') && insertRef.dataset.parentId === swapCard.dataset.taskId) {
                insertRef = insertRef.nextSibling;
            }
            parent.insertBefore(card, insertRef);
            if (subtasks) parent.insertBefore(subtasks, card.nextSibling);
        }

        if (!swapCard) return;

        // Determine which container holds these tasks and save accordingly
        const projectContainer = document.getElementById('project-tasks-list');
        const globalContainer = document.getElementById('all-tasks-list');

        if (projectContainer && projectContainer.contains(card)) {
            this.saveTaskReorder(projectId);
        } else if (globalContainer && globalContainer.contains(card)) {
            this._saveAllTasksReorder(globalContainer);
        } else {
            // Fallback: direct API swap
            this.saveTaskReorder(projectId);
        }
    }

    async cycleTaskStatus(projectId, taskId, currentStatus) {
        const cycle = { 'todo': 'in_progress', 'in_progress': 'done', 'done': 'todo', 'blocked': 'todo' };
        const newStatus = cycle[currentStatus] || 'todo';
        try {
            await this.api('PATCH', `/projects/${projectId}/tasks/${taskId}/status`, { status: newStatus });
        } catch (e) {
            this.showToast('Error', e.message, 'error');
        }
    }

    async deleteTask(projectId, taskId) {
        // Fetch project tasks to count subtasks
        let subtaskCount = 0;
        try {
            const tasks = await this.api('GET', `/projects/${projectId}/tasks`);
            subtaskCount = tasks.filter(t => t.parent_id?.Valid && t.parent_id.Int64 === taskId).length;
        } catch (e) { /* proceed without count */ }

        let message = 'A tarefa será removida permanentemente.';
        let confirmLabel = 'Excluir';
        if (subtaskCount > 0) {
            message = `A tarefa e suas <strong>${subtaskCount} subtarefa(s)</strong> serão removidas permanentemente.`;
            confirmLabel = 'Excluir tudo';
        }

        showConfirmModal(
            'Excluir tarefa?',
            message,
            async () => {
                try {
                    await this.api('DELETE', `/projects/${projectId}/tasks/${taskId}`);
                    this.showToast('Sucesso', 'Tarefa excluída', 'success');
                } catch (e) {
                    this.showToast('Erro', e.message, 'error');
                }
            },
            confirmLabel
        );
    }

    async duplicateTask(projectId, taskId) {
        try {
            await this.api('POST', `/projects/${projectId}/tasks/${taskId}/duplicate`);
            this.showToast('Success', 'Task duplicated', 'success');
        } catch (e) {
            this.showToast('Error', e.message, 'error');
        }
    }

    // ============ Task Collapse/Expand ============

    toggleTaskCollapse(taskId) {
        if (!this._collapsedTasks) this._collapsedTasks = new Set();

        const subtaskContainer = document.querySelector(`.task-subtasks[data-parent-id="${taskId}"]`);
        const collapseBtn = subtaskContainer?.previousElementSibling?.querySelector('.task-collapse-btn');

        if (this._collapsedTasks.has(taskId)) {
            this._collapsedTasks.delete(taskId);
            subtaskContainer?.classList.remove('collapsed');
            collapseBtn?.classList.remove('collapsed');
        } else {
            this._collapsedTasks.add(taskId);
            subtaskContainer?.classList.add('collapsed');
            collapseBtn?.classList.add('collapsed');
        }
    }

    // ============ Done Section Collapse/Expand ============

    _doneCollapsed = false;

    toggleDoneSection() {
        this._doneCollapsed = !this._doneCollapsed;
        const body = document.querySelector('.done-section-body');
        const chevron = document.querySelector('.done-section-chevron');
        if (body) body.classList.toggle('collapsed', this._doneCollapsed);
        if (chevron) chevron.classList.toggle('collapsed', this._doneCollapsed);
    }

    // ============ Task Drag-and-Drop Reorder (SortableJS) ============

    _sortableInstances = [];
    _draggedSubtaskContainer = null;
    _wasCollapsed = null;

    setupTaskDragAndDrop(projectId) {
        const container = document.getElementById('project-tasks-list');
        if (!container) return;
        this._initSortable(container, projectId);
    }

    _destroySortables() {
        this._sortableInstances.forEach(s => s.destroy());
        this._sortableInstances = [];
    }

    _initSortable(container, projectId) {
        this._destroySortables();

        const sortableOpts = {
            group: { name: 'tasks', pull: true, put: true },
            animation: 150,
            handle: '.task-drag-handle',
            draggable: '.task-card',
            ghostClass: 'sortable-ghost',
            chosenClass: 'sortable-chosen',
            dragClass: 'sortable-drag',
            delay: 150,
            delayOnTouchOnly: true,
            touchStartThreshold: 5,
            forceFallback: true,
            fallbackOnBody: true,
            swapThreshold: 0.65,
            scrollSensitivity: 60,
            scrollSpeed: 12,
            filter: '.btn-add-task, .task-subtasks, .task-indent-dropzone, .done-section',
            onStart: (evt) => this._onDragStart(evt, container, projectId),
            onEnd: (evt) => this._onDragEnd(evt, container, projectId),
        };

        // Root-level Sortable (top-level tasks)
        const root = new Sortable(container, sortableOpts);
        this._sortableInstances.push(root);

        // Sortable on each subtask container
        container.querySelectorAll('.task-subtasks').forEach(sc => {
            this._initSubtaskSortable(sc, container, projectId);
        });
    }

    _initSubtaskSortable(subtaskContainer, rootContainer, projectId) {
        const sortable = new Sortable(subtaskContainer, {
            group: {
                name: 'tasks',
                pull: true,
                put: (to, from, dragEl) => {
                    // Prevent drop into own subtask container
                    const parentId = to.el.dataset.parentId;
                    if (dragEl.dataset.taskId === parentId) return false;
                    // In global view, prevent cross-project nesting
                    if (!projectId) {
                        const parentCard = to.el.previousElementSibling;
                        if (parentCard?.dataset?.projectId !== dragEl.dataset.projectId) return false;
                    }
                    return true;
                }
            },
            animation: 150,
            handle: '.task-drag-handle',
            draggable: '.task-card',
            ghostClass: 'sortable-ghost',
            chosenClass: 'sortable-chosen',
            dragClass: 'sortable-drag',
            delay: 150,
            delayOnTouchOnly: true,
            touchStartThreshold: 5,
            forceFallback: true,
            fallbackOnBody: true,
            swapThreshold: 0.65,
            filter: '.btn-add-task',
            onStart: (evt) => this._onDragStart(evt, rootContainer, projectId),
            onEnd: (evt) => this._onDragEnd(evt, rootContainer, projectId),
        });
        this._sortableInstances.push(sortable);
    }

    // --- Horizontal drag tracking for indent/outdent ---
    _dragStartX = 0;
    _dragIndent = null; // 'indent' | 'outdent' | null
    _dragPointerHandler = null;

    _onDragStart(evt, container, projectId) {
        // Haptic feedback on mobile
        if (navigator.vibrate) navigator.vibrate(50);

        // Record starting X for horizontal tracking
        const origEvt = evt.originalEvent;
        this._dragStartX = origEvt?.touches?.[0]?.clientX ?? origEvt?.clientX ?? 0;
        this._dragIndent = null;

        // Auto-expand collapsed subtask containers so they can accept drops
        this._wasCollapsed = new Set();
        container.querySelectorAll('.task-subtasks.collapsed').forEach(sc => {
            sc.classList.remove('collapsed');
            this._wasCollapsed.add(sc.dataset.parentId);
            const btn = sc.previousElementSibling?.querySelector('.task-collapse-btn');
            if (btn) btn.classList.remove('collapsed');
        });

        // Hide subtasks of the dragged card to reduce visual noise
        const draggedId = evt.item.dataset.taskId;
        const subtasks = container.querySelector(`.task-subtasks[data-parent-id="${draggedId}"]`);
        if (subtasks) {
            subtasks.classList.add('dragging-parent');
            this._draggedSubtaskContainer = subtasks;
        }

        // Create temporary indent drop zones after each top-level card (except dragged)
        const isInSubtask = evt.item.parentElement.classList.contains('task-subtasks');
        container.querySelectorAll(':scope > .task-card').forEach(card => {
            const taskId = card.dataset.taskId;
            if (taskId === draggedId) return;
            // Skip if this card already has a subtask container
            const existing = container.querySelector(`.task-subtasks[data-parent-id="${taskId}"]`);
            if (existing) return;
            // In global view, only create zone for same project
            if (!projectId && card.dataset.projectId !== evt.item.dataset.projectId) return;

            const zone = document.createElement('div');
            zone.className = 'task-subtasks task-indent-dropzone';
            zone.dataset.parentId = taskId;
            card.parentElement.insertBefore(zone, card.nextSibling);
            this._initSubtaskSortable(zone, container, projectId);
        });

        // Track pointer movement for horizontal feedback
        const INDENT_THRESHOLD = 50;
        this._dragPointerHandler = (e) => {
            const clientX = e.touches?.[0]?.clientX ?? e.clientX ?? 0;
            const deltaX = clientX - this._dragStartX;
            const isCurrentlySubtask = evt.item.parentElement.classList.contains('task-subtasks') || isInSubtask;

            // Clear previous indicators
            container.querySelectorAll('.indent-indicator, .outdent-indicator').forEach(el => {
                el.classList.remove('indent-indicator', 'outdent-indicator');
            });

            if (deltaX > INDENT_THRESHOLD && !isCurrentlySubtask) {
                this._dragIndent = 'indent';
                // Show indent feedback on the fallback clone
                const clone = document.querySelector('.sortable-fallback');
                if (clone) clone.classList.add('indent-indicator');
            } else if (deltaX < -INDENT_THRESHOLD && isCurrentlySubtask) {
                this._dragIndent = 'outdent';
                const clone = document.querySelector('.sortable-fallback');
                if (clone) clone.classList.add('outdent-indicator');
            } else {
                this._dragIndent = null;
            }
        };
        document.addEventListener('touchmove', this._dragPointerHandler, { passive: true });
        document.addEventListener('mousemove', this._dragPointerHandler, { passive: true });
    }

    _onDragEnd(evt, container, projectId) {
        const draggedCard = evt.item;
        const indent = this._dragIndent;

        // Remove pointer tracking
        if (this._dragPointerHandler) {
            document.removeEventListener('touchmove', this._dragPointerHandler);
            document.removeEventListener('mousemove', this._dragPointerHandler);
            this._dragPointerHandler = null;
        }
        this._dragIndent = null;

        try {
            // --- Horizontal hierarchy change (indent/outdent) ---
            if (indent === 'indent') {
                // Find the card above the dragged card in the DOM to use as parent
                let prevEl = draggedCard.previousElementSibling;
                // Skip over subtask containers to find the actual card
                while (prevEl && !prevEl.classList.contains('task-card')) {
                    prevEl = prevEl.previousElementSibling;
                }
                if (prevEl && prevEl.classList.contains('task-card')) {
                    const targetId = prevEl.dataset.taskId;
                    // In global view, check same project
                    if (projectId || prevEl.dataset.projectId === draggedCard.dataset.projectId) {
                        let subtaskContainer = container.querySelector(`.task-subtasks[data-parent-id="${targetId}"]`);
                        if (!subtaskContainer) {
                            subtaskContainer = document.createElement('div');
                            subtaskContainer.className = 'task-subtasks';
                            subtaskContainer.dataset.parentId = targetId;
                            prevEl.parentElement.insertBefore(subtaskContainer, prevEl.nextSibling);
                        }
                        subtaskContainer.appendChild(draggedCard);
                        // Flatten children if dragged card had subtasks
                        if (this._draggedSubtaskContainer) {
                            this._draggedSubtaskContainer.classList.remove('dragging-parent');
                            this._draggedSubtaskContainer.querySelectorAll(':scope > .task-card').forEach(child => {
                                subtaskContainer.appendChild(child);
                            });
                            this._draggedSubtaskContainer.remove();
                            this._draggedSubtaskContainer = null;
                        }
                        if (navigator.vibrate) navigator.vibrate(30);
                    }
                }
            } else if (indent === 'outdent') {
                // Move card out of subtask container to top-level
                const subtasksDiv = draggedCard.closest('.task-subtasks');
                if (subtasksDiv) {
                    const parentEl = subtasksDiv.parentElement;
                    parentEl.insertBefore(draggedCard, subtasksDiv.nextSibling);
                    // Move its subtask container too if it has one
                    if (this._draggedSubtaskContainer) {
                        this._draggedSubtaskContainer.classList.remove('dragging-parent');
                        parentEl.insertBefore(this._draggedSubtaskContainer, draggedCard.nextSibling);
                        this._draggedSubtaskContainer = null;
                    }
                    if (navigator.vibrate) navigator.vibrate(30);
                }
            }

            // --- Handle subtask container of dragged card (non-indent/outdent case) ---
            if (this._draggedSubtaskContainer) {
                this._draggedSubtaskContainer.classList.remove('dragging-parent');
                const newParent = draggedCard.parentElement;

                if (newParent.classList.contains('task-subtasks')) {
                    // Card became a subtask via group drop — flatten its children
                    this._draggedSubtaskContainer.querySelectorAll(':scope > .task-card').forEach(child => {
                        newParent.appendChild(child);
                    });
                    this._draggedSubtaskContainer.remove();
                } else {
                    // Card is still top-level — move subtask container to follow it
                    newParent.insertBefore(this._draggedSubtaskContainer, draggedCard.nextSibling);
                }
                this._draggedSubtaskContainer = null;
            }

            // Re-collapse previously collapsed containers
            if (this._wasCollapsed) {
                this._wasCollapsed.forEach(parentId => {
                    const sc = container.querySelector(`.task-subtasks[data-parent-id="${parentId}"]`);
                    if (sc) {
                        sc.classList.add('collapsed');
                        const btn = sc.previousElementSibling?.querySelector('.task-collapse-btn');
                        if (btn) btn.classList.add('collapsed');
                    }
                });
                this._wasCollapsed = null;
            }
        } finally {
            // ALWAYS clean up — even if errors occurred above
            // Remove any stuck fallback clones (fixes ghost card bug)
            document.querySelectorAll('.sortable-fallback').forEach(el => el.remove());

            // Remove temporary indent drop zones
            container.querySelectorAll('.task-indent-dropzone').forEach(sc => {
                if (!sc.querySelector('.task-card')) {
                    sc.remove();
                } else {
                    // Drop zone received a card — promote it to real subtask container
                    sc.classList.remove('task-indent-dropzone');
                }
            });

            // Remove empty subtask containers
            container.querySelectorAll('.task-subtasks').forEach(sc => {
                if (!sc.querySelector('.task-card') && !sc.querySelector('.btn-add-task')) sc.remove();
            });

            // Clear visual indicators
            container.querySelectorAll('.indent-indicator, .outdent-indicator').forEach(el => {
                el.classList.remove('indent-indicator', 'outdent-indicator');
            });
        }

        // Save via existing API
        if (projectId) {
            this.saveTaskReorder(projectId);
        } else {
            this._saveAllTasksReorder(container);
        }
    }

    async saveTaskReorder(projectId) {
        const container = document.getElementById('project-tasks-list');
        if (!container) return;

        const reorderData = [];
        let sortOrder = 1;

        // Top-level cards (direct children of container)
        container.querySelectorAll(':scope > .task-card').forEach(card => {
            reorderData.push({
                id: parseInt(card.dataset.taskId),
                sort_order: sortOrder++,
                parent_id: null,
            });
        });

        // Subtask containers (only direct children, not inside done-section)
        container.querySelectorAll(':scope > .task-subtasks').forEach(subtaskContainer => {
            const parentCard = subtaskContainer.previousElementSibling;
            if (!parentCard?.classList.contains('task-card')) return;
            const parentId = parseInt(parentCard.dataset.taskId);

            subtaskContainer.querySelectorAll(':scope > .task-card').forEach(card => {
                reorderData.push({
                    id: parseInt(card.dataset.taskId),
                    sort_order: sortOrder++,
                    parent_id: parentId,
                });
            });
        });

        if (reorderData.length === 0) return;

        try {
            await this.api('PUT', `/projects/${projectId}/tasks/reorder`, reorderData);
        } catch (e) {
            this.showToast('Error', 'Failed to reorder tasks', 'error');
            this.loadProjectTasks(projectId);
        }
    }

    showTaskModal(projectId, taskId = null, parentId = null) {
        const isEdit = taskId !== null;
        const title = isEdit ? 'Edit Task' : (parentId ? 'Add Subtask' : 'Add Task');

        const loadAndShow = async () => {
            let task = null;
            if (isEdit) {
                try {
                    task = await this.api('GET', `/projects/${projectId}/tasks/${taskId}`);
                } catch (e) {
                    this.showToast('Error', 'Failed to load task', 'error');
                    return;
                }
            }

            const dueDateValue = task?.due_date?.Valid ? new Date(task.due_date.Time).toISOString().slice(0, 16) : '';

            // Load session history for existing tasks
            let sessionHistoryHtml = '';
            if (isEdit) {
                try {
                    const sessions = await this.api('GET', `/projects/${projectId}/tasks/${taskId}/sessions`);
                    if (sessions && sessions.length > 0) {
                        const sessItems = sessions.map(s => {
                            const date = new Date(s.start_time).toLocaleDateString('pt-BR', { day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' });
                            const isActive = s.status === 'running' || s.status === 'starting';
                            const badge = isActive ? '<span class="badge badge-running">running</span>' : `<span class="badge badge-${s.status}">${s.status}</span>`;
                            const nameLabel = s.name ? this.escapeHtml(s.name) : s.id.substring(0, 8);
                            const clickAction = isActive ? `onclick="app.hideModal();app.openTerminal('${s.id}')"` : '';
                            return `<div class="task-session-item ${isActive ? 'active' : ''}" ${clickAction} style="${isActive ? 'cursor:pointer' : ''}">
                                ${badge} <span class="task-session-name">${nameLabel}</span> <span class="task-session-date">${date}</span>
                            </div>`;
                        }).join('');
                        sessionHistoryHtml = `
                            <div class="form-group">
                                <label class="form-label">Sessions</label>
                                <div class="task-session-history">${sessItems}</div>
                            </div>`;
                    }
                } catch (e) { /* ignore */ }
            }

            const content = `
                <form id="task-form">
                    <div class="form-group">
                        <label class="form-label">Title</label>
                        <input type="text" class="form-input" name="title" value="${this.escapeHtml(task?.title || '')}" required placeholder="Task title...">
                    </div>
                    <div class="form-group">
                        <div class="hook-input-label-row">
                            <label class="form-label" style="margin-bottom:0">Description</label>
                            <button type="button" class="btn-icon btn-sm hook-voice-btn" id="task-description-voice-btn" title="Voice input">
                                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                                    <path d="M12 1a3 3 0 0 0-3 3v8a3 3 0 0 0 6 0V4a3 3 0 0 0-3-3z"></path>
                                    <path d="M19 10v2a7 7 0 0 1-14 0v-2"></path>
                                    <line x1="12" y1="19" x2="12" y2="23"></line>
                                    <line x1="8" y1="23" x2="16" y2="23"></line>
                                </svg>
                            </button>
                        </div>
                        <textarea class="form-input" name="description" rows="3" placeholder="Optional description...">${this.escapeHtml(task?.description || '')}</textarea>
                    </div>
                    <div class="form-row">
                        <div class="form-group">
                            <label class="form-label">Status</label>
                            <select class="form-input" name="status">
                                <option value="todo" ${task?.status === 'todo' ? 'selected' : ''}>Todo</option>
                                <option value="in_progress" ${task?.status === 'in_progress' ? 'selected' : ''}>In Progress</option>
                                <option value="done" ${task?.status === 'done' ? 'selected' : ''}>Done</option>
                                <option value="blocked" ${task?.status === 'blocked' ? 'selected' : ''}>Blocked</option>
                            </select>
                        </div>
                        <div class="form-group">
                            <label class="form-label">Priority</label>
                            <select class="form-input" name="priority">
                                <option value="low" ${task?.priority === 'low' ? 'selected' : ''}>Low</option>
                                <option value="medium" ${(task?.priority === 'medium' || !task) ? 'selected' : ''}>Medium</option>
                                <option value="high" ${task?.priority === 'high' ? 'selected' : ''}>High</option>
                                <option value="urgent" ${task?.priority === 'urgent' ? 'selected' : ''}>Urgent</option>
                            </select>
                        </div>
                    </div>
                    <div class="form-group">
                        <label class="form-label">Due Date</label>
                        <input type="datetime-local" class="form-input" name="due_date" value="${dueDateValue}">
                    </div>
                    ${sessionHistoryHtml}
                </form>
            `;

            const aiBtn = !isEdit ? `<button class="btn btn-primary" style="background:var(--color-ai,#8b5cf6)" onclick="app.createTaskWithAI(${projectId}, ${parentId})">&#10022; Criar com IA</button>` : '';

            const actions = `
                <button class="btn btn-secondary" onclick="app.hideModal()">Cancel</button>
                ${aiBtn}
                <button class="btn btn-primary" onclick="app.saveTask(${projectId}, ${taskId}, ${parentId})">
                    ${isEdit ? 'Update' : 'Create'}
                </button>
            `;

            this.showModal(title, content, actions);

            // Setup voice input for description field
            const voiceBtn = document.getElementById('task-description-voice-btn');
            const descTextarea = document.querySelector('#task-form textarea[name="description"]');
            if (voiceBtn && descTextarea && window.voiceInput) {
                voiceBtn.addEventListener('click', (e) => {
                    e.preventDefault();
                    e.stopPropagation();
                    if (window.voiceInput.isRecording) {
                        window.voiceInput.stopRecording();
                        voiceBtn.classList.remove('recording');
                        return;
                    }
                    voiceBtn.classList.add('recording');
                    window.voiceInput.startRecordingWithCallback((text) => {
                        voiceBtn.classList.remove('recording');
                        descTextarea.value = (descTextarea.value ? descTextarea.value + ' ' : '') + text;
                        descTextarea.focus();
                    });
                });
            }
        };

        loadAndShow();
    }

    async viewTaskDetail(projectId, taskId) {
        // Track which task detail is being viewed for WebSocket updates
        this._viewingTaskDetail = { projectId, taskId };

        try {
            const [task, sessions, allTasks, history] = await Promise.all([
                this.api('GET', `/projects/${projectId}/tasks/${taskId}`),
                this.api('GET', `/projects/${projectId}/tasks/${taskId}/sessions`).catch(() => []),
                this.api('GET', `/projects/${projectId}/tasks`).catch(() => []),
                this.api('GET', `/projects/${projectId}/tasks/${taskId}/history?limit=50`).catch(() => [])
            ]);

            // Find subtasks of this task
            const subtasks = (allTasks || []).filter(t => t.parent_id?.Valid && t.parent_id.Int64 === taskId);
            const isUmbrella = subtasks.length > 0;

            let md = '';

            // Status & Priority
            const statusLabel = (task.status || 'todo').replace('_', ' ');
            const priorityLabel = task.priority || 'medium';
            if (isUmbrella) {
                const doneCount = subtasks.filter(s => s.status === 'done').length;
                md += `**Status:** ${statusLabel} &nbsp;|&nbsp; **Prioridade:** ${priorityLabel} &nbsp;|&nbsp; **Umbrella:** ${doneCount}/${subtasks.length} concluídas\n\n`;
            } else {
                md += `**Status:** ${statusLabel} &nbsp;|&nbsp; **Prioridade:** ${priorityLabel}\n\n`;
            }

            // Due date
            if (task.due_date?.Valid) {
                const d = new Date(task.due_date.Time);
                const dateStr = d.toLocaleDateString('pt-BR', { day: '2-digit', month: '2-digit', year: 'numeric', hour: '2-digit', minute: '2-digit' });
                const isOverdue = d < new Date() && task.status !== 'done';
                md += `**Prazo:** ${dateStr}${isOverdue ? ' **(ATRASADO)**' : ''}\n\n`;
            }

            // Description
            if (task.description) {
                md += `---\n\n${task.description}\n\n`;
            } else {
                md += `---\n\n*Sem descrição.*\n\n`;
            }

            // Subtasks section
            if (isUmbrella) {
                const statusIcons = { done: '[x]', in_progress: '[~]', todo: '[ ]', blocked: '[!]' };
                md += `---\n\n### Subtarefas\n\n`;
                for (const sub of subtasks) {
                    const icon = statusIcons[sub.status] || '[ ]';
                    const subStatus = sub.status.replace('_', ' ');
                    let subDue = '';
                    if (sub.due_date?.Valid) {
                        const sd = new Date(sub.due_date.Time);
                        const isSubOverdue = sd < new Date() && sub.status !== 'done';
                        subDue = ` — prazo: ${sd.toLocaleDateString('pt-BR', { day: '2-digit', month: '2-digit' })}${isSubOverdue ? ' **(ATRASADO)**' : ''}`;
                    }
                    md += `- ${icon} **${this.escapeHtml(sub.title)}** *(${subStatus})*${subDue}\n`;
                }
                md += '\n';
            }

            // Session history
            const activeSession = sessions?.find(s => s.status === 'running' || s.status === 'starting');
            if (sessions && sessions.length > 0) {
                md += `---\n\n### Sessões\n\n`;
                for (const s of sessions) {
                    const date = new Date(s.start_time).toLocaleDateString('pt-BR', { day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' });
                    const name = s.name || s.id.substring(0, 8);
                    const isActive = s.status === 'running' || s.status === 'starting';
                    const badge = isActive ? '🟢 running' : s.status;
                    const sessionId = s.id.substring(0, 8);
                    md += `- **${this.escapeHtml(name)}** \`${sessionId}\` — ${badge} — ${date}\n`;
                }
            }

            // History timeline
            if (history && history.length > 0) {
                md += `---\n\n### Historico\n\n`;
                for (const entry of history) {
                    const icon = this.taskHistoryIcon(entry.event_type);
                    const label = this.taskHistoryLabel(entry);
                    const time = this.relativeTime(entry.created_at);
                    md += `${icon} ${label} — *${time}*\n\n`;
                }
            }

            // Project name
            const project = (this.projects || []).find(p => p.id === projectId);
            if (project) {
                md += `\n---\n\n*Projeto: ${this.escapeHtml(project.name)}*\n`;
            }

            // Action buttons
            const isDone = task.status === 'done';
            const actions = [
                {
                    label: 'Editar',
                    class: 'btn btn-secondary',
                    onClick: () => {
                        window.docViewer.close();
                        this.showTaskModal(projectId, taskId);
                    }
                }
            ];

            if (!isDone) {
                const cycle = { 'todo': 'in_progress', 'in_progress': 'done', 'blocked': 'todo' };
                const next = cycle[task.status] || 'in_progress';
                const nextLabel = next.replace('_', ' ');
                actions.push({
                    label: `Marcar ${nextLabel}`,
                    class: 'btn btn-primary',
                    onClick: async () => {
                        try {
                            await this.api('PATCH', `/projects/${projectId}/tasks/${taskId}/status`, { status: next });
                            window.docViewer.close();
                            this.showToast('Sucesso', `Tarefa marcada como ${nextLabel}`, 'success');
                            this.loadAllTasks();
                        } catch (e) {
                            this.showToast('Erro', e.message, 'error');
                        }
                    }
                });

                if (activeSession) {
                    actions.push({
                        label: 'Ir para Sessão',
                        class: 'btn btn-success',
                        onClick: () => {
                            window.docViewer.close();
                            this.openTerminal(activeSession.id);
                        }
                    });
                } else {
                    actions.push({
                        label: 'Iniciar Sessão',
                        class: 'btn btn-success',
                        onClick: () => {
                            window.docViewer.close();
                            this.startSessionFromTask(projectId, taskId);
                        }
                    });
                }
            }

            // "Add Note" button
            actions.push({
                label: 'Adicionar Nota',
                class: 'btn btn-secondary',
                onClick: async () => {
                    const comment = prompt('Adicionar nota:');
                    if (comment && comment.trim()) {
                        try {
                            await this.api('POST', `/projects/${projectId}/tasks/${taskId}/history`, { comment: comment.trim() });
                            this.viewTaskDetail(projectId, taskId);
                        } catch (e) {
                            this.showToast('Erro', e.message, 'error');
                        }
                    }
                }
            });

            // "Discuss with AI" button — always visible regardless of status
            actions.push({
                label: '💬 Discutir com IA',
                class: 'btn btn-primary',
                onClick: async () => {
                    window.docViewer.close();
                    await this.discussTaskWithAI(projectId, taskId);
                }
            });

            window.docViewer.openWithContent(task.title, md, {
                actions,
                onClose: () => { this._viewingTaskDetail = null; }
            });
        } catch (e) {
            this.showToast('Erro', 'Falha ao carregar detalhes da tarefa', 'error');
        }
    }

    relativeTime(dateStr) {
        const now = new Date();
        const date = new Date(dateStr);
        const diffMs = now - date;
        const diffSec = Math.floor(diffMs / 1000);
        const diffMin = Math.floor(diffSec / 60);
        const diffHour = Math.floor(diffMin / 60);
        const diffDay = Math.floor(diffHour / 24);

        if (diffSec < 60) return 'agora';
        if (diffMin < 60) return `${diffMin}m atras`;
        if (diffHour < 24) return `${diffHour}h atras`;
        if (diffDay < 7) return `${diffDay}d atras`;
        return date.toLocaleDateString('pt-BR', { day: '2-digit', month: '2-digit', year: 'numeric', hour: '2-digit', minute: '2-digit' });
    }

    taskHistoryIcon(eventType) {
        const icons = {
            task_created: '**[+]**',
            status_change: '**[~]**',
            priority_change: '**[!]**',
            title_updated: '**[T]**',
            description_updated: '**[D]**',
            due_date_changed: '**[C]**',
            session_linked: '**[L]**',
            session_started: '**[>]**',
            session_ended: '**[x]**',
            suggestion_accepted: '**[A]**',
            suggestion_dismissed: '**[-]**',
            comment_added: '**[N]**',
            task_assigned: '**[=]**',
            parent_changed: '**[P]**',
        };
        return icons[eventType] || '**[?]**';
    }

    taskHistoryLabel(entry) {
        let details = {};
        try { details = JSON.parse(entry.details || '{}'); } catch {}

        const labels = {
            task_created: () => `Tarefa criada (${details.status || 'todo'}, ${details.priority || 'medium'})`,
            status_change: () => {
                let label = `Status: ${details.old} → ${details.new}`;
                if (details.reason === 'auto_on_link') label += ' (auto)';
                if (details.reason === 'auto_update') label += ' (auto-update)';
                return label;
            },
            priority_change: () => `Prioridade: ${details.old} → ${details.new}`,
            title_updated: () => `Titulo atualizado`,
            description_updated: () => `Descricao atualizada`,
            due_date_changed: () => `Prazo alterado`,
            session_linked: () => `Sessao vinculada: ${details.session_name || details.session_id || ''}`,
            session_started: () => `Sessao iniciada`,
            session_ended: () => details.summary ? `Sessao encerrada — ${details.summary}` : `Sessao encerrada`,
            suggestion_accepted: () => `Sugestao aceita: ${details.title || details.suggestion_type || ''}`,
            suggestion_dismissed: () => `Sugestao ignorada: ${details.title || details.suggestion_type || ''}`,
            comment_added: () => details.comment || 'Nota adicionada',
            task_assigned: () => `Tarefa atribuida a sessao`,
            parent_changed: () => `Tarefa pai alterada`,
        };

        const fn = labels[entry.event_type];
        return fn ? fn() : entry.event_type;
    }

    async discussTaskWithAI(projectId, taskId) {
        if (!window.aiChat) {
            this.showToast('AI Chat não disponível', '', 'error');
            return;
        }
        try {
            const result = await this.api('POST', '/ai/initiate-task-discussion', {
                project_id: projectId,
                task_id: taskId,
            });
            if (result?.conversation_id) {
                window.aiChat.open();
                window.aiChat.loadConversation(result.conversation_id);
            }
        } catch (e) {
            this.showToast('Erro', 'Falha ao iniciar discussão com IA', 'error');
        }
    }

    async saveTask(projectId, taskId, parentId) {
        const form = document.getElementById('task-form');
        if (!form) return;

        const formData = new FormData(form);
        const data = {
            title: formData.get('title'),
            description: formData.get('description'),
            status: formData.get('status'),
            priority: formData.get('priority'),
            due_date: formData.get('due_date') || '',
        };

        if (parentId) {
            data.parent_id = parentId;
        }

        try {
            if (taskId) {
                await this.api('PUT', `/projects/${projectId}/tasks/${taskId}`, data);
                this.showToast('Success', 'Task updated', 'success');
            } else {
                await this.api('POST', `/projects/${projectId}/tasks`, data);
                this.showToast('Success', 'Task created', 'success');
            }
            this.hideModal();
        } catch (e) {
            this.showToast('Error', e.message, 'error');
        }
    }

    async createTaskWithAI(projectId, parentId) {
        const form = document.getElementById('task-form');
        if (!form) return;

        const formData = new FormData(form);
        const payload = {
            project_id: projectId,
            title: formData.get('title') || '',
            description: formData.get('description') || '',
            status: formData.get('status') || 'todo',
            priority: formData.get('priority') || 'medium',
            due_date: formData.get('due_date') || '',
        };
        if (parentId) {
            payload.parent_id = parentId;
        }

        this.hideModal();

        try {
            const result = await this.api('POST', '/ai/initiate-task-creation', payload);
            if (result?.conversation_id) {
                window.aiChat.open();
                window.aiChat.loadConversation(result.conversation_id);
            }
        } catch (e) {
            this.showToast('Erro ao iniciar criação com IA', e.message, 'error');
        }
    }

    // ============ Session-Task Integration ============

    showSessionReopenChoiceModal() {
        return new Promise((resolve) => {
            document.querySelector('.confirm-modal-overlay')?.remove();
            const overlay = document.createElement('div');
            overlay.className = 'confirm-modal-overlay';
            overlay.innerHTML = `
                <div class="confirm-modal">
                    <div class="confirm-modal-title">Previous Session Found</div>
                    <div class="confirm-modal-message">This task has a previous session that was closed. What would you like to do?</div>
                    <div class="confirm-modal-actions" style="gap:8px;">
                        <button class="confirm-modal-cancel">Cancel</button>
                        <button class="btn btn-secondary" data-choice="new">New Session</button>
                        <button class="btn btn-success" data-choice="reopen">Reopen</button>
                    </div>
                </div>
            `;
            const finish = (val) => { overlay.remove(); resolve(val); };
            overlay.querySelector('.confirm-modal-cancel').addEventListener('click', () => finish(null));
            overlay.addEventListener('click', (e) => { if (e.target === overlay) finish(null); });
            overlay.querySelector('[data-choice="reopen"]').addEventListener('click', () => finish('reopen'));
            overlay.querySelector('[data-choice="new"]').addEventListener('click', () => finish('new'));
            document.body.appendChild(overlay);
        });
    }

    async startSessionFromTask(projectId, taskId) {
        try {
            const sessionSummary = await this.api('GET', `/projects/${projectId}/tasks/session-summary`).catch(() => []);
            const taskSummary = (sessionSummary || []).find(s => s.task_id === taskId);

            if (taskSummary?.active_count > 0) {
                this.openTerminal(taskSummary.latest_session);
                return;
            }

            if (taskSummary?.stopped_count > 0 && taskSummary.latest_stopped_session) {
                const choice = await this.showSessionReopenChoiceModal();
                if (!choice) return;
                if (choice === 'reopen') {
                    const sess = await this.api('POST', `/sessions/${taskSummary.latest_stopped_session}/reopen`);
                    this.showToast('Success', 'Session reopened', 'success');
                    this.openTerminal(sess.id, sess, sess.name);
                    return;
                }
                // choice === 'new' — fall through to create new session
            }

            const task = await this.api('GET', `/projects/${projectId}/tasks/${taskId}`);
            const defaultName = `Task: ${task.title}`;
            const customName = await this.showSessionNameModal(defaultName);
            if (!customName) return;

            const session = await this.api('POST', '/sessions', {
                project_id: projectId,
                task_id: taskId,
                name: customName
            });

            // Mark this session as recently created (protect from restoreTabsFromStorage)
            this.recentlyCreatedSessions.add(session.id);
            setTimeout(() => this.recentlyCreatedSessions.delete(session.id), 5000);

            this.showToast('Success', 'Session started from task', 'success');
            this.openTerminal(session.id, session, customName);
        } catch (e) {
            this.showToast('Error', e.message, 'error');
        }
    }

    // ============ All Tasks (Cross-Project) ============

    async loadAllTasks() {
        const container = document.getElementById('all-tasks-list');
        const statsEl = document.getElementById('all-tasks-stats');
        if (!container) return;

        const params = new URLSearchParams();
        const status = document.getElementById('filter-status')?.value;
        const priority = document.getElementById('filter-priority')?.value;
        const projectId = document.getElementById('filter-project')?.value;
        const search = document.getElementById('filter-search')?.value;

        if (status) params.set('status', status);
        if (priority) params.set('priority', priority);
        if (projectId) params.set('project_id', projectId);
        if (search) params.set('search', search);

        const queryStr = params.toString() ? `?${params.toString()}` : '';

        try {
            const [data, sessionSummaryRaw] = await Promise.all([
                this.api('GET', `/tasks${queryStr}`),
                this.api('GET', '/tasks/session-summary').catch(() => [])
            ]);
            const tasks = data.tasks || [];
            const summary = data.summary || {};
            this._taskSessionSummary = {};
            (sessionSummaryRaw || []).forEach(s => { this._taskSessionSummary[s.task_id] = s; });

            if (statsEl) {
                const total = Object.values(summary).reduce((a, b) => a + b, 0);
                const activeStatus = document.getElementById('filter-status')?.value || '';
                statsEl.innerHTML = `
                    <div class="all-tasks-stat-cards">
                        <div class="stat-card stat-total${activeStatus === '' ? ' active' : ''}" data-filter="" onclick="app.filterByStatus('')"><span class="stat-number">${total}</span><span class="stat-label">Total</span></div>
                        <div class="stat-card stat-todo${activeStatus === 'todo' ? ' active' : ''}" data-filter="todo" onclick="app.filterByStatus('todo')"><span class="stat-number">${summary.todo || 0}</span><span class="stat-label">Todo</span></div>
                        <div class="stat-card stat-progress${activeStatus === 'in_progress' ? ' active' : ''}" data-filter="in_progress" onclick="app.filterByStatus('in_progress')"><span class="stat-number">${summary.in_progress || 0}</span><span class="stat-label">In Progress</span></div>
                        <div class="stat-card stat-done${activeStatus === 'done' ? ' active' : ''}" data-filter="done" onclick="app.filterByStatus('done')"><span class="stat-number">${summary.done || 0}</span><span class="stat-label">Done</span></div>
                        <div class="stat-card stat-blocked${activeStatus === 'blocked' ? ' active' : ''}" data-filter="blocked" onclick="app.filterByStatus('blocked')"><span class="stat-number">${summary.blocked || 0}</span><span class="stat-label">Blocked</span></div>
                    </div>
                `;
            }

            this._populateProjectFilter();

            if (tasks.length === 0) {
                container.innerHTML = '<div class="empty-state">No tasks match your filters.</div>';
                return;
            }

            const projectMap = {};
            (this.projects || []).forEach(p => { projectMap[p.id] = p.name; });

            // Group by parent for hierarchy display
            const topLevel = tasks.filter(t => !t.parent_id?.Valid);
            const children = {};
            tasks.filter(t => t.parent_id?.Valid).forEach(t => {
                const pid = t.parent_id.Int64;
                if (!children[pid]) children[pid] = [];
                children[pid].push(t);
            });

            // Separate active and done top-level tasks
            const activeTopLevel = topLevel.filter(t => t.status !== 'done');
            const doneTopLevel = topLevel.filter(t => t.status === 'done');

            // Render active tasks with sequential global numbering
            let html = '';
            const renderedIds = new Set();
            let globalOrder = 1;
            for (const task of activeTopLevel) {
                const projectName = projectMap[task.project_id] || `Project #${task.project_id}`;
                const kids = children[task.id] || [];
                const hasKids = kids.length > 0;
                const currentOrder = globalOrder++;
                html += this.renderAllTaskCard(task, projectName, hasKids, 0, 0, currentOrder, kids);
                renderedIds.add(task.id);
                if (hasKids) {
                    const collapsed = this._collapsedTasks?.has(task.id) ? ' collapsed' : '';
                    html += `<div class="task-subtasks all-tasks-subtasks${collapsed}" data-parent-id="${task.id}">`;
                    let subOrder = 1;
                    for (const sub of children[task.id]) {
                        const subProjectName = projectMap[sub.project_id] || `Project #${sub.project_id}`;
                        const subIdx = sub.status === 'done' ? 0 : subOrder++;
                        html += this.renderAllTaskCard(sub, subProjectName, false, subIdx, currentOrder);
                        renderedIds.add(sub.id);
                    }
                    html += '</div>';
                }
            }

            // Render any orphan subtasks whose parent wasn't in the filtered results
            for (const task of tasks) {
                if (!renderedIds.has(task.id) && task.status !== 'done') {
                    const projectName = projectMap[task.project_id] || `Project #${task.project_id}`;
                    html += this.renderAllTaskCard(task, projectName, false, 0, 0, globalOrder++);
                    renderedIds.add(task.id);
                }
            }

            // Done section at the bottom (includes done top-level and orphan done subtasks)
            const allDone = [...doneTopLevel];
            doneTopLevel.forEach(t => renderedIds.add(t.id));
            for (const task of tasks) {
                if (!renderedIds.has(task.id) && task.status === 'done') {
                    allDone.push(task);
                }
            }
            if (allDone.length > 0) {
                const doneCollapsed = this._doneCollapsed ? ' collapsed' : '';
                html += `
                    <div class="done-section">
                        <div class="done-section-header" onclick="app.toggleDoneSection()">
                            <svg class="done-section-chevron${this._doneCollapsed ? ' collapsed' : ''}" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="6 9 12 15 18 9"/></svg>
                            <span class="done-section-title">Concluídas</span>
                            <span class="done-section-count">${allDone.length}</span>
                        </div>
                        <div class="done-section-body${doneCollapsed}">`;
                for (const task of allDone) {
                    const projectName = projectMap[task.project_id] || `Project #${task.project_id}`;
                    const kids = children[task.id] || [];
                    const hasKids = kids.length > 0;
                    html += this.renderAllTaskCard(task, projectName, hasKids, 0, 0, 0, kids);
                    renderedIds.add(task.id);
                    if (hasKids) {
                        const collapsed = this._collapsedTasks?.has(task.id) ? ' collapsed' : '';
                        html += `<div class="task-subtasks all-tasks-subtasks${collapsed}" data-parent-id="${task.id}">`;
                        let subOrder = 1;
                        for (const sub of children[task.id]) {
                            const subProjectName = projectMap[sub.project_id] || `Project #${sub.project_id}`;
                            html += this.renderAllTaskCard(sub, subProjectName, false, subOrder++);
                            renderedIds.add(sub.id);
                        }
                        html += '</div>';
                    }
                }
                html += `
                        </div>
                    </div>`;
            }

            container.innerHTML = html;
            this.setupAllTasksDragAndDrop(container);
        } catch (e) {
            container.innerHTML = '<div class="empty-state">Failed to load tasks.</div>';
        }
    }

    renderAllTaskCard(task, projectName, hasChildren = false, subIndex = 0, parentOrder = 0, overrideOrder = 0, childrenList = []) {
        const now = new Date();
        const hasDue = task.due_date?.Valid;
        const isOverdue = hasDue && new Date(task.due_date.Time) < now && task.status !== 'done';
        const isDone = task.status === 'done';

        const priorityClass = `priority-${task.priority}`;
        const overdueClass = isOverdue ? ' task-overdue' : '';
        const doneClass = isDone ? ' task-done' : '';

        let dueDateHtml = '';
        if (hasDue) {
            const d = new Date(task.due_date.Time);
            const dateStr = d.toLocaleDateString('pt-BR', { day: '2-digit', month: '2-digit' });
            const timeStr = d.toLocaleTimeString('pt-BR', { hour: '2-digit', minute: '2-digit' });
            const overdueTag = isOverdue ? ' overdue' : '';
            dueDateHtml = `<span class="task-due${overdueTag}">
                <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/></svg>
                ${dateStr} ${timeStr}
            </span>`;
        }

        const descPreview = task.description ? `<span class="task-description-preview">${this.escapeHtml(task.description.substring(0, 80))}</span>` : '';
        const projectBadge = `<span class="task-project-badge" onclick="event.stopPropagation();app.showProjectDetail(${task.project_id})" title="Go to project">${this.escapeHtml(projectName)}</span>`;

        // Session indicator
        const sessSummary = this._taskSessionSummary?.[task.id];
        let sessionIndicatorHtml = '';
        if (sessSummary?.active_count > 0) {
            sessionIndicatorHtml = `<span class="task-session-indicator active" title="${sessSummary.active_count} active session(s)">
                <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="4 17 10 11 4 5"/><line x1="12" y1="19" x2="20" y2="19"/></svg>
            </span>`;
        } else if (sessSummary?.session_count > 0) {
            sessionIndicatorHtml = `<span class="task-session-indicator past" title="${sessSummary.session_count} past session(s)">
                <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/></svg>
            </span>`;
        }

        const startSessionBtn = !isDone ? `
            <button class="btn-icon btn-start-session" onclick="event.stopPropagation();app.startSessionFromTask(${task.project_id}, ${task.id})" title="Start Session">
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polygon points="5 3 19 12 5 21 5 3"/></svg>
            </button>` : '';

        // Order number badge (done tasks and subtasks never show order number)
        const hasSubIndex = subIndex > 0 && parentOrder > 0;
        const displayOrder = overrideOrder > 0 ? overrideOrder : task.sort_order;
        const orderNum = (!isDone && !hasSubIndex && displayOrder > 0) ? `<span class="task-order-num">${displayOrder}</span>` : '';
        const subIndexLabel = hasSubIndex ? `<span class="task-sub-index">${parentOrder}.${subIndex}</span>` : '';

        // Collapse button for parent tasks with children
        const collapseBtn = hasChildren ? `
            <button class="btn-icon task-collapse-btn${this._collapsedTasks?.has(task.id) ? ' collapsed' : ''}" onclick="event.stopPropagation();app.toggleTaskCollapse(${task.id})" title="Collapse/Expand">
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="6 9 12 15 18 9"/></svg>
            </button>` : '';

        // Umbrella task badge and progress
        let umbrellaHtml = '';
        if (hasChildren && childrenList.length > 0) {
            const total = childrenList.length;
            const done = childrenList.filter(c => c.status === 'done').length;
            const inProg = childrenList.filter(c => c.status === 'in_progress').length;
            const pct = Math.round((done / total) * 100);
            umbrellaHtml = `<span class="task-umbrella-badge" title="${done}/${total} concluídas, ${inProg} em andamento">${done}/${total}</span>
                <span class="task-umbrella-progress"><span class="task-umbrella-progress-bar" style="width:${pct}%"></span></span>`;
        }

        // Drag handle (desktop) - hidden for done tasks
        const dragHandle = isDone ? '' : `
            <div class="task-drag-handle" title="Drag to reorder">
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                    <circle cx="8" cy="6" r="1.5"/><circle cx="16" cy="6" r="1.5"/>
                    <circle cx="8" cy="12" r="1.5"/><circle cx="16" cy="12" r="1.5"/>
                    <circle cx="8" cy="18" r="1.5"/><circle cx="16" cy="18" r="1.5"/>
                </svg>
            </div>`;

        const umbrellaClass = hasChildren ? ' task-umbrella' : '';

        return `
            <div class="task-card${overdueClass}${doneClass}${umbrellaClass}" data-task-id="${task.id}" data-project-id="${task.project_id}">
                ${dragHandle}
                <div class="task-priority-indicator ${priorityClass}"></div>
                <div class="task-card-body">
                    <div class="task-card-top">
                        ${subIndexLabel}
                        ${orderNum}
                        ${collapseBtn}
                        <span class="task-title" onclick="app.viewTaskDetail(${task.project_id}, ${task.id})" style="cursor:pointer;">${this.escapeHtml(task.title)}</span>
                        ${umbrellaHtml}
                        ${sessionIndicatorHtml}
                        <span class="task-status-badge badge-${task.status}" onclick="app.cycleTaskStatus(${task.project_id}, ${task.id}, '${task.status}')">${task.status.replace('_', ' ')}</span>
                    </div>
                    <div class="task-card-meta">
                        ${projectBadge}
                        ${dueDateHtml}
                        ${descPreview}
                    </div>
                </div>
                <div class="task-card-actions">
                    ${startSessionBtn}
                    <button class="btn-icon" onclick="app.showTaskModal(${task.project_id}, ${task.id})" title="Edit">
                        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/></svg>
                    </button>
                    <button class="btn-icon" onclick="app.duplicateTask(${task.project_id}, ${task.id})" title="Duplicate">
                        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>
                    </button>
                    <button class="btn-icon" onclick="app.deleteTask(${task.project_id}, ${task.id})" title="Delete">
                        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>
                    </button>
                </div>
            </div>
        `;
    }

    // Drag-and-drop for global tasks view
    setupAllTasksDragAndDrop(container) {
        if (!container) return;
        this._initSortable(container, null);
    }

    // Save reorder for global tasks view: uses global reorder endpoint
    async _saveAllTasksReorder(container) {
        if (!container) return;

        const items = [];
        let order = 1;

        // Top-level cards (skip done-section)
        container.querySelectorAll(':scope > .task-card').forEach(card => {
            if (card.closest('.done-section')) return;
            items.push({ id: parseInt(card.dataset.taskId), global_sort_order: order++, parent_id: null });
        });

        // Subtask containers (only direct children, not inside done-section)
        container.querySelectorAll(':scope > .task-subtasks').forEach(subtaskContainer => {
            if (subtaskContainer.closest('.done-section')) return;
            const parentCard = subtaskContainer.previousElementSibling;
            const parentId = parentCard?.classList.contains('task-card') ? parseInt(parentCard.dataset.taskId) : null;
            subtaskContainer.querySelectorAll(':scope > .task-card').forEach(card => {
                items.push({ id: parseInt(card.dataset.taskId), global_sort_order: order++, parent_id: parentId });
            });
        });

        if (items.length > 0) {
            try {
                await this.api('PUT', '/tasks/reorder', items);
            } catch (e) {
                console.error('Failed to reorder global tasks:', e);
            }
            this.loadAllTasks();
        }
    }

    _populateProjectFilter() {
        const select = document.getElementById('filter-project');
        if (!select || select.options.length > 1) return;
        (this.projects || []).forEach(p => {
            const opt = document.createElement('option');
            opt.value = p.id;
            opt.textContent = p.name;
            select.appendChild(opt);
        });
    }

    setupAllTasksFilters() {
        let searchTimeout = null;
        const searchInput = document.getElementById('filter-search');
        if (searchInput) {
            searchInput.addEventListener('input', () => {
                clearTimeout(searchTimeout);
                searchTimeout = setTimeout(() => this.loadAllTasks(), 300);
            });
        }
        ['filter-status', 'filter-priority', 'filter-project'].forEach(id => {
            const el = document.getElementById(id);
            if (el) el.addEventListener('change', () => this.loadAllTasks());
        });
    }

    filterByStatus(status) {
        const select = document.getElementById('filter-status');
        if (select) {
            const current = select.value;
            select.value = (current === status) ? '' : status;
        }
        this.loadAllTasks();
    }

    // ============ AI Proactive Suggestions ============

    async _loadPendingSuggestions() {
        try {
            const suggestions = await this.api('GET', '/ai/suggestions');
            this._pendingSuggestions = (suggestions || []).map(s => this._normalizeProactive(s));
            this._updateProactiveBadge();
            if (this._pendingSuggestions.length > 0) {
                this._pendingSuggestions.slice(0, 3).reverse().forEach(s => this.showAISuggestion(s));
            }
        } catch (e) { /* ignore */ }
    }

    _updateSuggestionBadge() {
        // Delegate to the proactive badge which uses the server-side unread count
        this._updateProactiveBadge();
    }

    // === AI Proactive Interaction Framework ===

    // Canonical proactive format:
    // { level, proactive_type, title, body, conversation_id, suggestion_id, actions[] }
    // This normalizes legacy AISuggestion DB records to the canonical format.
    _normalizeProactive(raw) {
        // Already in canonical format (from ai_proactive WebSocket)
        if (raw.proactive_type !== undefined && raw.suggestion_id !== undefined) {
            return raw;
        }
        // Legacy AISuggestion from DB: { id, type, title, description, level, conversation_id, ... }
        const typeMap = {
            link_task: 'task_suggestion',
            create_task: 'task_suggestion',
            update_task: 'task_suggestion',
            complete_task: 'task_suggestion',
        };
        return {
            level: raw.level || 'standard',
            proactive_type: typeMap[raw.type] || raw.type || 'task_suggestion',
            title: raw.title || '',
            body: '',
            conversation_id: raw.conversation_id?.Int64 ?? raw.conversation_id ?? null,
            suggestion_id: raw.id || null,
            actions: [
                {label: 'Aceitar', action: 'accept', style: 'primary'},
                {label: 'Discutir', action: 'discuss', style: 'outline'},
                {label: 'Ignorar', action: 'dismiss', style: 'secondary'},
            ],
        };
    }

    handleAIProactive(data) {
        // Track as pending for badge
        if (!this._pendingSuggestions) this._pendingSuggestions = [];
        if (data.suggestion_id) {
            this._pendingSuggestions.push(data);
        }
        this._updateProactiveBadge();

        // Dispatch based on urgency level
        switch (data.level) {
            case 'critical':
                this._showProactiveCritical(data);
                break;
            case 'subtle':
                this._showProactiveSubtle(data);
                break;
            case 'standard':
            default:
                this.showAISuggestion(data);
                break;
        }

        if (window.aiChat?.isOpen) window.aiChat.refreshPendingSuggestions?.();
    }

    _showProactiveCritical(data) {
        const overlay = document.createElement('div');
        overlay.className = 'ai-proactive-overlay ai-proactive-critical';
        overlay.id = `ai-proactive-critical-${data.conversation_id || Date.now()}`;
        overlay.innerHTML = `
            <div class="ai-proactive-modal ai-proactive-modal-critical">
                <div class="ai-proactive-icon">
                    <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="var(--color-danger)" stroke-width="2">
                        <path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/>
                        <line x1="12" y1="9" x2="12" y2="13"/>
                        <line x1="12" y1="17" x2="12.01" y2="17"/>
                    </svg>
                </div>
                <h2 class="ai-proactive-title">${this.escapeHtml(data.title)}</h2>
                <div class="ai-proactive-body">${this.escapeHtml(data.body || '').replace(/\n/g, '<br>')}</div>
                <div class="ai-proactive-actions">
                    ${(data.actions || []).map(a =>
                        `<button class="btn btn-${a.style || 'primary'} btn-sm" data-action="${a.action}">${this.escapeHtml(a.label)}</button>`
                    ).join('')}
                </div>
            </div>
        `;
        document.body.appendChild(overlay);

        overlay.querySelectorAll('[data-action]').forEach(btn => {
            btn.addEventListener('click', () => {
                const action = btn.dataset.action;
                overlay.remove();
                this._handleProactiveAction(action, data);
            });
        });
    }

    _showProactiveSubtle(data) {
        const container = this._ensureSuggestionContainer();

        const toast = document.createElement('div');
        toast.className = 'ai-proactive-toast';
        toast.id = `ai-proactive-toast-${data.conversation_id || Date.now()}`;
        toast.innerHTML = `
            <div class="ai-proactive-toast-content">
                <span class="ai-proactive-toast-title">${this.escapeHtml(data.title)}</span>
                ${data.conversation_id ? `<a href="#" class="ai-proactive-toast-link" data-conv-id="${data.conversation_id}">Ver</a>` : ''}
                <button class="ai-proactive-toast-close">&times;</button>
            </div>
        `;

        container.appendChild(toast);

        const link = toast.querySelector('.ai-proactive-toast-link');
        if (link) {
            link.addEventListener('click', (e) => {
                e.preventDefault();
                this.openProactiveConversation(parseInt(link.dataset.convId));
                toast.remove();
            });
        }

        toast.querySelector('.ai-proactive-toast-close')?.addEventListener('click', () => {
            toast.classList.add('ai-proactive-toast-exit');
            setTimeout(() => toast.remove(), 300);
        });

        // Auto-dismiss after 10s
        setTimeout(() => {
            if (toast.parentNode) {
                toast.classList.add('ai-proactive-toast-exit');
                setTimeout(() => toast.remove(), 300);
            }
        }, 10000);
    }

    _handleProactiveAction(action, data) {
        // Always remove the visual notification element from DOM
        this._removeProactiveElement(data);

        switch (action) {
            case 'accept':
                if (data.suggestion_id) this.acceptSuggestion(data.suggestion_id);
                break;
            case 'discuss':
                if (data.suggestion_id) this.discussSuggestion(data.suggestion_id);
                else if (data.conversation_id) this.openProactiveConversation(data.conversation_id);
                break;
            case 'dismiss':
                if (data.suggestion_id) this.dismissSuggestion(data.suggestion_id);
                break;
            case 'open':
                if (data.conversation_id) this.openProactiveConversation(data.conversation_id);
                break;
        }
    }

    _removeProactiveElement(data) {
        const candidates = [
            data.suggestion_id ? `ai-suggestion-${data.suggestion_id}` : null,
            data.conversation_id ? `ai-proactive-critical-${data.conversation_id}` : null,
            data.conversation_id ? `ai-proactive-toast-${data.conversation_id}` : null,
            'ai-suggestion-null',
            'ai-suggestion-undefined',
        ].filter(Boolean);
        for (const id of candidates) {
            const el = document.getElementById(id);
            if (el) {
                el.classList.add('ai-suggestion-exit');
                setTimeout(() => el.remove(), 300);
                return;
            }
        }
    }

    openProactiveConversation(conversationId) {
        if (window.aiChat) {
            window.aiChat.open();
            window.aiChat.loadConversation(conversationId);
        }
    }

    async _updateProactiveBadge() {
        try {
            const result = await this.api('GET', '/ai/unread-count');
            const count = result?.count || 0;
            let badge = document.getElementById('ai-suggestion-badge');
            const toggleBtn = document.getElementById('ai-chat-toggle');
            if (!toggleBtn) return;

            if (count > 0) {
                if (!badge) {
                    badge = document.createElement('span');
                    badge.id = 'ai-suggestion-badge';
                    badge.className = 'ai-suggestion-badge';
                    toggleBtn.appendChild(badge);
                }
                badge.textContent = count > 9 ? '9+' : count;
                badge.style.display = '';
            } else if (badge) {
                badge.style.display = 'none';
            }
        } catch (e) { /* ignore */ }
    }

    _ensureSuggestionContainer() {
        let container = document.getElementById('ai-suggestions-container');
        if (!container) {
            container = document.createElement('div');
            container.id = 'ai-suggestions-container';
            container.className = 'ai-suggestions-container';
            document.body.appendChild(container);
        }
        return container;
    }

    // Renders a standard-level proactive notification card.
    // Expects canonical format: { level, proactive_type, title, body, conversation_id, suggestion_id, actions[] }
    showAISuggestion(data) {
        const container = this._ensureSuggestionContainer();
        const sugId = data.suggestion_id;

        // Store for discuss/accept/dismiss
        if (!this._suggestionCache) this._suggestionCache = {};
        if (sugId) this._suggestionCache[sugId] = data;

        // Limit visible cards to 3
        while (container.children.length >= 3) {
            container.removeChild(container.firstChild);
        }

        const typeLabels = {
            task_suggestion: 'Sugestao de Tarefa',
            memory_doc_update: 'Memory Doc',
            insight: 'Insight',
            alert: 'Alerta'
        };
        const typeLabel = typeLabels[data.proactive_type] || data.proactive_type || 'AI';

        const card = document.createElement('div');
        card.className = 'ai-suggestion-card';
        card.id = `ai-suggestion-${sugId}`;

        card.innerHTML = `
            <div class="ai-suggestion-header">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 2a7 7 0 0 1 7 7c0 2.5-1.5 4.5-3 6-1 1-1.5 2.5-1.5 4h-5c0-1.5-.5-3-1.5-4-1.5-1.5-3-3.5-3-6a7 7 0 0 1 7-7z"/><path d="M9 21h6"/><path d="M10 24h4"/></svg>
                <span class="ai-suggestion-type">${typeLabel}</span>
                <button class="ai-suggestion-close" onclick="app._handleProactiveAction('dismiss', ${JSON.stringify({suggestion_id: sugId, conversation_id: data.conversation_id}).replace(/"/g, '&quot;')})">&times;</button>
            </div>
            <div class="ai-suggestion-body">
                <div class="ai-suggestion-title">${this.escapeHtml(data.title)}</div>
            </div>
            <div class="ai-suggestion-actions">
                ${(data.actions || []).map(a =>
                    `<button class="btn btn-${a.style || 'primary'} btn-sm" onclick="app._handleProactiveAction('${a.action}', ${JSON.stringify({suggestion_id: sugId, conversation_id: data.conversation_id}).replace(/"/g, '&quot;')})">${this.escapeHtml(a.label)}</button>`
                ).join('')}
            </div>
        `;

        container.appendChild(card);

        // Auto-hide card after 30s (stays accessible via AI panel history)
        setTimeout(() => {
            const el = document.getElementById(`ai-suggestion-${sugId}`);
            if (el) {
                el.classList.add('ai-suggestion-exit');
                setTimeout(() => el.remove(), 300);
            }
        }, 30000);
    }

    async acceptSuggestion(id) {
        try {
            await this.api('POST', `/ai/suggestions/${id}/accept`);
            this._removeSuggestionFromPending(id);
            const el = document.getElementById(`ai-suggestion-${id}`);
            if (el) {
                el.classList.add('ai-suggestion-exit');
                setTimeout(() => el.remove(), 300);
            }
            this.showToast('AI Suggestion', 'Sugestao aceita', 'success');
            // Refresh suggestions in chat panel if open
            if (window.aiChat?.isOpen) window.aiChat.refreshPendingSuggestions?.();
        } catch (e) {
            this.showToast('Erro', e.message, 'error');
        }
    }

    async dismissSuggestion(id) {
        try {
            await this.api('POST', `/ai/suggestions/${id}/dismiss`);
        } catch (e) { /* ignore */ }
        this._removeSuggestionFromPending(id);
        const el = document.getElementById(`ai-suggestion-${id}`);
        if (el) {
            el.classList.add('ai-suggestion-exit');
            setTimeout(() => el.remove(), 300);
        }
        // Refresh suggestions in chat panel if open
        if (window.aiChat?.isOpen) window.aiChat.refreshPendingSuggestions?.();
    }

    async discussSuggestion(id) {
        if (!window.aiChat) return;

        try {
            // Server creates/returns a proactive conversation with the assistant's first message
            const result = await this.api('POST', `/ai/suggestions/${id}/discuss`);
            if (result?.conversation_id) {
                window.aiChat.open();
                await window.aiChat.loadConversation(result.conversation_id);
            }
        } catch (e) {
            this.showToast('Erro', e.message || 'Falha ao abrir discussao', 'error');
        }

        // Remove the card (chat takes over)
        this._removeSuggestionFromPending(id);
        const el = document.getElementById(`ai-suggestion-${id}`);
        if (el) {
            el.classList.add('ai-suggestion-exit');
            setTimeout(() => el.remove(), 300);
        }
    }

    _removeSuggestionFromPending(id) {
        if (this._pendingSuggestions) {
            this._pendingSuggestions = this._pendingSuggestions.filter(s => s.id !== id);
            this._updateSuggestionBadge();
        }
        if (this._suggestionCache) {
            delete this._suggestionCache[id];
        }
    }
}

// Global confirm modal - replaces native confirm() across the entire system
function showConfirmModal(title, message, onConfirm, confirmLabel = 'Confirmar') {
    document.querySelector('.confirm-modal-overlay')?.remove();

    const overlay = document.createElement('div');
    overlay.className = 'confirm-modal-overlay';
    overlay.innerHTML = `
        <div class="confirm-modal">
            <div class="confirm-modal-title">${title}</div>
            <div class="confirm-modal-message">${message}</div>
            <div class="confirm-modal-actions">
                <button class="confirm-modal-cancel">Cancelar</button>
                <button class="confirm-modal-confirm">${confirmLabel}</button>
            </div>
        </div>
    `;

    overlay.querySelector('.confirm-modal-cancel').addEventListener('click', () => overlay.remove());
    overlay.addEventListener('click', (e) => { if (e.target === overlay) overlay.remove(); });
    overlay.querySelector('.confirm-modal-confirm').addEventListener('click', async () => {
        overlay.remove();
        await onConfirm();
    });

    document.body.appendChild(overlay);
}
window.showConfirmModal = showConfirmModal;

// Initialize app
const app = new DevManager();
window.app = app;
