// DevManager Application
class DevManager {
    constructor() {
        this.ws = null;
        this.currentView = 'projects';
        this.currentSession = null;
        this.projects = [];
        this.sessions = [];
        this.macros = [];
        this.skills = [];
        this.mcpServers = [];

        // Tab management
        this.openTabs = new Map(); // sessionId -> { projectName, sessionName }
        this.tabGroups = new Map(); // projectId -> group element
        this.groupingEnabled = true; // Toggle for flat vs grouped view

        // Split-screen mode
        this.splitScreenMode = false;
        this.splitScreenSessions = []; // Max 2 sessions in split view

        this.init();
    }

    init() {
        this.setupNavigation();
        this.setupWebSocket();
        this.loadInitialData();
        this.setupEventListeners();
        this.setupKeyboardShortcuts();
        this.setupStatusListener();

        // Setup mobile session dropdown
        this.setupMobileSessionDropdown();

        // Setup mobile terminal input
        this.setupMobileTerminalInput();

        // Restore tabs from previous session (after a delay to ensure sessions are loaded)
        setTimeout(() => this.restoreTabsFromStorage(), 1000);

        // Listen for service worker messages (e.g. navigate to session from push notification click)
        if ('serviceWorker' in navigator) {
            navigator.serviceWorker.addEventListener('message', (event) => {
                if (event.data && event.data.type === 'navigate_to_session') {
                    this.openTerminal(event.data.session_id);
                }
            });
        }
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

        // Update navigation
        document.querySelectorAll('.nav-item, .mobile-nav-item').forEach(item => {
            item.classList.toggle('active', item.dataset.view === viewName);
        });

        this.currentView = viewName;

        // Close sidebar on mobile
        document.getElementById('sidebar')?.classList.remove('open');

        // Refresh view data
        this.refreshViewData(viewName);
    }

    refreshViewData(viewName) {
        switch (viewName) {
            case 'projects':
                this.loadProjects();
                break;
            case 'sessions':
                this.loadSessions();
                break;
            case 'macros':
                this.loadMacros();
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
                break;
            case 'ping':
                this.ws.send(JSON.stringify({ type: 'pong' }));
                break;
            default:
                // Route hook messages to HookManager
                if (msg.type && msg.type.startsWith('hook_') && window.hookManager) {
                    window.hookManager.handleMessage(msg);
                }
                break;
        }
    }

    handleStateUpdate(data) {
        switch (data.entity) {
            case 'project':
                this.loadProjects();
                break;
            case 'session':
                this.loadSessions();
                if (this.currentSession === data.id) {
                    // Update terminal status
                }
                break;
            case 'macro':
                this.loadMacros();
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
            this.loadSessions(),
            this.loadMacros()
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
            <div class="card" data-project-id="${project.id}">
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
                    <button class="btn btn-primary btn-sm" onclick="app.startSession(${project.id})">
                        Start Session
                    </button>
                    <button class="btn btn-secondary btn-sm" onclick="app.editProject(${project.id})">
                        Edit
                    </button>
                    <button class="btn btn-secondary btn-sm" onclick="app.syncProjectConfig(${project.id})">
                        Sync
                    </button>
                </div>
            </div>
        `).join('');
    }

    async startSession(projectId) {
        // Get project name for default session name
        const project = this.projects.find(p => p.id === projectId);
        const projectName = project?.name || 'Session';
        const defaultName = `${projectName} (${new Date().toLocaleTimeString()})`;

        // Show session name modal
        const customName = await this.showSessionNameModal(defaultName);
        if (!customName) return; // User cancelled

        try {
            const session = await this.api('POST', '/sessions', { project_id: projectId });
            this.showToast('Success', 'Session started', 'success');
            this.openTerminal(session.id, session, customName);
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
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

    async syncProjectConfig(projectId) {
        try {
            await this.api('POST', `/projects/${projectId}/sync-config`);
            this.showToast('Success', 'Config synced', 'success');
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    editProject(projectId) {
        const project = this.projects.find(p => p.id === projectId);
        if (project) this.showProjectModal(project);
    }

    // Sessions
    async loadSessions() {
        try {
            this.sessions = await this.api('GET', '/sessions');
            this.renderSessions();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    renderSessions() {
        const container = document.getElementById('sessions-list');
        if (!container) return;

        console.log('renderSessions called, total sessions:', this.sessions.length);
        const activeSessions = this.sessions.filter(s => s.status === 'running' || s.status === 'starting');
        console.log('Active sessions:', activeSessions.length);

        // Sync tab sidebar with all running sessions
        this.syncSessionTabs(activeSessions);

        if (activeSessions.length === 0) {
            container.innerHTML = `
                <div class="empty-state">
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                        <polyline points="4 17 10 11 4 5"></polyline>
                        <line x1="12" y1="19" x2="20" y2="19"></line>
                    </svg>
                    <h3>No active sessions</h3>
                    <p>Start a session from the Projects view</p>
                </div>
            `;
            return;
        }

        container.innerHTML = activeSessions.map(session => {
            const project = this.projects.find(p => p.id === session.project_id);
            return `
                <div class="session-item" onclick="app.openTerminal('${session.id}')">
                    <div class="session-info">
                        <span class="badge badge-${session.status}">${session.status}</span>
                        <div>
                            <div class="session-name">${this.escapeHtml(project?.name || 'Unknown')}</div>
                            <div class="session-time">${this.formatTime(session.start_time)}</div>
                        </div>
                    </div>
                    <button class="btn btn-danger btn-sm" onclick="event.stopPropagation(); app.stopSession('${session.id}')">
                        Stop
                    </button>
                </div>
            `;
        }).join('');
    }

    async openTerminal(sessionId, sessionData = null, customName = null) {
        console.log('openTerminal called with:', sessionId);

        // Find session data (use provided data or look it up)
        let session = sessionData || this.sessions.find(s => s.id === sessionId);
        if (!session) {
            console.error('Session not found:', sessionId);
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
        }

        // Refresh sessions to sync tabs (this will create tab if needed)
        await this.loadSessions();

        // If custom name provided, update it
        if (customName) {
            const tabData = this.openTabs.get(sessionId);
            if (tabData) {
                tabData.sessionName = customName;
                // Update tab name in UI
                const tab = document.querySelector(`.terminal-tab[data-session-id="${sessionId}"]`);
                if (tab) {
                    const nameSpan = tab.querySelector('.terminal-tab-name');
                    if (nameSpan) nameSpan.textContent = customName;
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

        // Save tabs to storage
        this.saveTabsToStorage();
    }

    async stopSession(sessionId) {
        console.log('stopSession called with:', sessionId);
        try {
            await this.api('DELETE', `/sessions/${sessionId}`);
            this.showToast('Success', 'Session stopped', 'success');

            // Close tab if open
            if (this.openTabs.has(sessionId)) {
                this.closeTab(sessionId);
            }

            await this.loadSessions();
        } catch (error) {
            console.error('stopSession error:', error);
            this.showToast('Error', error.message, 'error');
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

        const nameSpan = document.createElement('span');
        nameSpan.className = 'terminal-tab-name';
        nameSpan.textContent = sessionName;

        // Close button
        const closeImg = document.createElement('img');
        closeImg.src = '/static/icons/close.svg';
        closeImg.className = 'terminal-tab-close';
        closeImg.alt = 'Close';

        tabDiv.appendChild(statusDot);
        tabDiv.appendChild(nameSpan);
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

        // If no tabs left, return to sessions view
        if (this.openTabs.size === 0) {
            this.showView('sessions');
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
            const tabName = `${projectName} (${new Date(session.start_time).toLocaleTimeString()})`;

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

        // If tabs were removed, navigate accordingly
        if (tabsRemoved) {
            if (this.openTabs.size > 0) {
                // Switch to the next available session
                const nextSessionId = Array.from(this.openTabs.keys())[0];
                window.terminalManager.switchToSession(nextSessionId);
                this.updateMobileSessionTrigger(nextSessionId);
            } else if (this.currentView === 'terminal') {
                // No tabs left and we're on terminal view - go to sessions
                this.showView('sessions');
            }
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

        // Setup mobile special keys bar
        this.setupMobileSpecialKeys();
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
                window.terminalManager.sendInput(sequence);
            }
        });
    }

    sendMobileTerminalInput() {
        const input = document.getElementById('mobile-terminal-input');
        if (!input) return;

        const text = input.value.trim();
        if (text) {
            // Send input to terminal (same as voice input)
            if (window.terminalManager) {
                // First, clear the current line in terminal
                window.terminalManager.sendInput('\x15'); // Ctrl+U

                // Send the text
                window.terminalManager.sendInput(text);

                // Send Enter key separately after a small delay
                setTimeout(() => {
                    window.terminalManager.sendInput('\r');
                }, 50);
            }

            // Clear input field
            input.value = '';
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

            // Restore each tab if session still exists
            for (const tabData of state.tabs) {
                const session = this.sessions.find(s => s.id === tabData.sessionId);

                // Only restore if session is still running
                if (session && session.status === 'running') {
                    await this.openTerminal(tabData.sessionId);

                    // Restore custom name if different
                    if (tabData.sessionName) {
                        window.terminalManager.renameSession(tabData.sessionId, tabData.sessionName);
                        const tab = document.querySelector(`.terminal-tab[data-session-id="${tabData.sessionId}"]`);
                        if (tab) {
                            const nameSpan = tab.querySelector('.terminal-tab-name');
                            if (nameSpan) nameSpan.textContent = tabData.sessionName;
                        }
                    }
                }
            }

            // Restore active tab
            if (state.activeSessionId && this.openTabs.has(state.activeSessionId)) {
                window.terminalManager.switchToSession(state.activeSessionId);
                this.updateTabActiveState(state.activeSessionId);
            }

            // Restore tab order
            this.restoreTabOrder();

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
        });
    }

    switchToNextTab() {
        const sessionIds = Array.from(this.openTabs.keys());
        if (sessionIds.length === 0) return;

        const currentIndex = sessionIds.indexOf(window.terminalManager.activeSessionId);
        const nextIndex = (currentIndex + 1) % sessionIds.length;

        window.terminalManager.switchToSession(sessionIds[nextIndex]);
        this.updateTabActiveState(sessionIds[nextIndex]);
    }

    switchToPreviousTab() {
        const sessionIds = Array.from(this.openTabs.keys());
        if (sessionIds.length === 0) return;

        const currentIndex = sessionIds.indexOf(window.terminalManager.activeSessionId);
        const prevIndex = (currentIndex - 1 + sessionIds.length) % sessionIds.length;

        window.terminalManager.switchToSession(sessionIds[prevIndex]);
        this.updateTabActiveState(sessionIds[prevIndex]);
    }

    switchToTabByIndex(index) {
        const sessionIds = Array.from(this.openTabs.keys());
        if (index >= 0 && index < sessionIds.length) {
            window.terminalManager.switchToSession(sessionIds[index]);
            this.updateTabActiveState(sessionIds[index]);
        }
    }

    // Macros
    async loadMacros() {
        try {
            this.macros = await this.api('GET', '/macros');
            this.renderMacros();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    renderMacros() {
        const container = document.getElementById('macros-list');
        if (!container) return;

        container.innerHTML = this.macros.map(macro => `
            <div class="card" data-macro-id="${macro.id}">
                <div class="card-header">
                    <div>
                        <div class="card-title">${this.escapeHtml(macro.name)}</div>
                        <div class="card-subtitle">${this.escapeHtml(macro.description)}</div>
                    </div>
                    ${macro.is_builtin ? '<span class="badge badge-builtin">Built-in</span>' : ''}
                </div>
                <div class="card-body">
                    <div class="text-muted">Target: ${macro.target_type}</div>
                </div>
                <div class="card-actions">
                    <button class="btn btn-primary btn-sm" onclick="app.showRunMacroModal(${macro.id})">
                        Run
                    </button>
                    ${!macro.is_builtin ? `
                        <button class="btn btn-secondary btn-sm" onclick="app.editMacro(${macro.id})">
                            Edit
                        </button>
                    ` : ''}
                </div>
            </div>
        `).join('');
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
        }

        container.innerHTML = content;
    }

    renderSkillsConfig() {
        return `
            <div class="mb-4">
                <button class="btn btn-primary" onclick="app.showSkillModal()">Add Skill</button>
            </div>
            <div class="card-grid">
                ${this.skills.map(skill => `
                    <div class="card">
                        <div class="card-header">
                            <div class="card-title">${this.escapeHtml(skill.name)}</div>
                            <input type="checkbox" ${skill.enabled ? 'checked' : ''}
                                onchange="app.toggleSkill(${skill.id}, this.checked)">
                        </div>
                        <div class="card-body">
                            <pre style="max-height: 100px; overflow: hidden; font-size: 11px;">${this.escapeHtml(skill.content.substring(0, 200))}...</pre>
                        </div>
                        <div class="card-actions">
                            <button class="btn btn-secondary btn-sm" onclick="app.editSkill(${skill.id})">Edit</button>
                            <button class="btn btn-danger btn-sm" onclick="app.deleteSkill(${skill.id})">Delete</button>
                        </div>
                    </div>
                `).join('')}
            </div>
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

        let pushStatusText = '';
        let pushBtnText = '';
        let pushBtnClass = '';
        let pushBtnAction = '';

        if (!pushSupported) {
            pushStatusText = 'Push notifications are not supported on this browser.';
        } else if (pushPermission === 'denied') {
            pushStatusText = 'Notifications blocked by the browser. Unblock in your browser/OS settings.';
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

        const html = `
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
                    <div class="form-group">
                        <label style="display: flex; align-items: center; cursor: pointer;">
                            <input type="checkbox" id="voice-auto-submit" style="margin-right: 8px;">
                            <span>Auto-submit after transcription (clears input and presses Enter)</span>
                        </label>
                    </div>
                    <button class="btn btn-primary btn-sm" onclick="app.saveWhisperSettings()">Save</button>
                </div>
            </div>
        `;

        // Populate settings after render
        setTimeout(() => {
            if (this.settings) {
                const providerSelect = document.getElementById('whisper-provider');
                if (providerSelect && this.settings.whisper_provider) {
                    providerSelect.value = this.settings.whisper_provider;
                }
                const autoSubmitCheckbox = document.getElementById('voice-auto-submit');
                if (autoSubmitCheckbox) {
                    autoSubmitCheckbox.checked = this.settings.voice_auto_submit === 'true';
                }
            }
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
        const isEdit = !!project;
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
            ssh_credential: formData.get('ssh_credential')
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

    async deleteProject(projectId) {
        if (!confirm('Are you sure you want to delete this project?')) return;

        try {
            await this.api('DELETE', `/projects/${projectId}`);
            this.hideModal();
            this.showToast('Success', 'Project deleted', 'success');
            this.loadProjects();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
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

    // Event listeners
    setupEventListeners() {
        // Add project button
        document.getElementById('btn-add-project')?.addEventListener('click', () => {
            this.showProjectModal();
        });

        // Add macro button
        document.getElementById('btn-add-macro')?.addEventListener('click', () => {
            this.showMacroModal();
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

        // Terminal back button
        document.getElementById('btn-back-sessions')?.addEventListener('click', () => {
            this.showView('sessions');
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

    // Additional methods for macros, skills, MCPs...
    showRunMacroModal(macroId) {
        const macro = this.macros.find(m => m.id === macroId);
        if (!macro) return;

        const filteredProjects = this.projects.filter(p =>
            macro.target_type === 'any' || macro.target_type === p.type
        );

        const content = `
            <form id="run-macro-form">
                <div class="form-group">
                    <label class="form-label">Select Project</label>
                    <select class="form-select" name="project_id" required>
                        ${filteredProjects.map(p => `
                            <option value="${p.id}">${this.escapeHtml(p.name)}</option>
                        `).join('')}
                    </select>
                </div>
            </form>
        `;

        const actions = `
            <button class="btn btn-secondary" onclick="app.hideModal()">Cancel</button>
            <button class="btn btn-primary" onclick="app.runMacro(${macroId})">Run</button>
        `;

        this.showModal(`Run Macro: ${macro.name}`, content, actions);
    }

    async runMacro(macroId) {
        const form = document.getElementById('run-macro-form');
        const projectId = parseInt(form.querySelector('select[name="project_id"]').value);

        try {
            const result = await this.api('POST', `/macros/${macroId}/run`, { project_id: projectId });
            this.hideModal();
            this.showToast('Success', 'Macro started', 'success');
            // Could open macro output view here
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    showMacroModal(macro = null) {
        if (window.macroEditor) {
            window.macroEditor.show(macro);
        }
    }

    editMacro(macroId) {
        const macro = this.macros.find(m => m.id === macroId);
        if (macro) this.showMacroModal(macro);
    }

    showSkillModal(skill = null) {
        const isEdit = !!skill;
        const content = `
            <form id="skill-form">
                <div class="form-group">
                    <label class="form-label">Name</label>
                    <input type="text" class="form-input" name="name" value="${skill?.name || ''}" required>
                </div>
                <div class="form-group">
                    <label class="form-label">Content</label>
                    <textarea class="form-textarea" name="content" rows="10" required>${skill?.content || ''}</textarea>
                </div>
                <div class="form-checkbox">
                    <input type="checkbox" name="enabled" ${skill?.enabled !== false ? 'checked' : ''}>
                    <label>Enabled</label>
                </div>
            </form>
        `;

        const actions = `
            <button class="btn btn-secondary" onclick="app.hideModal()">Cancel</button>
            <button class="btn btn-primary" onclick="app.saveSkill(${skill?.id || 'null'})">${isEdit ? 'Save' : 'Create'}</button>
        `;

        this.showModal(isEdit ? 'Edit Skill' : 'New Skill', content, actions);
    }

    async saveSkill(skillId) {
        const form = document.getElementById('skill-form');
        const data = {
            name: form.querySelector('input[name="name"]').value,
            content: form.querySelector('textarea[name="content"]').value,
            enabled: form.querySelector('input[name="enabled"]').checked
        };

        try {
            if (skillId) {
                await this.api('PUT', `/config/skills/${skillId}`, data);
            } else {
                await this.api('POST', '/config/skills', data);
            }
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

    async deleteSkill(skillId) {
        if (!confirm('Delete this skill?')) return;
        try {
            await this.api('DELETE', `/config/skills/${skillId}`);
            this.showToast('Success', 'Skill deleted', 'success');
            this.loadConfig();
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }

    async toggleSkill(skillId, enabled) {
        const skill = this.skills.find(s => s.id === skillId);
        if (skill) {
            await this.api('PUT', `/config/skills/${skillId}`, { ...skill, enabled });
        }
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

    async deleteMCP(mcpId) {
        if (!confirm('Delete this MCP server?')) return;
        try {
            await this.api('DELETE', `/config/mcps/${mcpId}`);
            this.showToast('Success', 'MCP server deleted', 'success');
            this.loadConfig();
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

    async saveWhisperSettings() {
        const provider = document.getElementById('whisper-provider').value;
        const openaiKey = document.getElementById('openai-key').value;
        const groqKey = document.getElementById('groq-key').value;
        const autoSubmit = document.getElementById('voice-auto-submit').checked;

        const settings = {
            whisper_provider: provider,
            voice_auto_submit: autoSubmit ? 'true' : 'false'
        };
        if (openaiKey) settings.openai_api_key = openaiKey;
        if (groqKey) settings.groq_api_key = groqKey;

        try {
            await this.api('PUT', '/config/settings', settings);
            this.showToast('Success', 'Settings saved', 'success');
            // Notify voice input of setting change
            if (window.voiceInput) {
                window.voiceInput.updateSettings();
            }
        } catch (error) {
            this.showToast('Error', error.message, 'error');
        }
    }
}

// Initialize app
const app = new DevManager();
window.app = app;
