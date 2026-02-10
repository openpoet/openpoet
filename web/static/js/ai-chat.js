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

        this.isOpen = false;
        this.isStreaming = false;
        this.abortController = null;
        this.currentConversationId = null;
        this.conversations = [];

        if (this.panel) {
            this.setupEventListeners();
            this.checkStatus();
        }
    }

    setupEventListeners() {
        // Close button
        this.closeBtn?.addEventListener('click', () => this.close());

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

            // /app/doc/{id} → open document viewer modal
            const docMatch = href.match(/^\/app\/doc\/([a-zA-Z0-9-]+)$/);
            if (docMatch) {
                e.preventDefault();
                this.openDocumentViewer(docMatch[1]);
                return;
            }
        });
    }

    async openDocumentViewer(docId) {
        try {
            const resp = await fetch(`/api/documents/${docId}`);
            if (!resp.ok) throw new Error('Document not found');
            const doc = await resp.json();

            // Remove existing viewer if any
            document.getElementById('doc-viewer-modal')?.remove();

            const modal = document.createElement('div');
            modal.id = 'doc-viewer-modal';
            modal.className = 'doc-viewer-modal';
            modal.innerHTML = `
                <div class="doc-viewer-container">
                    <div class="doc-viewer-header">
                        <h3>${this.escapeHtml(doc.title || 'Documento')}</h3>
                        <button class="doc-viewer-close" title="Fechar">&times;</button>
                    </div>
                    <div class="doc-viewer-body meta-rendered">
                        ${this.renderMarkdown(doc.content)}
                    </div>
                </div>
            `;

            document.body.appendChild(modal);

            // Close handlers
            modal.querySelector('.doc-viewer-close').addEventListener('click', () => modal.remove());
            modal.addEventListener('click', (e) => {
                if (e.target === modal) modal.remove();
            });
        } catch (e) {
            console.error('Failed to open document:', e);
        }
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
    }

    newConversation() {
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
                meta_update: 'Meta Doc',
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

                            case 'error':
                                fullText += `\n\n**Error:** ${event.data.message}`;
                                contentEl.innerHTML = this.renderMarkdown(fullText);
                                break;

                            case 'done':
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

    addMessage(role, text) {
        const el = document.createElement('div');
        el.className = `ai-chat-msg ai-chat-msg-${role}`;
        el.innerHTML = `
            <div class="ai-chat-msg-avatar">${role === 'user' ? 'You' : 'AI'}</div>
            <div class="ai-chat-msg-content">${role === 'user' ? this.escapeHtml(text) : this.renderMarkdown(text)}</div>
        `;
        this.messagesContainer?.appendChild(el);
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
            'get_project_meta': 'Reading meta document',
            'update_project_meta': 'Updating meta document',
            'create_document': 'Creating document',
            'list_tasks': 'Listing tasks',
            'create_task': 'Creating task',
            'update_task': 'Updating task',
            'delete_task': 'Deleting task',
            'get_task_report': 'Generating task report',
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
        if (this.abortController) {
            this.abortController.abort();
            this.abortController = null;
        }
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
        try {
            const resp = await fetch(`/api/ai/conversations/${id}`);
            const data = await resp.json();

            this.currentConversationId = id;
            this._updateDeleteCurrentBtn();
            this.messagesContainer.innerHTML = '';
            this.hideHistory();

            for (const msg of data.messages) {
                this.addMessage(msg.role, msg.content);
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
