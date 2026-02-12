// AI Chat Manager
class AIChatManager {
    constructor() {
        this.panel = document.getElementById('ai-chat-panel');
        this.messagesContainer = document.getElementById('ai-chat-messages');
        this.input = document.getElementById('ai-chat-input');
        this.sendBtn = document.getElementById('ai-chat-send');
        this.stopBtn = document.getElementById('ai-chat-stop');
        this.newChatBtn = document.getElementById('ai-chat-new');
        this.historyBtn = document.getElementById('ai-chat-history');
        this.deleteCurrentBtn = document.getElementById('ai-chat-delete-current');
        this.closeBtn = document.getElementById('ai-chat-close');
        this.statusEl = document.getElementById('ai-chat-status');
        this.expandBtn = document.getElementById('ai-chat-expand');
        this.voiceBtn = document.getElementById('ai-chat-voice');
        this.editor = document.getElementById('ai-chat-editor');
        this.editorTextarea = document.getElementById('ai-chat-editor-textarea');
        this.editorClose = document.getElementById('ai-chat-editor-close');
        this.editorSend = document.getElementById('ai-chat-editor-send');
        this.editorVoice = document.getElementById('ai-chat-editor-voice');

        this.toolsBtn = document.getElementById('ai-chat-tools');
        this.toolsCountEl = document.getElementById('ai-chat-tools-count');
        this.toolsPanel = document.getElementById('ai-chat-tools-panel');
        this.toolsList = document.getElementById('ai-chat-tools-list');

        this.isOpen = false;
        this.isStreaming = false;
        this.abortController = null;
        this.currentConversationId = null;
        this.conversations = [];
        this._toolsLoaded = false;
        this._toolsPanelOpen = false;

        if (this.panel) {
            this.setupEventListeners();
            this.checkStatus();
            this.checkActiveStream();
            this.loadToolDefinitions();
        }
    }

    setupEventListeners() {
        // Close button
        this.closeBtn?.addEventListener('click', () => this.close());

        // Tools button
        this.toolsBtn?.addEventListener('click', () => this.toggleToolsPanel());

        // Send
        this.sendBtn?.addEventListener('click', () => this.sendMessage());
        this.input?.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault();
                this.sendMessage();
            }
        });

        // Auto-resize textarea
        this.input?.addEventListener('input', () => {
            this.input.style.height = 'auto';
            this.input.style.height = Math.min(this.input.scrollHeight, 120) + 'px';
        });

        // Stop
        this.stopBtn?.addEventListener('click', () => this.stopStreaming());

        // New chat
        this.newChatBtn?.addEventListener('click', () => this.newConversation());

        // History
        this.historyBtn?.addEventListener('click', () => this.toggleHistory());

        // Delete current conversation
        this.deleteCurrentBtn?.addEventListener('click', () => this.deleteCurrentConversation());

        // Click outside to close on mobile
        this.panel?.addEventListener('click', (e) => {
            if (e.target === this.panel) this.close();
        });

        // Expand to full-screen editor
        this.expandBtn?.addEventListener('click', () => this.openEditor());

        // Voice input (inline)
        this.voiceBtn?.addEventListener('click', () => {
            if (window.voiceInput) {
                window.voiceInput.startRecordingWithCallback((text) => {
                    if (this.input) {
                        const current = this.input.value;
                        this.input.value = current.length > 0 ? current + ' ' + text : text;
                        this.input.dispatchEvent(new Event('input'));
                    }
                });
            }
        });

        // Full-screen editor: close
        this.editorClose?.addEventListener('click', () => this.closeEditor());

        // Full-screen editor: send
        this.editorSend?.addEventListener('click', () => {
            if (!this.editorTextarea) return;
            const text = this.editorTextarea.value.trim();
            if (text && this.input) {
                this.input.value = text;
            }
            this.editorTextarea.value = '';
            this.closeEditor();
            if (text) this.sendMessage();
        });

        // Full-screen editor: voice
        this.editorVoice?.addEventListener('click', () => {
            if (window.voiceInput) {
                window.voiceInput.startRecordingWithCallback((text) => {
                    if (this.editorTextarea) {
                        const current = this.editorTextarea.value;
                        this.editorTextarea.value = current.length > 0 ? current + ' ' + text : text;
                    }
                });
            }
        });

        // Intercept internal app links in chat messages
        this.messagesContainer?.addEventListener('click', (e) => {
            const link = e.target.closest('a');
            if (!link) return;
            const href = link.getAttribute('href');
            if (!href) return;

            // /app/project/{id} → navigate to project detail
            const projectMatch = href.match(/^\/app\/project\/(\d+)$/);
            if (projectMatch) {
                e.preventDefault();
                const projectId = parseInt(projectMatch[1]);
                this.close();
                if (window.app) window.app.showProjectDetail(projectId);
                return;
            }

            // /app/doc/{id} → open document viewer overlay
            const docMatch = href.match(/^\/app\/doc\/([a-zA-Z0-9-]+)$/);
            if (docMatch) {
                e.preventDefault();
                if (window.docViewer) {
                    window.docViewer.open(docMatch[1]);
                }
                return;
            }
        });
    }

    openEditor() {
        if (!this.editor || !this.editorTextarea || !this.input) return;
        this.editorTextarea.value = this.input.value;
        this.input.value = '';
        this.editor.classList.remove('hidden');
        this.editorTextarea.focus();
    }

    closeEditor() {
        if (!this.editor || !this.editorTextarea) return;
        // Copy any remaining text back to the inline input
        if (this.input && this.editorTextarea.value.trim()) {
            this.input.value = this.editorTextarea.value;
        }
        this.editorTextarea.value = '';
        this.editor.classList.add('hidden');
    }

    async checkStatus() {
        try {
            const resp = await fetch('/api/ai/status');
            const data = await resp.json();
            this.configured = data.configured;
            this.provider = data.provider;
            this.model = data.model;
            this.updateStatusDisplay();
        } catch (e) {
            this.configured = false;
        }
    }

    async checkActiveStream() {
        try {
            const resp = await fetch('/api/ai/active-stream');
            const data = await resp.json();
            if (data.active && data.conversation_id) {
                // Auto-load the active streaming conversation
                await this.loadConversation(data.conversation_id);
            }
        } catch (e) {
            // Ignore — no active stream
        }
    }

    updateStatusDisplay() {
        if (!this.statusEl) return;
        if (this.configured) {
            const providerLabel = this.provider === 'apikey' ? 'API Key' : 'Claude Code';
            this.statusEl.textContent = providerLabel;
            this.statusEl.className = 'ai-chat-status-badge configured';
        } else {
            this.statusEl.textContent = 'Not configured';
            this.statusEl.className = 'ai-chat-status-badge not-configured';
        }
    }

    toggle() {
        if (this.isOpen) {
            this.close();
        } else {
            this.open();
        }
    }

    open() {
        if (!this.panel) return;
        this.isOpen = true;
        this.panel.classList.add('open');
        document.body.classList.add('ai-chat-open');
        if (window.innerWidth > 768) {
            this.input?.focus();
        }
        this.checkStatus();
        this.refreshPendingSuggestions();
    }

    close() {
        if (!this.panel) return;
        this.isOpen = false;
        this.panel.classList.remove('open');
        document.body.classList.remove('ai-chat-open');
        if (this._toolsPanelOpen) this.toggleToolsPanel();
    }

    async loadToolDefinitions() {
        try {
            const resp = await fetch('/api/ai/tool-definitions');
            if (!resp.ok) return;
            const tools = await resp.json();
            if (!Array.isArray(tools) || tools.length === 0) return;

            this._tools = tools;
            this._toolsLoaded = true;
            if (this.toolsBtn) this.toolsBtn.style.display = '';
            if (this.toolsCountEl) this.toolsCountEl.textContent = tools.length;
        } catch (e) {
            // silently ignore
        }
    }

    toggleToolsPanel() {
        if (!this.toolsPanel) return;
        this._toolsPanelOpen = !this._toolsPanelOpen;
        this.toolsPanel.style.display = this._toolsPanelOpen ? '' : 'none';
        this.toolsBtn?.classList.toggle('active', this._toolsPanelOpen);

        if (this._toolsPanelOpen && this._tools && this.toolsList) {
            this.toolsList.innerHTML = this._tools.map(t => {
                const desc = t.description || '';
                const shortDesc = desc.length > 80 ? desc.slice(0, 80) + '...' : desc;
                return `<div class="ai-chat-tool-item"><strong>${this.escapeHtml(t.name)}</strong><span>${this.escapeHtml(shortDesc)}</span></div>`;
            }).join('');
        }
    }

    newConversation() {
        if (this._pollInterval) { clearInterval(this._pollInterval); this._pollInterval = null; }
        if (this.abortController) { this.abortController.abort(); this.abortController = null; }
        this.currentConversationId = null;
        this._updateDeleteCurrentBtn();
        this.messagesContainer.innerHTML = '';
        this.hideHistory();
        this.addWelcomeMessage();
    }

    _updateDeleteCurrentBtn() {
        if (this.deleteCurrentBtn) {
            this.deleteCurrentBtn.style.display = this.currentConversationId ? '' : 'none';
        }
    }

    addWelcomeMessage() {
        if (!this.messagesContainer) return;
        if (this.messagesContainer.children.length > 0) return;

        const welcome = document.createElement('div');
        welcome.className = 'ai-chat-welcome';
        welcome.innerHTML = `
            <div class="ai-chat-welcome-icon">
                <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
                    <path d="M12 2L2 7l10 5 10-5-10-5z"/>
                    <path d="M2 17l10 5 10-5"/>
                    <path d="M2 12l10 5 10-5"/>
                </svg>
            </div>
            <h3>AI Assistant</h3>
            <p>Ask me to create skills, manage MCP servers, or configure your DevManager.</p>
            <div id="ai-chat-pending-suggestions"></div>
            <div class="ai-chat-suggestions">
                <button class="ai-chat-suggestion" onclick="window.aiChat?.sendPreset('Create a skill for Python best practices')">Create a Python skill</button>
                <button class="ai-chat-suggestion" onclick="window.aiChat?.sendPreset('List all my skills')">List my skills</button>
                <button class="ai-chat-suggestion" onclick="window.aiChat?.sendPreset('List all projects')">List projects</button>
            </div>
        `;
        this.messagesContainer.appendChild(welcome);
        this.refreshPendingSuggestions();
    }

    async refreshPendingSuggestions() {
        const container = document.getElementById('ai-chat-pending-suggestions');
        if (!container) return;

        try {
            const rawSuggestions = await window.app?.api('GET', '/ai/suggestions');
            if (!rawSuggestions || rawSuggestions.length === 0) {
                container.innerHTML = '';
                return;
            }

            // Normalize to canonical proactive format
            const suggestions = rawSuggestions.map(s => window.app?._normalizeProactive(s) || s);

            const typeLabels = {
                task_suggestion: 'Tarefa',
                memory_doc_update: 'Memory Doc',
                insight: 'Insight',
                alert: 'Alerta'
            };

            container.innerHTML = `
                <div class="ai-chat-pending-section">
                    <div class="ai-chat-pending-header">
                        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 2a7 7 0 0 1 7 7c0 2.5-1.5 4.5-3 6-1 1-1.5 2.5-1.5 4h-5c0-1.5-.5-3-1.5-4-1.5-1.5-3-3.5-3-6a7 7 0 0 1 7-7z"/><path d="M9 21h6"/></svg>
                        Sugestoes pendentes (${suggestions.length})
                    </div>
                    ${suggestions.map(s => `
                        <div class="ai-chat-pending-item">
                            <div class="ai-chat-pending-item-info">
                                <span class="ai-chat-pending-type">${typeLabels[s.proactive_type] || s.proactive_type}</span>
                                <span class="ai-chat-pending-title">${this._escapeHtml(s.title)}</span>
                            </div>
                            <div class="ai-chat-pending-item-actions">
                                <button class="btn btn-primary btn-xs" onclick="window.app?.acceptSuggestion(${s.suggestion_id})">Aceitar</button>
                                <button class="btn btn-outline btn-xs" onclick="window.app?.discussSuggestion(${s.suggestion_id})">Discutir</button>
                                <button class="btn btn-secondary btn-xs" onclick="window.app?.dismissSuggestion(${s.suggestion_id})">Ignorar</button>
                            </div>
                        </div>
                    `).join('')}
                </div>
            `;

            // Update app cache with normalized data
            if (window.app) {
                window.app._pendingSuggestions = suggestions;
                if (!window.app._suggestionCache) window.app._suggestionCache = {};
                suggestions.forEach(s => { if (s.suggestion_id) window.app._suggestionCache[s.suggestion_id] = s; });
                window.app._updateSuggestionBadge();
            }
        } catch (e) { /* ignore */ }
    }

    _escapeHtml(text) {
        const div = document.createElement('div');
        div.textContent = text;
        return div.innerHTML;
    }

    sendPreset(text) {
        if (this.input) {
            this.input.value = text;
            this.sendMessage();
        }
    }

    async sendMessage() {
        const text = this.input?.value?.trim();
        if (!text || this.isStreaming) return;

        // Remove welcome message
        const welcome = this.messagesContainer?.querySelector('.ai-chat-welcome');
        if (welcome) welcome.remove();

        // Clear input
        this.input.value = '';
        this.input.style.height = 'auto';

        // Add user message
        this.addMessage('user', text);

        // Show streaming state
        this.setStreaming(true);

        // Create assistant message placeholder
        const assistantEl = this.addMessage('assistant', '');
        const contentEl = assistantEl.querySelector('.ai-chat-msg-content');

        this.abortController = new AbortController();

        try {
            const resp = await fetch('/api/ai/chat', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    conversation_id: this.currentConversationId || 0,
                    message: text,
                }),
                signal: this.abortController.signal,
            });

            if (!resp.ok) {
                const err = await resp.json();
                throw new Error(err.error || 'Request failed');
            }

            // Read SSE stream
            const reader = resp.body.getReader();
            const decoder = new TextDecoder();
            let buffer = '';
            let fullText = '';
            let pendingDocCards = [];

            while (true) {
                const { done, value } = await reader.read();
                if (done) break;

                buffer += decoder.decode(value, { stream: true });
                const lines = buffer.split('\n');
                buffer = lines.pop(); // keep incomplete line

                for (const line of lines) {
                    if (!line.startsWith('data: ')) continue;
                    const jsonStr = line.slice(6);
                    if (!jsonStr) continue;

                    try {
                        const event = JSON.parse(jsonStr);
                        switch (event.type) {
                            case 'conversation':
                                this.currentConversationId = event.data.id;
                                this._updateDeleteCurrentBtn();
                                break;

                            case 'message_id':
                                this.currentStreamingMessageId = event.data.id;
                                if (assistantEl) assistantEl.dataset.messageId = event.data.id;
                                break;

                            case 'text':
                                fullText += event.data.text;
                                contentEl.innerHTML = this.renderMarkdown(fullText);
                                this.scrollToBottom();
                                break;

                            case 'tool_start':
                                this.addToolIndicator(contentEl, event.data.name, 'running');
                                break;

                            case 'tool_executing':
                                this.updateToolIndicator(contentEl, event.data.name, 'executing');
                                break;

                            case 'tool_result':
                                this.updateToolIndicator(contentEl, event.data.name, 'done', event.data.result);
                                break;

                            case 'doc_card':
                                pendingDocCards.push(event.data);
                                break;

                            case 'error':
                                fullText += `\n\n**Error:** ${event.data.message}`;
                                contentEl.innerHTML = this.renderMarkdown(fullText);
                                break;

                            case 'done':
                                // Render doc cards inside content (after text is finalized)
                                for (const cardData of pendingDocCards) {
                                    this.addDocCard(contentEl, cardData);
                                }
                                // Render doc cards queued from WebSocket (MCP propose flow)
                                if (this._pendingWSDocCards) {
                                    for (const cardData of this._pendingWSDocCards) {
                                        this.addDocCard(contentEl, cardData);
                                    }
                                    this._pendingWSDocCards = [];
                                }
                                // Render mermaid diagrams now that streaming is complete
                                if (typeof FileViewer !== 'undefined') FileViewer.renderMermaid(contentEl);
                                // Show token usage below the message
                                if (event.data?.usage) {
                                    const u = event.data.usage;
                                    const totalTokens = (u.input_tokens || 0) + (u.output_tokens || 0);
                                    if (totalTokens > 0) {
                                        const tokenInfo = document.createElement('div');
                                        tokenInfo.className = 'ai-chat-token-info';
                                        const cost = u.cost_usd || 0;
                                        tokenInfo.textContent = `${totalTokens.toLocaleString()} tokens (~$${cost.toFixed(4)})`;
                                        contentEl.parentElement.appendChild(tokenInfo);
                                    }
                                }
                                break;
                        }
                    } catch (e) {
                        // skip malformed events
                    }
                }
            }
        } catch (e) {
            if (e.name === 'AbortError') {
                contentEl.innerHTML += '<em class="ai-chat-stopped">[Stopped]</em>';
            } else {
                contentEl.innerHTML = `<span class="ai-chat-error">${this.escapeHtml(e.message)}</span>`;
            }
        } finally {
            this.setStreaming(false);
            this.scrollToBottom();
        }
    }

    addMessage(role, text, messageId) {
        const el = document.createElement('div');
        el.className = `ai-chat-msg ai-chat-msg-${role}`;
        if (messageId) el.dataset.messageId = messageId;
        el.innerHTML = `
            <div class="ai-chat-msg-avatar">${role === 'user' ? 'You' : 'AI'}</div>
            <div class="ai-chat-msg-content">${role === 'user' ? this.escapeHtml(text) : this.renderMarkdown(text)}</div>
        `;
        this.messagesContainer?.appendChild(el);
        if (role === 'assistant') {
            const contentEl = el.querySelector('.ai-chat-msg-content');
            if (contentEl && typeof FileViewer !== 'undefined') FileViewer.renderMermaid(contentEl);
        }
        this.scrollToBottom();
        return el;
    }

    addToolIndicator(container, toolName, status) {
        const indicator = document.createElement('div');
        indicator.className = 'ai-chat-tool-indicator';
        indicator.dataset.tool = toolName;
        indicator.innerHTML = `
            <span class="ai-chat-tool-spinner"></span>
            <span class="ai-chat-tool-name">${this.escapeHtml(this.formatToolName(toolName))}</span>
            <span class="ai-chat-tool-status">${status === 'running' ? 'Starting...' : ''}</span>
        `;
        container.appendChild(indicator);
        this.scrollToBottom();
    }

    updateToolIndicator(container, toolName, status, result) {
        const indicator = container.querySelector(`.ai-chat-tool-indicator[data-tool="${toolName}"]`);
        if (!indicator) return;

        if (status === 'done') {
            indicator.classList.add('done');
            indicator.querySelector('.ai-chat-tool-spinner').innerHTML = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="var(--color-success)" stroke-width="2.5"><polyline points="20 6 9 17 4 12"/></svg>';
            indicator.querySelector('.ai-chat-tool-status').textContent = 'Done';
        } else {
            indicator.querySelector('.ai-chat-tool-status').textContent = 'Executing...';
        }
    }

    addDocCard(contentEl, data) {
        const card = document.createElement('div');
        card.className = `ai-chat-doc-card ai-chat-doc-card-${data.type || 'document'}`;
        if (data.doc_id) card.dataset.docId = data.doc_id;

        const isProposal = data.type === 'proposal';
        const isPlanning = data.type === 'planning';
        const isTaskProposal = data.type === 'task_proposal';
        const status = data.status || 'pending';

        let icon;
        if (isPlanning) {
            icon = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2"/><rect x="9" y="3" width="6" height="4" rx="1"/><path d="M9 14l2 2 4-4"/></svg>';
        } else if (isProposal) {
            icon = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M11 4H4a2 2 0 00-2 2v14a2 2 0 002 2h14a2 2 0 002-2v-7"/><path d="M18.5 2.5a2.121 2.121 0 013 3L12 15l-4 1 1-4 9.5-9.5z"/></svg>';
        } else if (isTaskProposal) {
            icon = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 20h9"/><path d="M16.5 3.5a2.121 2.121 0 013 3L7 19l-4 1 1-4L16.5 3.5z"/></svg>';
        } else {
            icon = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M14 2H6a2 2 0 00-2 2v16a2 2 0 002 2h12a2 2 0 002-2V8z"/><polyline points="14 2 14 8 20 8"/><line x1="9" y1="15" x2="15" y2="15"/><line x1="9" y1="11" x2="15" y2="11"/></svg>';
        }

        const title = this.escapeHtml(data.title || 'Documento');
        const summary = data.summary ? `<div class="ai-chat-doc-card-summary">${this.escapeHtml(data.summary)}</div>` : '';
        const taskCount = data.task_count ? `<div class="ai-chat-doc-card-task-count">${data.task_count} ação(ões)</div>` : '';

        let actionHtml;
        if (status === 'approved') {
            actionHtml = '<span class="ai-chat-doc-card-badge ai-chat-doc-card-badge-approved">Aprovado</span>';
        } else if (status === 'rejected') {
            actionHtml = '<span class="ai-chat-doc-card-badge ai-chat-doc-card-badge-rejected">Rejeitado</span>';
        } else {
            const btnLabel = isTaskProposal ? 'Revisar Tarefa' : (isPlanning ? 'Revisar Plano' : (isProposal ? 'Revisar Proposta' : 'Ver Documento'));
            actionHtml = `<button class="ai-chat-doc-card-btn">${btnLabel}</button>`;
        }

        card.innerHTML = `
            <div class="ai-chat-doc-card-icon">${icon}</div>
            <div class="ai-chat-doc-card-info">
                <div class="ai-chat-doc-card-title">${title}</div>
                ${summary}
                ${taskCount}
            </div>
            ${actionHtml}
        `;

        const btn = card.querySelector('.ai-chat-doc-card-btn');
        if (btn) {
            btn.addEventListener('click', () => {
                if (window.docViewer && data.doc_id) {
                    window.docViewer.open(data.doc_id);
                }
            });
        }

        contentEl.appendChild(card);
        this.scrollToBottom();
    }

    // Called by doc viewer after approve/reject to update the card status immediately.
    updateDocCardStatus(docId, status) {
        const card = this.messagesContainer?.querySelector(`.ai-chat-doc-card[data-doc-id="${docId}"]`);
        if (!card) return;

        // Remove existing button
        const btn = card.querySelector('.ai-chat-doc-card-btn');
        if (btn) btn.remove();

        // Remove existing badge (in case of double-call)
        const oldBadge = card.querySelector('.ai-chat-doc-card-badge');
        if (oldBadge) oldBadge.remove();

        let badgeHtml;
        if (status === 'approved') {
            badgeHtml = '<span class="ai-chat-doc-card-badge ai-chat-doc-card-badge-approved">Aprovado</span>';
        } else if (status === 'rejected') {
            badgeHtml = '<span class="ai-chat-doc-card-badge ai-chat-doc-card-badge-rejected">Rejeitado</span>';
        }
        if (badgeHtml) {
            card.insertAdjacentHTML('beforeend', badgeHtml);
        }
    }

    // Called by WebSocket handler when a chat_doc_card event arrives (e.g. from MCP propose endpoint).
    // Queues the card for injection when the stream ends (to avoid being wiped by innerHTML updates).
    injectDocCardFromWS(data) {
        // Only queue if the conversation_id matches the current chat conversation
        const convId = data.conversation_id;
        if (convId && String(convId) !== String(this.currentConversationId)) return;
        if (!this._pendingWSDocCards) this._pendingWSDocCards = [];
        this._pendingWSDocCards.push(data);
    }

    formatToolName(name) {
        const labels = {
            'create_skill': 'Creating skill',
            'update_skill': 'Updating skill',
            'delete_skill': 'Deleting skill',
            'list_skills': 'Listing skills',
            'list_projects': 'Listing projects',
            'list_mcp_servers': 'Listing MCP servers',
            'create_mcp_server': 'Creating MCP server',
            'update_setting': 'Updating setting',
            'sync_config': 'Syncing configuration',
            'get_memory_doc': 'Reading memory doc',
            'update_memory_doc': 'Updating memory doc',
            'create_document': 'Creating document',
            'list_tasks': 'Listing tasks',
            'create_task': 'Creating task',
            'update_task': 'Updating task',
            'delete_task': 'Deleting task',
            'get_task_report': 'Generating task report',
            'list_directory': 'Browsing files',
            'read_file': 'Reading file',
            'find_files': 'Searching files',
            'grep_content': 'Searching content',
            'activate_planning_mode': 'Activating planning mode',
        };
        return labels[name] || name;
    }

    setStreaming(streaming) {
        this.isStreaming = streaming;
        if (this.sendBtn) this.sendBtn.style.display = streaming ? 'none' : '';
        if (this.stopBtn) this.stopBtn.style.display = streaming ? '' : 'none';
        if (this.input) this.input.disabled = streaming;
    }

    stopStreaming() {
        // Abort the SSE connection (original or reconnected)
        if (this.abortController) {
            this.abortController.abort();
            this.abortController = null;
        }
        // Tell the backend to cancel the LLM stream
        if (this.currentConversationId) {
            fetch(`/api/ai/conversations/${this.currentConversationId}/stop`, { method: 'POST' });
        }
        this.setStreaming(false);
    }

    async toggleHistory() {
        const existing = this.panel?.querySelector('.ai-chat-history-panel');
        if (existing) {
            existing.remove();
            return;
        }
        await this.showHistory();
    }

    hideHistory() {
        this.panel?.querySelector('.ai-chat-history-panel')?.remove();
    }

    async showHistory() {
        try {
            const resp = await fetch('/api/ai/conversations');
            this.conversations = await resp.json();
        } catch (e) {
            return;
        }

        this.hideHistory();

        const historyPanel = document.createElement('div');
        historyPanel.className = 'ai-chat-history-panel';

        if (this.conversations.length === 0) {
            historyPanel.innerHTML = '<div class="ai-chat-history-empty">No conversations yet</div>';
        } else {
            const listHtml = this.conversations.map(c => {
                const isAI = c.source === 'ai';
                const isUnread = isAI && !c.is_read;
                return `
                <div class="ai-chat-history-item ${c.id === this.currentConversationId ? 'active' : ''} ${isAI ? 'ai-initiated' : ''}" data-id="${c.id}">
                    <div class="ai-chat-history-item-content">
                        ${isAI ? '<span class="ai-chat-history-badge-ai">AI</span>' : ''}
                        <div class="ai-chat-history-title">${this.escapeHtml(c.title || 'Untitled')}</div>
                        ${isUnread ? '<span class="ai-chat-history-unread"></span>' : ''}
                    </div>
                    <div class="ai-chat-history-date">${this.formatDate(c.updated_at)}</div>
                    <button class="ai-chat-history-delete" data-id="${c.id}" title="Excluir">&times;</button>
                </div>
            `}).join('');

            const actionsHtml = `
                <div class="ai-chat-history-actions">
                    <button class="ai-chat-history-clear-all" title="Excluir todo o histórico">
                        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 01-2 2H7a2 2 0 01-2-2V6m3 0V4a2 2 0 012-2h4a2 2 0 012 2v2"/></svg>
                        Excluir tudo
                    </button>
                </div>
            `;

            historyPanel.innerHTML = listHtml + actionsHtml;
        }

        // Insert after header
        const header = this.panel?.querySelector('.ai-chat-header');
        if (header) {
            header.after(historyPanel);
        }

        // Event listeners
        historyPanel.querySelectorAll('.ai-chat-history-item').forEach(item => {
            item.addEventListener('click', (e) => {
                if (e.target.classList.contains('ai-chat-history-delete')) return;
                this.loadConversation(parseInt(item.dataset.id));
            });
        });

        historyPanel.querySelectorAll('.ai-chat-history-delete').forEach(btn => {
            btn.addEventListener('click', async (e) => {
                e.stopPropagation();
                const id = parseInt(btn.dataset.id);
                this.confirmDeleteConversation(id);
            });
        });

        const clearAllBtn = historyPanel.querySelector('.ai-chat-history-clear-all');
        if (clearAllBtn) {
            clearAllBtn.addEventListener('click', (e) => {
                e.stopPropagation();
                this.confirmDeleteAllConversations();
            });
        }
    }

    async loadConversation(id) {
        // Clear any active polling/SSE from previous conversation
        if (this._pollInterval) { clearInterval(this._pollInterval); this._pollInterval = null; }
        if (this.abortController) { this.abortController.abort(); this.abortController = null; }

        try {
            const resp = await fetch(`/api/ai/conversations/${id}`);
            const data = await resp.json();

            this.currentConversationId = id;
            this._updateDeleteCurrentBtn();
            this.messagesContainer.innerHTML = '';
            this.hideHistory();

            for (const msg of data.messages) {
                const el = this.addMessage(msg.role, msg.content, msg.id);

                // Show status indicators for incomplete assistant messages
                if (msg.role === 'assistant' && el) {
                    const contentEl = el.querySelector('.ai-chat-msg-content');
                    if (contentEl && msg.status === 'streaming') {
                        const indicator = document.createElement('div');
                        indicator.className = 'ai-chat-status-indicator ai-chat-status-streaming';
                        indicator.innerHTML = '<em>Processando...</em>';
                        contentEl.appendChild(indicator);
                    } else if (contentEl && msg.status === 'error') {
                        const indicator = document.createElement('div');
                        indicator.className = 'ai-chat-status-indicator ai-chat-status-error';
                        if (msg.error_info === 'aborted') {
                            indicator.innerHTML = '<em>[Resposta parada pelo usuário]</em>';
                        } else {
                            indicator.innerHTML = `<em>[Erro: ${this.escapeHtml(msg.error_info || 'Erro desconhecido')}]</em>`;
                            this._addRetryButton(el, msg);
                        }
                        contentEl.appendChild(indicator);
                    }
                }
            }

            // If last message is still streaming, reconnect to SSE stream
            const lastMsg = data.messages[data.messages.length - 1];
            if (lastMsg && lastMsg.role === 'assistant' && lastMsg.status === 'streaming') {
                this._reconnectToStream(id);
            }

            // Render persisted doc cards matched to their originating assistant message
            if (data.doc_cards?.length) {
                for (const doc of data.doc_cards) {
                    let type = 'document';
                    if (doc.title && doc.title.startsWith('Memory Doc:')) type = 'proposal';
                    else if (doc.title && doc.title.startsWith('Planejamento:')) type = 'planning';
                    else if (doc.title && doc.title.startsWith('Tarefa:')) type = 'task_proposal';

                    // Find the specific assistant message this doc card belongs to
                    let targetContent = null;
                    if (doc.message_id) {
                        const msgEl = this.messagesContainer.querySelector(`.ai-chat-msg[data-message-id="${doc.message_id}"]`);
                        if (msgEl) targetContent = msgEl.querySelector('.ai-chat-msg-content');
                    }
                    // Fallback: attach to last assistant message
                    if (!targetContent) {
                        const assistantMsgs = this.messagesContainer.querySelectorAll('.ai-chat-msg-assistant .ai-chat-msg-content');
                        targetContent = assistantMsgs.length > 0 ? assistantMsgs[assistantMsgs.length - 1] : null;
                    }

                    if (targetContent) {
                        this.addDocCard(targetContent, {
                            doc_id: doc.id,
                            type,
                            title: doc.title,
                            summary: doc.summary,
                            status: doc.status,
                        });
                    }
                }
            }

            this.scrollToBottom();

            // Update proactive badge (server auto-marks as read)
            if (data.conversation?.source === 'ai') {
                window.app?._updateProactiveBadge?.();
            }
        } catch (e) {
            console.error('Failed to load conversation:', e);
        }
    }

    _reconnectToStream(convId) {
        this.setStreaming(true);
        const contentEl = this.messagesContainer.querySelector('.ai-chat-msg-assistant:last-child .ai-chat-msg-content');
        if (!contentEl) return;

        // Get current text from the rendered content as baseline
        let fullText = contentEl.textContent || '';

        // Remove any streaming indicator
        const oldIndicator = contentEl.querySelector('.ai-chat-status-indicator');
        if (oldIndicator) oldIndicator.remove();

        this.abortController = new AbortController();

        fetch(`/api/ai/conversations/${convId}/stream`, {
            signal: this.abortController.signal,
        }).then(async (resp) => {
            if (!resp.ok) {
                // No active stream — fall back to loading the final state
                this.setStreaming(false);
                await this.loadConversation(convId);
                return;
            }

            const reader = resp.body.getReader();
            const decoder = new TextDecoder();
            let buffer = '';
            let pendingDocCards = [];

            while (true) {
                const { done, value } = await reader.read();
                if (done) break;

                buffer += decoder.decode(value, { stream: true });
                const lines = buffer.split('\n');
                buffer = lines.pop();

                for (const line of lines) {
                    if (!line.startsWith('data: ')) continue;
                    const jsonStr = line.slice(6);
                    if (!jsonStr) continue;

                    try {
                        const event = JSON.parse(jsonStr);
                        switch (event.type) {
                            case 'reconnected':
                                // Connection established — re-fetch content baseline from DB
                                try {
                                    const convResp = await fetch(`/api/ai/conversations/${convId}`);
                                    const convData = await convResp.json();
                                    const lastMsg = convData.messages[convData.messages.length - 1];
                                    if (lastMsg && lastMsg.role === 'assistant') {
                                        fullText = lastMsg.content;
                                        contentEl.innerHTML = this.renderMarkdown(fullText);
                                    }
                                } catch (_) {}
                                break;

                            case 'text':
                                fullText += event.data.text;
                                contentEl.innerHTML = this.renderMarkdown(fullText);
                                this.scrollToBottom();
                                break;

                            case 'tool_start':
                                this.addToolIndicator(contentEl, event.data.name, 'running');
                                break;

                            case 'tool_executing':
                                this.updateToolIndicator(contentEl, event.data.name, 'executing');
                                break;

                            case 'tool_result':
                                this.updateToolIndicator(contentEl, event.data.name, 'done', event.data.result);
                                break;

                            case 'doc_card':
                                pendingDocCards.push(event.data);
                                break;

                            case 'error':
                                fullText += `\n\n**Error:** ${event.data.message}`;
                                contentEl.innerHTML = this.renderMarkdown(fullText);
                                break;

                            case 'done':
                                for (const cardData of pendingDocCards) {
                                    this.addDocCard(contentEl, cardData);
                                }
                                if (typeof FileViewer !== 'undefined') FileViewer.renderMermaid(contentEl);
                                if (event.data?.usage) {
                                    const u = event.data.usage;
                                    const totalTokens = (u.input_tokens || 0) + (u.output_tokens || 0);
                                    if (totalTokens > 0) {
                                        const tokenInfo = document.createElement('div');
                                        tokenInfo.className = 'ai-chat-token-info';
                                        const cost = u.cost_usd || 0;
                                        tokenInfo.textContent = `${totalTokens.toLocaleString()} tokens (~$${cost.toFixed(4)})`;
                                        contentEl.parentElement.appendChild(tokenInfo);
                                    }
                                }
                                break;
                        }
                    } catch (_) {}
                }
            }

            this.setStreaming(false);
            this.scrollToBottom();
            // Re-render full conversation for final state
            await this.loadConversation(convId);
        }).catch((e) => {
            if (e.name !== 'AbortError') {
                console.error('Stream reconnect error:', e);
            }
            this.setStreaming(false);
        });
    }

    _addRetryButton(messageEl, msg) {
        const retryBtn = document.createElement('button');
        retryBtn.className = 'ai-chat-retry-btn';
        retryBtn.textContent = 'Tentar novamente';
        retryBtn.addEventListener('click', () => this.retryMessage(msg));
        messageEl.appendChild(retryBtn);
    }

    async retryMessage(failedMsg) {
        if (!this.currentConversationId || this.isStreaming) return;

        try {
            // Find the last user message in the conversation
            const resp = await fetch(`/api/ai/conversations/${this.currentConversationId}`);
            const data = await resp.json();
            const messages = data.messages || [];

            let lastUserMsg = null;
            for (let i = messages.length - 1; i >= 0; i--) {
                if (messages[i].role === 'user') {
                    lastUserMsg = messages[i];
                    break;
                }
            }

            if (!lastUserMsg) return;

            // Reload the conversation to get clean state, then re-send
            await this.loadConversation(this.currentConversationId);
            this.input.value = lastUserMsg.content;
            this.sendMessage();
        } catch (e) {
            console.error('Retry failed:', e);
        }
    }

    confirmDeleteConversation(id) {
        showConfirmModal('Excluir conversa?', 'Esta ação não pode ser desfeita.', async () => {
            await this.deleteConversation(id);
        }, 'Excluir');
    }

    confirmDeleteAllConversations() {
        showConfirmModal('Excluir todo o histórico?', 'Todas as conversas serão removidas permanentemente.', async () => {
            await this.deleteAllConversations();
        }, 'Excluir');
    }

    async deleteConversation(id) {
        try {
            await fetch(`/api/ai/conversations/${id}`, { method: 'DELETE' });
            if (this.currentConversationId === id) {
                this.newConversation();
            }
            await this.showHistory();
        } catch (e) {
            console.error('Failed to delete conversation:', e);
        }
    }

    async deleteAllConversations() {
        try {
            await fetch('/api/ai/conversations', { method: 'DELETE' });
            this.newConversation();
            await this.showHistory();
        } catch (e) {
            console.error('Failed to delete all conversations:', e);
        }
    }

    deleteCurrentConversation() {
        if (!this.currentConversationId) return;
        this.confirmDeleteConversation(this.currentConversationId);
    }

    scrollToBottom() {
        if (this.messagesContainer) {
            this.messagesContainer.scrollTop = this.messagesContainer.scrollHeight;
        }
    }

    renderMarkdown(text) {
        if (!text) return '';
        if (typeof marked !== 'undefined') {
            try {
                // Ensure the mermaid-aware renderer is initialized
                if (typeof FileViewer !== 'undefined') FileViewer._initMarked();
                return marked.parse(text);
            } catch (e) {
                return this.escapeHtml(text);
            }
        }
        return this.escapeHtml(text).replace(/\n/g, '<br>');
    }

    escapeHtml(str) {
        if (!str) return '';
        return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }

    formatDate(dateStr) {
        if (!dateStr) return '';
        const d = new Date(dateStr);
        const now = new Date();
        const diff = now - d;
        if (diff < 60000) return 'just now';
        if (diff < 3600000) return Math.floor(diff / 60000) + 'm ago';
        if (diff < 86400000) return Math.floor(diff / 3600000) + 'h ago';
        return d.toLocaleDateString();
    }
}

// Initialize when DOM is ready
document.addEventListener('DOMContentLoaded', () => {
    // Delay slightly to ensure panel HTML is in the DOM
    setTimeout(() => {
        window.aiChat = new AIChatManager();
        window.aiChat.addWelcomeMessage();
    }, 100);
});
