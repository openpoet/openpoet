/**
 * StructuredViewManager — Chat-style JSONL event viewer
 *
 * Renders Claude Code session events as chat cards with collapsible
 * tool calls, thinking blocks, and token counters.
 */
class StructuredViewManager {
    constructor() {
        // sessionId → { container, messagesEl, tokenBarEl, events[], totalTokens }
        this.views = new Map();
        this.activeSessionId = null;
    }

    /**
     * Create the structured view container for a session.
     */
    createView(sessionId) {
        if (this.views.has(sessionId)) return this.views.get(sessionId);

        const wrapper = document.getElementById('terminal-containers-wrapper');
        if (!wrapper) return null;

        const container = document.createElement('div');
        container.className = 'structured-view-container';
        container.id = `structured-view-${sessionId}`;

        // Token bar
        const tokenBar = document.createElement('div');
        tokenBar.className = 'sv-token-bar';
        tokenBar.innerHTML = `
            <span><span class="sv-token-label">In:</span> <span class="sv-token-value" data-token="input">0</span></span>
            <span><span class="sv-token-label">Out:</span> <span class="sv-token-value" data-token="output">0</span></span>
            <span><span class="sv-token-label">Cache Read:</span> <span class="sv-token-value" data-token="cache_read">0</span></span>
            <span><span class="sv-token-label">Cache Write:</span> <span class="sv-token-value" data-token="cache_create">0</span></span>
        `;
        container.appendChild(tokenBar);

        // Messages area
        const messagesEl = document.createElement('div');
        messagesEl.className = 'sv-messages';
        container.appendChild(messagesEl);

        // Input area
        const inputArea = document.createElement('div');
        inputArea.className = 'sv-input-area';
        inputArea.innerHTML = `
            <div class="sv-input-state"></div>
            <div class="sv-input-wrapper">
                <textarea class="sv-input-textarea" rows="1"
                    placeholder="Send a message..."
                    autocomplete="off" autocorrect="off" autocapitalize="off"
                    spellcheck="false"></textarea>
                <button class="sv-input-send" title="Send message">
                    <svg width="20" height="20" viewBox="0 0 24 24" fill="none"
                         stroke="currentColor" stroke-width="2">
                        <line x1="22" y1="2" x2="11" y2="13"></line>
                        <polygon points="22 2 15 22 11 13 2 9 22 2"></polygon>
                    </svg>
                </button>
            </div>
            <div class="sv-input-hint">Enter to send, Shift+Enter for new line</div>
        `;
        container.appendChild(inputArea);

        const textarea = inputArea.querySelector('.sv-input-textarea');
        const sendBtn = inputArea.querySelector('.sv-input-send');

        // Real-time sync: forward textarea content to terminal on each keystroke
        let syncTimer = null;
        const syncToTerminal = () => {
            const tm = window.terminalManager;
            if (!tm) return;
            tm.sendInputToSession(sessionId, '\x15'); // Ctrl+U clear line
            if (textarea.value) {
                tm.sendInputToSession(sessionId, textarea.value);
            }
        };

        // Auto-resize + debounced sync to terminal
        textarea.addEventListener('input', () => {
            textarea.style.height = 'auto';
            textarea.style.height = Math.min(textarea.scrollHeight, 150) + 'px';
            clearTimeout(syncTimer);
            syncTimer = setTimeout(syncToTerminal, 50);
        });

        // Enter sends \r (text already on terminal from sync)
        textarea.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault();
                clearTimeout(syncTimer);
                syncToTerminal(); // ensure latest text is on terminal
                const tm = window.terminalManager;
                if (tm) {
                    setTimeout(() => tm.sendInputToSession(sessionId, '\r'), 30);
                }
                textarea.value = '';
                textarea.style.height = 'auto';
            }
        });

        // Send button: sync + Enter
        sendBtn.addEventListener('click', () => {
            clearTimeout(syncTimer);
            syncToTerminal();
            const tm = window.terminalManager;
            if (tm) {
                setTimeout(() => tm.sendInputToSession(sessionId, '\r'), 30);
            }
            textarea.value = '';
            textarea.style.height = 'auto';
        });

        wrapper.appendChild(container);

        const viewData = {
            container,
            messagesEl,
            tokenBarEl: tokenBar,
            inputArea,
            textarea,
            events: [],
            totalTokens: { input: 0, output: 0, cache_read: 0, cache_create: 0 },
            loaded: false,
            userScrolled: false,
        };

        // Track user scroll
        messagesEl.addEventListener('scroll', () => {
            const { scrollTop, scrollHeight, clientHeight } = messagesEl;
            viewData.userScrolled = scrollHeight - scrollTop - clientHeight > 50;
        });

        this.views.set(sessionId, viewData);
        return viewData;
    }

    /**
     * Load all events from the REST API.
     */
    async loadEvents(sessionId) {
        const view = this.views.get(sessionId);
        if (!view || view.loaded) return;

        view.messagesEl.innerHTML = '<div class="sv-empty">Loading events...</div>';

        try {
            const resp = await fetch(`/api/sessions/${sessionId}/events`);
            const data = await resp.json();

            let events;
            if (Array.isArray(data)) {
                events = data;
            } else if (data.reason) {
                view.messagesEl.innerHTML = `<div class="sv-empty">${this._reasonMessage(data.reason)}</div>`;
                view.loaded = true;
                return;
            } else {
                events = data.events || [];
            }

            view.messagesEl.innerHTML = '';

            if (events.length === 0) {
                view.messagesEl.innerHTML = '<div class="sv-empty">No events yet. Waiting for session activity...</div>';
            } else {
                for (const event of events) {
                    this._appendEventToDOM(view, event);
                }
                this._scrollToBottom(view);
            }

            view.loaded = true;

            // Start watching for real-time updates if session is active
            if (this._isSessionRunning(sessionId)) {
                this.startWatching(sessionId);
            }
        } catch (err) {
            console.error('Failed to load events:', err);
            view.messagesEl.innerHTML = '<div class="sv-empty">Failed to load events</div>';
        }
    }

    /**
     * Append a single event from WebSocket (real-time).
     */
    appendEvent(sessionId, event) {
        let view = this.views.get(sessionId);
        if (!view) return;

        // Remove "no events" placeholder if present
        const empty = view.messagesEl.querySelector('.sv-empty');
        if (empty) empty.remove();

        this._appendEventToDOM(view, event);

        if (!view.userScrolled) {
            this._scrollToBottom(view);
        }
    }

    /**
     * Show the structured view for a session.
     */
    show(sessionId) {
        const view = this.views.get(sessionId) || this.createView(sessionId);
        if (!view) return;

        view.container.classList.add('active');
        this.activeSessionId = sessionId;

        if (!view.loaded) {
            this.loadEvents(sessionId);
        }
    }

    /**
     * Start real-time file watching for an active session.
     */
    async startWatching(sessionId) {
        try {
            await fetch(`/api/sessions/${sessionId}/events/watch`, { method: 'POST' });
        } catch (e) {
            console.warn('Failed to start watching:', e);
        }
    }

    /**
     * Stop real-time file watching for a session.
     */
    async stopWatching(sessionId) {
        try {
            await fetch(`/api/sessions/${sessionId}/events/watch`, { method: 'DELETE' });
        } catch (e) {
            // ignore
        }
    }

    /**
     * Hide the structured view for a session.
     */
    hide(sessionId) {
        const view = this.views.get(sessionId);
        if (view) {
            view.container.classList.remove('active');
        }
        if (this.activeSessionId === sessionId) {
            this.activeSessionId = null;
        }
    }

    /**
     * Check if structured view is active for a session.
     */
    isActive(sessionId) {
        const view = this.views.get(sessionId);
        return view ? view.container.classList.contains('active') : false;
    }

    /**
     * Clean up when session tab is closed.
     */
    dispose(sessionId) {
        const view = this.views.get(sessionId);
        if (view) {
            this.stopWatching(sessionId);
            view.container.remove();
            this.views.delete(sessionId);
        }
    }

    // --- Internal rendering ---

    _appendEventToDOM(view, event) {
        view.events.push(event);

        const el = this._renderEvent(event);
        if (el) {
            view.messagesEl.appendChild(el);
        }

        // Update token counter
        if (event.message?.usage) {
            const u = event.message.usage;
            view.totalTokens.input += u.input_tokens || 0;
            view.totalTokens.output += u.output_tokens || 0;
            view.totalTokens.cache_read += u.cache_read_tokens || 0;
            view.totalTokens.cache_create += u.cache_creation_tokens || 0;
            this._updateTokenBar(view);
        }

        // Update input state hint
        this._updateInputState(view, event);
    }

    _renderEvent(event) {
        switch (event.type) {
            case 'user':
                return this._renderUserMessage(event);
            case 'assistant':
                return this._renderAssistantMessage(event);
            case 'progress':
                return this._renderProgress(event);
            default:
                return null;
        }
    }

    _renderUserMessage(event) {
        const msg = event.message;
        if (!msg) return null;

        const div = document.createElement('div');
        div.className = 'sv-message sv-message--user';

        const header = document.createElement('div');
        header.className = 'sv-message-header';
        header.innerHTML = `
            <span class="sv-role sv-role--user">User</span>
            <span class="sv-timestamp">${this._formatTime(event.timestamp)}</span>
        `;
        div.appendChild(header);

        const content = document.createElement('div');
        content.className = 'sv-content';

        for (const block of (msg.content_blocks || [])) {
            const blockEl = this._renderContentBlock(block);
            if (blockEl) content.appendChild(blockEl);
        }

        div.appendChild(content);
        return div;
    }

    _renderAssistantMessage(event) {
        const msg = event.message;
        if (!msg) return null;

        const div = document.createElement('div');
        div.className = 'sv-message sv-message--assistant';

        const header = document.createElement('div');
        header.className = 'sv-message-header';

        let headerHTML = '<span class="sv-role sv-role--assistant">Assistant</span>';
        if (msg.model) {
            headerHTML += `<span class="sv-model">${this._escapeHtml(msg.model)}</span>`;
        }
        headerHTML += `<span class="sv-timestamp">${this._formatTime(event.timestamp)}</span>`;
        if (msg.usage) {
            const u = msg.usage;
            headerHTML += `<span class="sv-tokens-inline">in:${this._formatNum(u.input_tokens)} out:${this._formatNum(u.output_tokens)}</span>`;
        }
        header.innerHTML = headerHTML;
        div.appendChild(header);

        const content = document.createElement('div');
        content.className = 'sv-content';

        for (const block of (msg.content_blocks || [])) {
            const blockEl = this._renderContentBlock(block);
            if (blockEl) content.appendChild(blockEl);
        }

        div.appendChild(content);
        return div;
    }

    _renderContentBlock(block) {
        switch (block.type) {
            case 'text':
                return this._renderTextBlock(block);
            case 'tool_use':
                if (block.tool_name === 'AskUserQuestion') {
                    return this._renderAskUserBlock(block);
                }
                return this._renderToolUseBlock(block);
            case 'tool_result':
                return this._renderToolResultBlock(block);
            case 'thinking':
                return this._renderThinkingBlock(block);
            default:
                return null;
        }
    }

    _renderTextBlock(block) {
        if (!block.text) return null;
        const div = document.createElement('div');
        div.className = 'sv-text markdown-body';
        if (typeof marked !== 'undefined') {
            try {
                div.innerHTML = marked.parse(block.text);
            } catch (e) {
                div.textContent = block.text;
            }
        } else {
            div.textContent = block.text;
        }
        return div;
    }

    _renderToolUseBlock(block) {
        const div = document.createElement('div');
        div.className = 'sv-tool-use';

        const summary = this._toolInputSummary(block.tool_name, block.tool_input);

        div.innerHTML = `
            <button class="sv-tool-toggle">
                <span class="sv-tool-icon">&#9881;</span>
                <span class="sv-tool-name">${this._escapeHtml(block.tool_name || 'Tool')}</span>
                <span class="sv-tool-summary">${this._escapeHtml(summary)}</span>
                <span class="sv-tool-chevron">&#9654;</span>
            </button>
            <div class="sv-tool-body">
                <div class="sv-tool-section-label">Input</div>
                <pre class="sv-tool-input">${this._escapeHtml(this._formatToolInput(block.tool_input))}</pre>
            </div>
        `;

        div.querySelector('.sv-tool-toggle').addEventListener('click', () => {
            div.classList.toggle('expanded');
        });

        return div;
    }

    _renderToolResultBlock(block) {
        const div = document.createElement('div');
        div.className = 'sv-tool-result';

        const content = block.content || '';
        const preview = content.length > 80 ? content.substring(0, 80) + '...' : content;
        const errorClass = block.is_error ? ' sv-tool-output--error' : '';

        div.innerHTML = `
            <button class="sv-tool-toggle">
                <span class="sv-tool-icon">${block.is_error ? '&#10060;' : '&#9989;'}</span>
                <span class="sv-tool-name">Result</span>
                <span class="sv-tool-summary">${this._escapeHtml(preview)}</span>
                <span class="sv-tool-chevron">&#9654;</span>
            </button>
            <div class="sv-tool-body">
                <pre class="sv-tool-output${errorClass}">${this._escapeHtml(content)}</pre>
            </div>
        `;

        div.querySelector('.sv-tool-toggle').addEventListener('click', () => {
            div.classList.toggle('expanded');
        });

        return div;
    }

    _renderThinkingBlock(block) {
        if (!block.text) return null;

        const div = document.createElement('div');
        div.className = 'sv-thinking';

        const preview = block.text.length > 100 ? block.text.substring(0, 100) + '...' : block.text;

        div.innerHTML = `
            <button class="sv-thinking-toggle">
                <span class="sv-tool-icon">&#128161;</span>
                <span>Thinking</span>
                <span class="sv-thinking-preview">${this._escapeHtml(preview)}</span>
                <span class="sv-tool-chevron">&#9654;</span>
            </button>
            <div class="sv-thinking-body">${this._escapeHtml(block.text)}</div>
        `;

        div.querySelector('.sv-thinking-toggle').addEventListener('click', () => {
            div.classList.toggle('expanded');
        });

        return div;
    }

    _renderProgress(event) {
        if (!event.progress) return null;
        const p = event.progress;
        const div = document.createElement('div');
        div.className = 'sv-message sv-message--progress';
        div.innerHTML = `<div class="sv-progress"><span class="sv-progress-dot"></span> ${this._escapeHtml(p.hook_event || p.type || '')} ${this._escapeHtml(p.hook_name || '')}</div>`;
        return div;
    }

    _renderAskUserBlock(block) {
        const input = block.tool_input || {};
        const questions = input.questions || [];
        if (questions.length === 0) return this._renderToolUseBlock(block);

        const div = document.createElement('div');
        div.className = 'sv-ask-user';

        const header = document.createElement('div');
        header.className = 'sv-ask-header';
        header.innerHTML = '&#10067; Question from Claude';
        div.appendChild(header);

        for (const q of questions) {
            const qDiv = document.createElement('div');
            qDiv.className = 'sv-ask-question';

            if (q.question) {
                const text = document.createElement('div');
                text.className = 'sv-ask-question-text';
                text.textContent = q.question;
                qDiv.appendChild(text);
            }

            const options = q.options || [];
            if (options.length > 0) {
                const optionsDiv = document.createElement('div');
                optionsDiv.className = 'sv-ask-options';

                for (const opt of options) {
                    const optEl = document.createElement('div');
                    optEl.className = 'sv-ask-option';

                    const label = document.createElement('span');
                    label.className = 'sv-ask-option-label';
                    label.textContent = opt.label || '';
                    optEl.appendChild(label);

                    if (opt.description) {
                        const desc = document.createElement('span');
                        desc.className = 'sv-ask-option-desc';
                        desc.textContent = opt.description;
                        optEl.appendChild(desc);
                    }

                    optionsDiv.appendChild(optEl);
                }

                qDiv.appendChild(optionsDiv);
            }

            div.appendChild(qDiv);
        }

        return div;
    }

    _updateInputState(view, event) {
        const stateEl = view.inputArea?.querySelector('.sv-input-state');
        if (!stateEl) return;

        const msg = event.message;
        if (!msg) {
            stateEl.textContent = '';
            return;
        }

        if (msg.role === 'assistant') {
            if (msg.stop_reason === 'end_turn') {
                stateEl.textContent = '';
                if (view.textarea) view.textarea.placeholder = 'Send a message...';
            } else if (msg.stop_reason === 'tool_use') {
                stateEl.textContent = 'Claude is using tools...';
            } else {
                stateEl.textContent = 'Claude is responding...';
            }
        } else if (msg.role === 'user') {
            stateEl.textContent = 'Waiting for response...';
        }
    }

    // --- Helpers ---

    _updateTokenBar(view) {
        const t = view.totalTokens;
        const bar = view.tokenBarEl;
        bar.querySelector('[data-token="input"]').textContent = this._formatNum(t.input);
        bar.querySelector('[data-token="output"]').textContent = this._formatNum(t.output);
        bar.querySelector('[data-token="cache_read"]').textContent = this._formatNum(t.cache_read);
        bar.querySelector('[data-token="cache_create"]').textContent = this._formatNum(t.cache_create);
    }

    _scrollToBottom(view) {
        requestAnimationFrame(() => {
            view.messagesEl.scrollTop = view.messagesEl.scrollHeight;
        });
    }

    _formatTime(ts) {
        if (!ts) return '';
        const d = new Date(ts);
        return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
    }

    _formatNum(n) {
        if (!n) return '0';
        return n.toLocaleString();
    }

    _escapeHtml(str) {
        if (!str) return '';
        const div = document.createElement('div');
        div.textContent = str;
        return div.innerHTML;
    }

    _toolInputSummary(toolName, input) {
        if (!input) return '';
        if (typeof input === 'string') return input.substring(0, 60);

        // Common tool shortcuts
        switch (toolName) {
            case 'Bash':
                return input.command || '';
            case 'Read':
                return input.file_path || '';
            case 'Write':
                return input.file_path || '';
            case 'Edit':
                return input.file_path || '';
            case 'Grep':
                return `${input.pattern || ''} ${input.path || ''}`.trim();
            case 'Glob':
                return input.pattern || '';
            case 'Skill':
                return input.skill || '';
            case 'Task':
                return input.description || '';
            default:
                // Try to find a useful short field
                const keys = Object.keys(input);
                if (keys.length === 0) return '';
                const val = input[keys[0]];
                if (typeof val === 'string') return val.substring(0, 60);
                return '';
        }
    }

    _formatToolInput(input) {
        if (!input) return '';
        if (typeof input === 'string') return input;
        try {
            return JSON.stringify(input, null, 2);
        } catch {
            return String(input);
        }
    }

    _isSessionRunning(sessionId) {
        if (!window.terminalManager) return false;
        const td = window.terminalManager.terminals.get(sessionId);
        return td && td.status === 'running';
    }

    _reasonMessage(reason) {
        switch (reason) {
            case 'remote':
                return 'Structured view is not available for remote sessions';
            case 'unsupported_backend':
                return 'Structured view is only available for Claude Code sessions';
            default:
                return 'Structured view is not available for this session';
        }
    }
}

// Global instance
window.structuredView = new StructuredViewManager();

// Wire toggle button
document.getElementById('btn-structured-view')?.addEventListener('click', () => {
    if (window.terminalManager) {
        window.terminalManager.toggleStructuredView();
    }
});
