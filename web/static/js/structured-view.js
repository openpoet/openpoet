/**
 * StructuredViewManager — Chat-style JSONL event viewer
 *
 * Renders Claude Code session events as chat cards with collapsible
 * tool calls, thinking blocks, and token counters.
 */
var flog = window.flog || ((...a) => console.log(...a));
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

        // Top bar: mode pill (left) + token counters (right)
        const tokenBar = document.createElement('div');
        tokenBar.className = 'sv-token-bar';
        tokenBar.innerHTML = `
            <button class="sv-mode-pill" data-sessionid="${sessionId}" title="Click to cycle Claude Code modes (Shift+Tab)">
                <span class="sv-mode-icon">⏵</span>
                <span class="sv-mode-label">default</span>
                <span class="sv-mode-hint">shift+tab</span>
            </button>
            <div class="sv-session-model" hidden title="Current Codex model and reasoning effort">
                <span class="sv-session-meta-label">model</span>
                <span class="sv-session-meta-value" data-codex-model>default</span>
                <span class="sv-session-meta-separator"></span>
                <span class="sv-session-meta-label">reasoning</span>
                <span class="sv-session-meta-value" data-codex-reasoning>default</span>
            </div>
            <div class="sv-token-group">
                <span><span class="sv-token-label">in</span><span class="sv-token-value" data-token="input">0</span></span>
                <span><span class="sv-token-label">out</span><span class="sv-token-value" data-token="output">0</span></span>
                <span><span class="sv-token-label">cache r</span><span class="sv-token-value" data-token="cache_read">0</span></span>
                <span><span class="sv-token-label">cache w</span><span class="sv-token-value" data-token="cache_create">0</span></span>
            </div>
        `;
        container.appendChild(tokenBar);

        // Wire mode pill: click cycles via Shift+Tab to the PTY
        const modePill = tokenBar.querySelector('.sv-mode-pill');
        const modelInfoEl = tokenBar.querySelector('.sv-session-model');
        modePill?.addEventListener('click', () => {
            window.terminalManager?.sendShiftTab?.(sessionId);
            // Schedule a re-read after the TUI repaints
            setTimeout(() => this._refreshMode(sessionId), 80);
            setTimeout(() => this._refreshMode(sessionId), 250);
        });

        // Messages area
        const messagesEl = document.createElement('div');
        messagesEl.className = 'sv-messages';
        container.appendChild(messagesEl);

        // Input area — only on desktop. On mobile, the shared #mobile-terminal-input-bar
        // is the single input component used in both terminal and SV modes.
        const isMobile = window.innerWidth <= 768;
        let inputArea = null;
        let textarea = null;
        let sendToTerminal = null;

        if (!isMobile) {
            inputArea = document.createElement('div');
            inputArea.className = 'sv-input-area';
            inputArea.innerHTML = `
                <div class="sv-input-state"></div>
                <div class="sv-input-wrapper">
                    <textarea class="sv-input-textarea" rows="1"
                        placeholder="Send a message..."
                        autocomplete="off" autocorrect="off" autocapitalize="off"
                        spellcheck="false"></textarea>
                    <button class="sv-input-btn sv-btn-expand" title="Expand editor">
                        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                            <polyline points="15 3 21 3 21 9"></polyline>
                            <polyline points="9 21 3 21 3 15"></polyline>
                            <line x1="21" y1="3" x2="14" y2="10"></line>
                            <line x1="3" y1="21" x2="10" y2="14"></line>
                        </svg>
                    </button>
                    <button class="sv-input-btn sv-btn-image" title="Send Image">
                        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                            <rect x="3" y="3" width="18" height="18" rx="2" ry="2"></rect>
                            <circle cx="8.5" cy="8.5" r="1.5"></circle>
                            <polyline points="21 15 16 10 5 21"></polyline>
                        </svg>
                    </button>
                    <button class="sv-input-btn sv-btn-voice" title="Voice Input">
                        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                            <path d="M12 1a3 3 0 0 0-3 3v8a3 3 0 0 0 6 0V4a3 3 0 0 0-3-3z"></path>
                            <path d="M19 10v2a7 7 0 0 1-14 0v-2"></path>
                            <line x1="12" y1="19" x2="12" y2="23"></line>
                            <line x1="8" y1="23" x2="16" y2="23"></line>
                        </svg>
                    </button>
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

            textarea = inputArea.querySelector('.sv-input-textarea');
            const sendBtn = inputArea.querySelector('.sv-input-send');

            // Send text to terminal: clear line → text → Enter
            sendToTerminal = () => {
                const text = textarea.value;
                const tm = window.terminalManager;
                if (!tm) return;

                flog('SV-SEND', `sendToTerminal: sessionId=${sessionId}, text="${text}" (len=${text.length})`);
                // Send button: text and Enter must be separate writes for Claude Code
                if (text) {
                    tm.submitTerminalLine(sessionId, text);
                } else {
                    tm.sendInputToSession(sessionId, '\r');
                }

                textarea.value = '';
                textarea.style.height = 'auto';
                flog('SV-SEND', `sendToTerminal: textarea cleared`);
            };

            // Auto-resize textarea
            textarea.addEventListener('input', () => {
                const prevH = textarea.style.height;
                textarea.style.height = 'auto';
                const newH = Math.min(textarea.scrollHeight, 150) + 'px';
                textarea.style.height = newH;
                if (prevH !== newH) {
                    flog('SV-INPUT', `SV textarea resize: ${prevH} -> ${newH}, value="${textarea.value}" (len=${textarea.value.length})`);
                }
            });

            // Enter submits (Shift+Enter for newline)
            textarea.addEventListener('keydown', (e) => {
                if (this._handleCodexSlashTextareaKey(sessionId, textarea, e)) {
                    return;
                }
                if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault();
                    flog('SV-INPUT', `Enter pressed in SV textarea. value="${textarea.value}"`);
                    sendToTerminal();
                }
            });

            // Send button
            sendBtn.addEventListener('click', () => {
                flog('SV-INPUT', `Send button clicked in SV. value="${textarea.value}"`);
                if (this._openCodexSlashIfCommandPrefix(sessionId, textarea)) {
                    return;
                }
                sendToTerminal();
            });

            // Expand editor button
            inputArea.querySelector('.sv-btn-expand')?.addEventListener('click', () => {
                flog('SV-INPUT', `Expand editor clicked in SV. sessionId=${sessionId}, textarea value="${textarea.value}"`);
                window.app?.openSVEditor?.(sessionId);
            });

            // Image picker button
            inputArea.querySelector('.sv-btn-image')?.addEventListener('click', () => {
                document.getElementById('image-picker-input')?.click();
            });

            // Voice input button
            const voiceBtn = inputArea.querySelector('.sv-btn-voice');
            voiceBtn?.addEventListener('click', () => {
                if (window.voiceInput) {
                    if (window.voiceInput.isRecording) {
                        window.voiceInput.stopRecording(false);
                        voiceBtn.classList.remove('recording');
                        return;
                    }
                    voiceBtn.classList.add('recording');
                    window.voiceInput.startRecordingWithCallback((text) => {
                        voiceBtn.classList.remove('recording');
                        const current = textarea.value;
                        textarea.value = current.length > 0 ? current + ' ' + text : text;
                        textarea.style.height = 'auto';
                        textarea.style.height = Math.min(textarea.scrollHeight, 150) + 'px';
                        textarea.focus();
                    });
                }
            });
        }

        wrapper.appendChild(container);

        const viewData = {
            sessionId,
            container,
            messagesEl,
            tokenBarEl: tokenBar,
            modePillEl: modePill,
            modelInfoEl,
            inputArea,
            textarea,
            sendToTerminal,
            events: [],
            uuidTypeMap: new Map(), // uuid → event type, for parent-chain detection
            toolIdMap: new Map(), // tool_id → { tool_name, title, cardEl }
            // message_id → { cardEl, contentEl, mergedMessage } for streaming dedup
            messageIdMap: new Map(),
            currentProgressWidget: null, // active blinking progress widget
            totalTokens: { input: 0, output: 0, cache_read: 0, cache_create: 0 },
            // Track which message_ids have already contributed to totalTokens
            // (assistant chunks stream multiple usage updates per message_id; only
            // the final stop_reason chunk has the authoritative usage)
            tokenAccountedIds: new Set(),
            loaded: false,
            sourceGeneration: 0,
            codexMode: false,
            codexState: null,
            codexTranscript: { order: [], items: new Map() },
            statusLabel: null,
            userScrolled: false,
            modeRefreshTimer: null,
        };

        // Track user scroll
        messagesEl.addEventListener('scroll', () => {
            const { scrollTop, scrollHeight, clientHeight } = messagesEl;
            viewData.userScrolled = scrollHeight - scrollTop - clientHeight > 50;
        });

        this.views.set(sessionId, viewData);
        return viewData;
    }

    _handleCodexSlashTextareaKey(sessionId, textarea, event) {
        const tm = window.terminalManager;
        const palette = tm?.codexSlashPalette;
        if (!tm?.isCodexAppServerSession?.(sessionId) || !palette) return false;

        const data = this._codexSlashKeyData(event);
        if ((data === '\x1b' || data === '\x03') && tm._isCodexTurnActive?.(sessionId)) {
            event.preventDefault();
            tm.stopCodexTurn?.(sessionId);
            return true;
        }

        if (palette.open) {
            if (!data) return false;
            event.preventDefault();
            palette.handleData(sessionId, data);
            return true;
        }

        if (event.key === '/' && !event.metaKey && !event.ctrlKey && !event.altKey && (textarea.value || '').length === 0) {
            event.preventDefault();
            palette.handleData(sessionId, '/');
            return true;
        }

        return false;
    }

    _openCodexSlashIfCommandPrefix(sessionId, textarea) {
        const text = (textarea?.value || '').trim();
        if (text !== '/') return false;
        const tm = window.terminalManager;
        const palette = tm?.codexSlashPalette;
        if (!tm?.isCodexAppServerSession?.(sessionId) || !palette) return false;
        palette.handleData(sessionId, '/');
        textarea.value = '';
        textarea.style.height = 'auto';
        return true;
    }

    _codexSlashKeyData(event) {
        if (event.key === 'Escape') return '\x1b';
        if (event.ctrlKey && event.key.toLowerCase() === 'c') return '\x03';
        if (event.key === 'Enter') return '\r';
        if (event.key === 'Tab') return '\t';
        if (event.key === 'Backspace') return '\x7f';
        if (event.key === 'ArrowUp') return '\x1b[A';
        if (event.key === 'ArrowDown') return '\x1b[B';
        if (event.key && event.key.length === 1 && !event.metaKey && !event.ctrlKey && !event.altKey) {
            return event.key;
        }
        return '';
    }

    /**
     * Load all events from the REST API.
     */
    async loadEvents(sessionId) {
        const view = this.views.get(sessionId);
        if (!view || view.loaded) return;
        const sourceGeneration = view.sourceGeneration;

        view.messagesEl.innerHTML = '<div class="sv-empty">Loading events...</div>';

        try {
            const resp = await fetch(`/api/sessions/${sessionId}/events`);
            const data = await resp.json();
            if (view.sourceGeneration !== sourceGeneration) return;

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

            // Preserve any pending optimistic cards across loads
            const optimistics = Array.from(view.messagesEl.querySelectorAll('.sv-optimistic'));
            view.messagesEl.innerHTML = '';

            if (events.length === 0) {
                view.messagesEl.innerHTML = '<div class="sv-empty">No events yet. Waiting for session activity...</div>';
            } else {
                for (const event of events) {
                    // REST responses are transcript history, not proof that the
                    // agent is active right now. Only live WebSocket events may
                    // change the activity indicator.
                    this._appendEventToDOM(view, event, { updateStatus: false });
                }
                this._scrollToBottom(view);
            }
            for (const el of optimistics) view.messagesEl.appendChild(el);

            view.loaded = true;

            // Start watching for real-time updates if session is active
            if (this._isSessionRunning(sessionId)) {
                this.startWatching(sessionId);
            }
        } catch (err) {
            if (view.sourceGeneration !== sourceGeneration) return;
            console.error('Failed to load events:', err);
            view.messagesEl.innerHTML = '<div class="sv-empty">Failed to load events</div>';
        }
    }

    /**
     * Reset the Claude timeline when the CLI switches transcripts via /resume
     * (or another command such as /clear). When the server already had a file
     * watcher, it replays the replacement JSONL from byte zero; otherwise fetch
     * the replacement history through the normal REST path.
     */
    handleSourceChange(sessionId, change = {}) {
        const view = this.views.get(sessionId);
        if (!view || view.codexMode) return;

        view.sourceGeneration += 1;
        this._stopStreamingPreview(view);
        view.events = [];
        view.uuidTypeMap.clear();
        view.toolIdMap.clear();
        view.messageIdMap.clear();
        view.tokenAccountedIds.clear();
        view.totalTokens = { input: 0, output: 0, cache_read: 0, cache_create: 0 };
        view.currentProgressWidget = null;
        view.messagesEl.innerHTML = change.replaying
            ? '<div class="sv-empty">Loading resumed conversation...</div>'
            : '<div class="sv-empty">Reloading resumed conversation...</div>';
        this._updateTokenBar(view);

        view.loaded = !!change.replaying;
        if (!change.replaying) {
            this.loadEvents(sessionId);
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

        // If this is the real user event matching a pending optimistic echo,
        // remove the optimistic card before rendering the authoritative one.
        if (event.type === 'user' && !event.isSidechain) {
            this._reconcileOptimisticUser(view, event);
        }

        // The real assistant event has arrived — drop the PTY streaming preview
        // so the formatted card takes over. We do this BEFORE rendering so the
        // preview never sits between user and the new assistant card.
        if (event.type === 'assistant' && view.streamingPreview) {
            this._stopStreamingPreview(view);
        }

        this._appendEventToDOM(view, event);

        // Streaming preview must always stay anchored at the bottom — moving it
        // after each append prevents new (real) events from rendering below it.
        this._pinStreamingPreviewToBottom(view);

        if (!view.userScrolled) {
            this._scrollToBottom(view);
        }
    }

    _pinStreamingPreviewToBottom(view) {
        const sp = view.streamingPreview;
        if (!sp || !sp.cardEl) return;
        if (view.messagesEl.lastElementChild !== sp.cardEl) {
            view.messagesEl.appendChild(sp.cardEl);
        }
    }

    /**
     * Show the user's message in the SV immediately, before the JSONL watcher
     * delivers the real event. The card is removed when the real event arrives
     * (matched by trimmed text, FIFO).
     */
    addOptimisticUserMessage(sessionId, text) {
        const view = this.views.get(sessionId);
        if (!view) return;
        if (!text || !text.trim()) return;

        // Remove "no events" placeholder if present
        const empty = view.messagesEl.querySelector('.sv-empty');
        if (empty) empty.remove();

        const div = document.createElement('div');
        div.className = 'sv-message sv-message--user sv-optimistic';
        div.dataset.optimisticText = text.trim();

        const header = document.createElement('div');
        header.className = 'sv-message-header';
        header.innerHTML = `
            <span class="sv-role sv-role--user">User</span>
            <span class="sv-timestamp">${this._formatTime(new Date())}</span>
        `;
        div.appendChild(header);

        const content = document.createElement('div');
        content.className = 'sv-content';
        const textEl = document.createElement('div');
        textEl.className = 'sv-text';
        textEl.textContent = text;
        content.appendChild(textEl);
        div.appendChild(content);

        view.messagesEl.appendChild(div);
        view.userScrolled = false;
        this._scrollToBottom(view);

        // Begin PTY streaming preview: until the JSONL assistant event arrives,
        // we mirror Claude Code's TUI output (ANSI-stripped) into a live preview
        // card so the user sees text growing instead of staring at a void.
        this._startStreamingPreview(view);
    }

    /**
     * Receive a raw PTY chunk (already written into xterm) and, if a streaming
     * preview is active for this session, append its visible text.
     */
    onPtyOutput(sessionId, ptyChunk) {
        const view = this.views.get(sessionId);
        if (!view || !view.streamingPreview) return;
        view.streamingPreview.rawBuffer += ptyChunk;
        // Trim to last ~32KB to avoid runaway memory
        if (view.streamingPreview.rawBuffer.length > 32768) {
            view.streamingPreview.rawBuffer =
                view.streamingPreview.rawBuffer.slice(-32768);
        }
        // Coalesce DOM updates into ~60fps
        if (view.streamingPreview.rafPending) return;
        view.streamingPreview.rafPending = true;
        requestAnimationFrame(() => {
            view.streamingPreview.rafPending = false;
            this._renderStreamingPreview(view);
        });
    }

    _startStreamingPreview(view) {
        if (view.streamingPreview) return;

        const div = document.createElement('div');
        div.className = 'sv-streaming-preview';
        div.innerHTML = `
            <div class="sv-stream-header">
                <span class="sv-stream-dot"></span>
                <span class="sv-stream-label">streaming from terminal…</span>
            </div>
            <pre class="sv-stream-body"></pre>
        `;
        view.messagesEl.appendChild(div);

        view.streamingPreview = {
            cardEl: div,
            bodyEl: div.querySelector('.sv-stream-body'),
            rawBuffer: '',
            rafPending: false,
            startedAt: Date.now(),
            // Auto-stop after 90s of silence so we never leak the preview
            timeoutHandle: setTimeout(() => this._stopStreamingPreview(view), 90000),
        };

        if (!view.userScrolled) this._scrollToBottom(view);
    }

    _stopStreamingPreview(view) {
        if (!view.streamingPreview) return;
        clearTimeout(view.streamingPreview.timeoutHandle);
        view.streamingPreview.cardEl?.remove();
        view.streamingPreview = null;
    }

    _renderStreamingPreview(view) {
        const sp = view.streamingPreview;
        if (!sp) return;

        // Strip ANSI/CSI sequences and box-drawing chrome. We want a clean
        // readable text — not a perfect TUI reproduction.
        const cleaned = sp.rawBuffer
            .replace(/\x1b\][^\x07\x1b]*(\x07|\x1b\\)/g, '')   // OSC (hyperlinks)
            .replace(/\x1b\[[\?0-9;]*[A-Za-z]/g, '')           // CSI
            .replace(/\x1b[=>]/g, '')                          // DEC modes
            .replace(/\x1b[()][AB012]/g, '')                   // SCS
            .replace(/\r/g, '')                                 // CR
            .replace(/\x07/g, '')                              // BEL
            .replace(/[\x00-\x08\x0b\x0c\x0e-\x1f]/g, '');     // other control chars

        // Show only the tail (last ~24 lines) so the preview stays focused on
        // the assistant's current response and doesn't include old scrollback.
        const lines = cleaned.split('\n');
        // Skip leading blank/box lines and prefer the bottom
        const tail = lines.slice(-40);
        // Drop pure box-drawing dividers and empty lines clumped at the start
        let firstReal = 0;
        while (firstReal < tail.length && /^[\s─━─┄┈│]*$/.test(tail[firstReal])) firstReal++;
        const visible = tail.slice(firstReal).join('\n').trimEnd();

        sp.bodyEl.textContent = visible || '…';
        if (!view.userScrolled) this._scrollToBottom(view);
    }

    /**
     * If an optimistic user card is pending and this real user event matches,
     * remove the optimistic card so the real one takes its place.
     */
    _reconcileOptimisticUser(view, event) {
        const blocks = event.message?.content_blocks || [];
        const realText = blocks
            .filter(b => b.type === 'text')
            .map(b => (b.text || '').trim())
            .join('\n')
            .trim();
        if (!realText) return;

        const optimistics = view.messagesEl.querySelectorAll('.sv-optimistic');
        for (const el of optimistics) {
            const optText = (el.dataset.optimisticText || '').trim();
            // Loose match: real msg should start with our optimistic text
            // (Claude Code occasionally appends "(via …)" suffixes)
            if (optText && (realText === optText || realText.startsWith(optText) || optText.startsWith(realText))) {
                el.remove();
                return;
            }
        }
    }

    /**
     * Append a document card directly (e.g. from WebSocket chat_doc_card event).
     */
    appendDocCard(sessionId, data) {
        const view = this.views.get(sessionId);
        if (!view) return;

        const div = document.createElement('div');
        div.className = 'sv-doc-card';

        const title = data.title || 'Document';
        const docId = data.doc_id;

        div.innerHTML = `
            <div class="sv-doc-card-icon">
                <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                    <path d="M14 2H6a2 2 0 00-2 2v16a2 2 0 002 2h12a2 2 0 002-2V8z"/>
                    <polyline points="14 2 14 8 20 8"/>
                    <line x1="9" y1="15" x2="15" y2="15"/>
                    <line x1="9" y1="11" x2="15" y2="11"/>
                </svg>
            </div>
            <div class="sv-doc-card-title">${this._escapeHtml(title)}</div>
            <button class="sv-doc-card-btn"${docId ? '' : ' disabled'}>View Document</button>
        `;

        if (docId) {
            div.querySelector('.sv-doc-card-btn').onclick = () => window.docViewer?.open(docId);
        }

        // Wrap in a message container for consistent spacing
        const wrapper = document.createElement('div');
        wrapper.className = 'sv-message sv-message--tool-results';
        const content = document.createElement('div');
        content.className = 'sv-content';
        content.appendChild(div);
        wrapper.appendChild(content);

        view.messagesEl.appendChild(wrapper);

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

        this._configureCodexView(view, false);
        view.container.classList.add('active');
        this.activeSessionId = sessionId;
        this._syncStatusIndicator(view);

        if (!view.loaded) {
            this.loadEvents(sessionId);
        }

        // Refresh mode pill from terminal buffer immediately and poll while active
        this._refreshMode(sessionId);
        if (!view.modeRefreshTimer) {
            view.modeRefreshTimer = setInterval(() => {
                if (view.container.classList.contains('active')) {
                    this._refreshMode(sessionId);
                }
            }, 1500);
        }
    }

    /**
     * Show Codex app-server transcript in the existing Structured View shell.
     * Codex does not expose Claude JSONL events, so it feeds normalized events
     * through appendCodexTranscriptEvent instead of loadEvents().
     */
    showCodex(sessionId) {
        const view = this.views.get(sessionId) || this.createView(sessionId);
        if (!view) return;

        this._configureCodexView(view, true);
        view.container.classList.add('active');
        this.activeSessionId = sessionId;
        this._syncStatusIndicator(view);
        view.loaded = true;

        if (!view.codexTranscript.order.length && !view.messagesEl.children.length) {
            view.messagesEl.innerHTML = '<div class="sv-empty">Waiting for Codex activity</div>';
        }

        if (view.textarea) {
            requestAnimationFrame(() => view.textarea.focus());
        }
    }

    _configureCodexView(view, enabled) {
        view.codexMode = !!enabled;
        const pill = view.modePillEl;
        if (!pill) return;

        if (enabled) {
            pill.dataset.mode = 'codex';
            pill.title = 'Codex app-server';
            pill.querySelector('.sv-mode-icon').textContent = '>';
            pill.querySelector('.sv-mode-label').textContent = 'codex';
            pill.querySelector('.sv-mode-hint').textContent = 'app-server';
            if (view.modelInfoEl?.hidden) {
                this._updateCodexModelInfo(view, view.codexState);
            }
        } else {
            pill.title = 'Click to cycle Claude Code modes (Shift+Tab)';
            pill.querySelector('.sv-mode-icon').textContent = '⏵';
            pill.querySelector('.sv-mode-label').textContent = 'default';
            pill.querySelector('.sv-mode-hint').textContent = 'shift+tab';
            delete pill.dataset.mode;
            if (view.modelInfoEl) view.modelInfoEl.hidden = true;
        }
    }

    updateCodexState(sessionId, state) {
        const view = this.views.get(sessionId) || this.createView(sessionId);
        if (!view || !state) return;
        view.codexState = state;
        this._configureCodexView(view, true);

        if (state.model && view.modePillEl) {
            view.modePillEl.title = [state.model, state.approvalPolicy, state.sandboxMode]
                .filter(Boolean)
                .join(' / ');
        }
        this._updateCodexModelInfo(view, state);

        view.totalTokens.input = Number(state.lastInputTokens || 0);
        view.totalTokens.output = Number(state.lastOutputTokens || 0);
        this._updateTokenBar(view);

        const phase = state.phase || '';
        const label = ({
            thinking: 'thinking',
            working: 'working',
            responding: 'responding',
            running_command: 'using tools',
            editing: 'using tools',
            starting: 'starting',
        })[phase] || null;

        const stateEl = window.innerWidth <= 768
            ? document.getElementById('mobile-input-state')
            : view.inputArea?.querySelector('.sv-input-state');
        if (stateEl) {
            this._setStatusIndicator(view, stateEl, label);
        }
    }

    _updateCodexModelInfo(view, state) {
        const modelInfoEl = view?.modelInfoEl;
        if (!modelInfoEl) return;

        modelInfoEl.hidden = !view.codexMode;
        if (!view.codexMode) return;

        const model = this._codexModelLabel(state?.model);
        const reasoning = this._codexReasoningLabel(state?.reasoningEffort);
        const modelEl = modelInfoEl.querySelector('[data-codex-model]');
        const reasoningEl = modelInfoEl.querySelector('[data-codex-reasoning]');

        if (modelEl) modelEl.textContent = model;
        if (reasoningEl) reasoningEl.textContent = reasoning;

        const details = [
            `Model: ${model}`,
            `Reasoning: ${reasoning}`,
            state?.serviceTier ? `Service tier: ${state.serviceTier}` : '',
            state?.approvalPolicy ? `Approval: ${state.approvalPolicy}` : '',
            state?.sandboxMode ? `Sandbox: ${state.sandboxMode}` : '',
        ].filter(Boolean);
        modelInfoEl.title = details.join(' / ');
    }

    _codexModelLabel(model) {
        return String(model || '').trim() || 'default';
    }

    _codexReasoningLabel(effort) {
        const value = String(effort || '').trim().toLowerCase();
        if (!value) return 'default';
        return ({
            minimal: 'minimal',
            low: 'low',
            medium: 'medium',
            high: 'high',
            xhigh: 'extra high',
            'x-high': 'extra high',
            'extra-high': 'extra high',
            extra_high: 'extra high',
            'extra high': 'extra high',
        })[value] || value.replace(/[_-]+/g, ' ');
    }

    resetCodexTranscript(sessionId) {
        const view = this.views.get(sessionId) || this.createView(sessionId);
        if (!view) return;

        this._configureCodexView(view, true);
        view.loaded = true;
        view.events = [];
        view.uuidTypeMap.clear();
        view.toolIdMap.clear();
        view.messageIdMap.clear();
        view.tokenAccountedIds.clear();
        view.totalTokens = { input: 0, output: 0, cache_read: 0, cache_create: 0 };
        view.codexTranscript = { order: [], items: new Map() };
        view.currentProgressWidget = null;
        view.messagesEl.innerHTML = '<div class="sv-empty">Waiting for Codex activity</div>';
        this._updateTokenBar(view);
    }

    /**
     * Render a full transcript snapshot in one batch. Merges all delta chunks
     * into complete items BEFORE touching the DOM, then renders each card
     * exactly once and scrolls once at the end. Feeding snapshot events through
     * appendCodexTranscriptEvent instead re-renders a card's entire markdown
     * for every chunk it ever streamed (quadratic), freezing the tab on long
     * sessions.
     */
    loadCodexTranscriptSnapshot(sessionId, transcript) {
        const events = Array.isArray(transcript?.events) ? transcript.events : null;
        if (!events) return false;

        const view = this.views.get(sessionId) || this.createView(sessionId);
        if (!view) return false;

        this.resetCodexTranscript(sessionId);

        for (const event of events) {
            if (event && event.id) this._upsertCodexTranscriptItem(view, event);
        }
        if (!view.codexTranscript.order.length) return true;

        view.messagesEl.innerHTML = '';
        if (transcript.truncated) {
            const notice = document.createElement('div');
            notice.className = 'sv-transcript-notice';
            notice.textContent = 'Older activity trimmed — showing the most recent transcript.';
            view.messagesEl.appendChild(notice);
        }
        for (const id of view.codexTranscript.order) {
            const item = view.codexTranscript.items.get(id);
            const normalized = this._codexTranscriptItemToEvent(item);
            if (normalized) this._appendEventToDOM(view, normalized, { updateStatus: false });
        }
        this._scrollToBottom(view);
        return true;
    }

    appendCodexTranscriptEvent(sessionId, event) {
        const view = this.views.get(sessionId) || this.createView(sessionId);
        if (!view || !event || !event.id) return;

        this._configureCodexView(view, true);
        view.loaded = true;

        const item = this._upsertCodexTranscriptItem(view, event);
        const normalized = this._codexTranscriptItemToEvent(item);
        if (!normalized) return;

        this.appendEvent(sessionId, normalized);
    }

    _upsertCodexTranscriptItem(view, event) {
        let item = view.codexTranscript.items.get(event.id);
        if (!item) {
            item = {
                id: event.id,
                kind: event.kind || 'status',
                text: '',
                title: event.title || '',
                command: event.command || '',
                status: event.status || '',
                createdAt: event.created_at || event.createdAt || new Date().toISOString(),
            };
            view.codexTranscript.items.set(event.id, item);
            view.codexTranscript.order.push(event.id);
        }

        if (event.kind) item.kind = event.kind;
        if (event.title) item.title = event.title;
        if (event.command) item.command = event.command;
        if (event.status) item.status = event.status;
        if (event.created_at || event.createdAt) item.createdAt = event.created_at || event.createdAt;
        if (event.append) item.text += event.text || '';
        else if (typeof event.text === 'string') item.text = event.text;

        return item;
    }

    _codexTranscriptItemToEvent(item) {
        const timestamp = item.createdAt || new Date().toISOString();
        const uuid = `codex-${item.id}`;
        const messageId = `codex-msg-${item.id}`;
        // Codex transcript rows are durable log entries, not live Claude JSONL
        // stream chunks. Treat them as complete so historical assistant cards do
        // not keep the Structured View streaming caret blinking indefinitely.
        const stopReason = 'end_turn';
        const text = item.text || item.title || '';

        if (item.kind === 'user') {
            return {
                type: 'user',
                uuid,
                timestamp,
                message: {
                    role: 'user',
                    content_blocks: [{ type: 'text', text }],
                },
            };
        }

        if (item.kind === 'command' || item.kind === 'file') {
            const isFile = item.kind === 'file';
            const toolId = `codex-tool-${item.id}`;
            const toolName = isFile ? 'Edit' : 'Bash';
            const blocks = [{
                type: 'tool_use',
                tool_id: toolId,
                tool_name: toolName,
                tool_input: isFile
                    ? { file_path: item.command || item.title || 'file change', status: item.status || undefined }
                    : { command: item.command || text },
            }];

            if (item.text) {
                blocks.push({
                    type: 'tool_result',
                    tool_id: toolId,
                    content: item.text,
                    is_error: item.status === 'failed',
                });
            }

            return {
                type: 'assistant',
                uuid,
                timestamp,
                message: {
                    role: 'assistant',
                    model: 'codex',
                    message_id: messageId,
                    stop_reason: stopReason,
                    content_blocks: blocks,
                },
            };
        }

        const blockType = item.kind === 'reasoning' ? 'thinking' : 'text';
        const prefix = this._codexKindPrefix(item.kind, item.title);
        return {
            type: 'assistant',
            uuid,
            timestamp,
            message: {
                role: 'assistant',
                model: 'codex',
                message_id: messageId,
                stop_reason: stopReason,
                content_blocks: [{
                    type: blockType,
                    text: blockType === 'thinking' ? text : `${prefix}${text}`.trim(),
                }],
            },
        };
    }

    _codexKindPrefix(kind, title) {
        if (!kind || kind === 'assistant') return '';
        if (kind === 'reasoning') return '';
        const label = title || ({
            plan: 'Plan',
            status: 'Status',
            warning: 'Warning',
            error: 'Error',
            tokens: 'Tokens',
        }[kind] || '');
        return label ? `${label}: ` : '';
    }

    /**
     * Detect Claude Code mode by scanning the xterm buffer for status-bar
     * indicators ("plan mode on", "accept edits", etc) and update the pill.
     */
    _refreshMode(sessionId) {
        const view = this.views.get(sessionId);
        if (!view?.modePillEl) return;
        if (view.codexMode) return;

        const tm = window.terminalManager;
        const mode = tm?.detectClaudeMode?.(sessionId) || { key: 'default', label: 'default', icon: '⏵' };

        const pill = view.modePillEl;
        if (pill.dataset.mode === mode.key) return;
        pill.dataset.mode = mode.key;
        pill.querySelector('.sv-mode-icon').textContent = mode.icon;
        pill.querySelector('.sv-mode-label').textContent = mode.label;
    }

    _scheduleModeRefresh(view) {
        if (!view?.modePillEl) return;
        clearTimeout(view._modeRefreshSoon);
        view._modeRefreshSoon = setTimeout(() => {
            this._refreshMode(this.activeSessionId);
        }, 200);
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
            if (view._statusIdleTimer) {
                clearTimeout(view._statusIdleTimer);
                view._statusIdleTimer = null;
            }
            if (view.modeRefreshTimer) {
                clearInterval(view.modeRefreshTimer);
                view.modeRefreshTimer = null;
            }
            this.stopWatching(sessionId);
            view.container.remove();
            this.views.delete(sessionId);
        }
    }

    // --- Internal rendering ---

    _appendEventToDOM(view, event, options = {}) {
        view.events.push(event);

        // Track UUID → type for parent-chain detection
        if (event.uuid) {
            view.uuidTypeMap.set(event.uuid, event.type);
        }

        // Progress events use a blinking widget instead of individual lines
        if (event.type === 'progress') {
            this._updateProgressWidget(view, event);
        } else {
            // Non-progress event: finalize any active progress widget
            view.currentProgressWidget = null;

            // Streaming: assistant events stream as partial chunks sharing one
            // message_id. The watcher delivers raw chunks. Merge into an existing
            // card if we've seen this id before so text/tools grow in-place.
            const msgID = event.type === 'assistant' && event.message?.message_id
                ? event.message.message_id
                : null;
            if (msgID && view.messageIdMap.has(msgID)) {
                this._updateAssistantCard(view, msgID, event);
            } else {
                const el = this._renderEvent(view, event);
                if (el) {
                    view.messagesEl.appendChild(el);
                    if (msgID && event.type === 'assistant') {
                        const wrapper = el.classList?.contains('sv-message')
                            ? el
                            : el.querySelector?.('.sv-message--assistant');
                        if (wrapper) {
                            view.messageIdMap.set(msgID, {
                                cardEl: wrapper,
                                contentEl: wrapper.querySelector('.sv-content'),
                                merged: this._cloneMessage(event.message),
                            });
                            this._setStreamingState(wrapper, !event.message?.stop_reason);
                        }
                    }
                }
            }
        }

        // Update token counter — only credit a message_id once (use final chunk usage)
        if (event.message?.usage) {
            const msgID = event.message.message_id;
            if (!msgID || !view.tokenAccountedIds.has(msgID)) {
                const u = event.message.usage;
                view.totalTokens.input += u.input_tokens || 0;
                view.totalTokens.output += u.output_tokens || 0;
                view.totalTokens.cache_read += u.cache_read_tokens || 0;
                view.totalTokens.cache_create += u.cache_creation_tokens || 0;
                this._updateTokenBar(view);
                if (msgID && event.message.stop_reason) {
                    view.tokenAccountedIds.add(msgID);
                }
            }
        }

        // Update input state hint for live events only. Historical transcript
        // replay must not make an idle session appear active.
        if (options.updateStatus !== false) {
            this._updateInputState(view, event);
        }

        // Streaming activity → refresh mode soon (TUI may have repainted)
        if (event.type === 'assistant') {
            this._scheduleModeRefresh(view);
        }
    }

    _cloneMessage(msg) {
        if (!msg) return null;
        return {
            role: msg.role,
            model: msg.model,
            message_id: msg.message_id,
            stop_reason: msg.stop_reason,
            usage: msg.usage ? { ...msg.usage } : null,
            content_blocks: (msg.content_blocks || []).map(b => ({ ...b })),
        };
    }

    /**
     * Merge a partial assistant chunk into the existing card.
     * Mirrors backend mergeAssistantMessage: keeps one block per type
     * (text/thinking/tool_use) and updates metadata from the newer chunk.
     */
    _updateAssistantCard(view, msgID, event) {
        const slot = view.messageIdMap.get(msgID);
        if (!slot) return;

        const newMsg = event.message;
        if (!newMsg) return;

        // Merge content blocks by stable identity. Claude can rewrite the
        // same message_id with richer content, and a message may contain
        // multiple tool_use/text blocks, so type alone is not enough.
        const seenKeys = new Map(); // key -> index in merged.content_blocks
        const existingOrdinals = new Map();
        slot.merged.content_blocks.forEach((b, i) => {
            seenKeys.set(this._contentBlockMergeKey(b, existingOrdinals), i);
        });
        const newOrdinals = new Map();
        for (const b of (newMsg.content_blocks || [])) {
            const key = this._contentBlockMergeKey(b, newOrdinals);
            if (seenKeys.has(key)) {
                const idx = seenKeys.get(key);
                if (this._shouldReplaceContentBlock(slot.merged.content_blocks[idx], b)) {
                    // Replace with newer version (text might have grown)
                    slot.merged.content_blocks[idx] = { ...b };
                }
            } else {
                seenKeys.set(key, slot.merged.content_blocks.length);
                slot.merged.content_blocks.push({ ...b });
            }
        }

        // Update metadata
        if (newMsg.usage) slot.merged.usage = { ...newMsg.usage };
        if (newMsg.stop_reason) slot.merged.stop_reason = newMsg.stop_reason;
        if (newMsg.model) slot.merged.model = newMsg.model;

        // Re-render content area (cheap — small messages)
        this._renderAssistantContent(view, slot.contentEl, slot.merged);

        // Update header tokens & streaming state
        const u = slot.merged.usage;
        const tokensEl = slot.cardEl.querySelector('.sv-tokens-inline');
        if (tokensEl && u) {
            tokensEl.textContent = `in:${this._formatNum(u.input_tokens)} out:${this._formatNum(u.output_tokens)}`;
        }
        this._setStreamingState(slot.cardEl, !slot.merged.stop_reason);
    }

    _contentBlockMergeKey(block, ordinals) {
        if ((block.type === 'tool_use' || block.type === 'tool_result') && block.tool_id) {
            return `${block.type}:${block.tool_id}`;
        }
        const base = block.type || 'unknown';
        const idx = ordinals.get(base) || 0;
        ordinals.set(base, idx + 1);
        return `${base}:${idx}`;
    }

    _shouldReplaceContentBlock(existing, incoming) {
        if (!this._contentBlockHasValue(incoming) && this._contentBlockHasValue(existing)) {
            return false;
        }
        return true;
    }

    _contentBlockHasValue(block) {
        return !!(
            block?.text ||
            block?.tool_name ||
            block?.tool_id ||
            block?.tool_input ||
            block?.content
        );
    }

    _setStreamingState(cardEl, streaming) {
        if (!cardEl) return;
        cardEl.classList.toggle('sv-streaming', !!streaming);
    }

    _renderEvent(view, event) {
        switch (event.type) {
            case 'user':
                return this._renderUserMessage(view, event);
            case 'assistant':
                return this._renderAssistantMessage(view, event);
            default:
                return null;
        }
    }

    _renderUserMessage(view, event) {
        const msg = event.message;
        if (!msg) return null;

        const blocks = msg.content_blocks || [];
        const textBlocks = blocks.filter(b => b.type === 'text');
        const toolResultBlocks = blocks.filter(b => b.type === 'tool_result');

        // Detect system-injected user messages:
        // - isSidechain: true → agent/subagent injections
        // - Parent is a user event → skill prompts (skill content follows tool_result in same user chain)
        const parentType = event.parent_uuid ? view.uuidTypeMap.get(event.parent_uuid) : null;
        const isRealUser = !event.isSidechain && parentType !== 'user';

        // If only tool_result blocks, render as a system-style card (not user)
        if (textBlocks.length === 0 && toolResultBlocks.length > 0) {
            return this._renderToolResultsMessage(view, event, toolResultBlocks);
        }

        const fragment = document.createDocumentFragment();

        // Render tool_result blocks first as a separate system card
        if (toolResultBlocks.length > 0) {
            const trMsg = this._renderToolResultsMessage(view, event, toolResultBlocks);
            if (trMsg) fragment.appendChild(trMsg);
        }

        // Render text blocks
        if (textBlocks.length > 0) {
            const div = document.createElement('div');
            div.className = isRealUser
                ? 'sv-message sv-message--user'
                : 'sv-message sv-message--assistant';

            const header = document.createElement('div');
            header.className = 'sv-message-header';
            header.innerHTML = isRealUser
                ? `<span class="sv-role sv-role--user">User</span>
                   <span class="sv-timestamp">${this._formatTime(event.timestamp)}</span>`
                : `<span class="sv-role sv-role--assistant">System</span>
                   <span class="sv-timestamp">${this._formatTime(event.timestamp)}</span>`;
            div.appendChild(header);

            const content = document.createElement('div');
            content.className = 'sv-content';

            for (const block of textBlocks) {
                const blockEl = this._renderContentBlock(view, block);
                if (blockEl) content.appendChild(blockEl);
            }

            div.appendChild(content);
            fragment.appendChild(div);
        }

        return fragment;
    }

    _renderToolResultsMessage(view, event, toolResultBlocks) {
        const div = document.createElement('div');
        div.className = 'sv-message sv-message--tool-results';

        const content = document.createElement('div');
        content.className = 'sv-content';

        for (const block of toolResultBlocks) {
            const blockEl = this._renderContentBlock(view, block);
            if (blockEl) content.appendChild(blockEl);
        }

        div.appendChild(content);
        return div;
    }

    _renderAssistantMessage(view, event) {
        const msg = event.message;
        if (!msg) return null;

        const div = document.createElement('div');
        div.className = 'sv-message sv-message--assistant';

        const header = document.createElement('div');
        header.className = 'sv-message-header';

        let headerHTML = '<span class="sv-role sv-role--assistant">Assistant</span>';
        if (msg.model) {
            headerHTML += `<span class="sv-model">${this._escapeHtml(this._shortModel(msg.model))}</span>`;
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
        this._renderAssistantContent(view, content, msg);
        div.appendChild(content);
        return div;
    }

    /**
     * Render assistant content blocks into `contentEl`, replacing children.
     * Reuses existing tool_use/tool_result rendering. Called both on initial
     * render and on partial-chunk updates (streaming).
     */
    _renderAssistantContent(view, contentEl, msg) {
        // Capture which tool toggles were expanded so streaming re-renders
        // don't collapse the user's open panes.
        const expandedKeys = new Set();
        contentEl.querySelectorAll('[data-tool-key].expanded').forEach(el => {
            expandedKeys.add(el.getAttribute('data-tool-key'));
        });

        contentEl.innerHTML = '';
        for (const block of (msg.content_blocks || [])) {
            const blockEl = this._renderContentBlock(view, block);
            if (!blockEl) continue;
            if (block.type === 'tool_use' && block.tool_id) {
                blockEl.setAttribute?.('data-tool-key', `tool_use:${block.tool_id}`);
                if (expandedKeys.has(`tool_use:${block.tool_id}`)) {
                    blockEl.classList?.add('expanded');
                }
            }
            contentEl.appendChild(blockEl);
        }
    }

    _shortModel(model) {
        if (!model) return '';
        // claude-opus-4-7-something → opus 4.7
        const m = model.match(/claude-(opus|sonnet|haiku)-(\d+)-(\d+)/i);
        if (m) return `${m[1].toLowerCase()} ${m[2]}.${m[3]}`;
        return model.replace(/^claude-/, '');
    }

    _renderContentBlock(view, block) {
        switch (block.type) {
            case 'text':
                return this._renderTextBlock(block);
            case 'tool_use':
                if (block.tool_name === 'AskUserQuestion') {
                    return this._renderAskUserBlock(block, view);
                }
                if (block.tool_name === 'TodoWrite') {
                    return this._renderTodoWriteBlock(block);
                }
                if (block.tool_name?.endsWith('openpoet_create_document')) {
                    return this._renderDocCardBlock(view, block);
                }
                return this._renderToolUseBlock(block);
            case 'tool_result': {
                // If this tool_result corresponds to a create_document, render as doc card update
                const meta = view?.toolIdMap?.get(block.tool_id);
                if (meta?.tool_name?.endsWith('openpoet_create_document')) {
                    const match = (block.content || '').match(/\/app\/doc\/([a-zA-Z0-9]+)/);
                    if (match && meta.cardEl) {
                        const btn = meta.cardEl.querySelector('.sv-doc-card-btn');
                        if (btn) {
                            btn.disabled = false;
                            btn.onclick = () => window.docViewer?.open(match[1]);
                        }
                    }
                    return null; // suppress generic tool_result for doc cards
                }
                if (meta?.tool_name === 'AskUserQuestion' && meta.cardEl) {
                    this._applyAskUserAnswers(meta, block.content || '');
                    return null; // suppress generic tool_result for ask-user cards
                }
                return this._renderToolResultBlock(block);
            }
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
        div.className = 'sv-tool-use sv-tool-compact';

        const summary = this._toolInputSummary(block.tool_name, block.tool_input);
        const icon = this._toolIcon(block.tool_name);

        div.innerHTML = `
            <button class="sv-tool-toggle">
                <span class="sv-tool-icon">${icon}</span>
                <span class="sv-tool-name">${this._escapeHtml(block.tool_name || 'Tool')}</span>
                <span class="sv-tool-sep">·</span>
                <span class="sv-tool-summary">${this._escapeHtml(summary)}</span>
                <span class="sv-tool-chevron">▸</span>
            </button>
            <div class="sv-tool-body">
                <pre class="sv-tool-input">${this._escapeHtml(this._formatToolInput(block.tool_input))}</pre>
            </div>
        `;

        div.querySelector('.sv-tool-toggle').addEventListener('click', () => {
            div.classList.toggle('expanded');
        });

        return div;
    }

    _toolIcon(name) {
        switch (name) {
            case 'Bash': return '⌨';
            case 'Read': return '📄';
            case 'Write': return '✎';
            case 'Edit': return '✎';
            case 'Grep': return '🔍';
            case 'Glob': return '🔍';
            case 'WebFetch': return '🌐';
            case 'WebSearch': return '🌐';
            case 'Task':
            case 'Agent': return '🤖';
            case 'Skill': return '✨';
            case 'TaskCreate':
            case 'TaskUpdate':
            case 'TaskList': return '✓';
            case 'AskUserQuestion': return '❓';
            default: return '·';
        }
    }

    _renderDocCardBlock(view, block) {
        const title = block.tool_input?.title || 'Document';
        const div = document.createElement('div');
        div.className = 'sv-doc-card';

        div.innerHTML = `
            <div class="sv-doc-card-icon">
                <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                    <path d="M14 2H6a2 2 0 00-2 2v16a2 2 0 002 2h12a2 2 0 002-2V8z"/>
                    <polyline points="14 2 14 8 20 8"/>
                    <line x1="9" y1="15" x2="15" y2="15"/>
                    <line x1="9" y1="11" x2="15" y2="11"/>
                </svg>
            </div>
            <div class="sv-doc-card-title">${this._escapeHtml(title)}</div>
            <button class="sv-doc-card-btn" disabled>View Document</button>
        `;

        // Store reference so tool_result can enable the button with doc_id
        if (view?.toolIdMap) {
            view.toolIdMap.set(block.tool_id, {
                tool_name: block.tool_name,
                title,
                cardEl: div
            });
        }

        return div;
    }

    /**
     * Extract readable text from tool result content.
     * Handles JSON arrays [{text:"..."}], objects {text:"..."}, and plain strings.
     */
    _extractResultText(content) {
        if (!content) return null;

        const trimmed = content.trim();

        // Strategy 1: Parse as JSON and extract text fields
        try {
            const parsed = JSON.parse(trimmed);
            if (Array.isArray(parsed)) {
                const texts = parsed
                    .filter(item => item && typeof item.text === 'string')
                    .map(item => item.text);
                if (texts.length > 0) return this._cleanResultText(texts.join('\n\n'));
            }
            if (parsed && typeof parsed === 'object' && typeof parsed.text === 'string') {
                return this._cleanResultText(parsed.text);
            }
        } catch (e) {
            // not valid JSON, try fallback strategies
        }

        // Strategy 2: Content looks like JSON array with text keys but parsing failed
        // (handles malformed JSON from streaming, truncation, etc.)
        if (trimmed.startsWith('[{') && trimmed.includes('"text"')) {
            try {
                const textMatches = [];
                const re = /"text"\s*:\s*"((?:[^"\\]|\\.)*)"/g;
                let match;
                while ((match = re.exec(trimmed)) !== null) {
                    // Unescape JSON string escapes
                    const raw = match[1]
                        .replace(/\\n/g, '\n')
                        .replace(/\\t/g, '\t')
                        .replace(/\\"/g, '"')
                        .replace(/\\\\/g, '\\');
                    textMatches.push(raw);
                }
                if (textMatches.length > 0) {
                    return this._cleanResultText(textMatches.join('\n\n'));
                }
            } catch (e) {
                // regex fallback also failed
            }
        }

        return null;
    }

    /**
     * Clean extracted text by removing metadata noise (agentId, usage tags).
     */
    _cleanResultText(text) {
        if (!text) return null;
        return text
            // Remove agentId lines
            .replace(/\n*agentId:\s*\S+\s*(\(for resuming[^)]*\))?/g, '')
            // Remove <usage>...</usage> blocks
            .replace(/\n*<usage>[\s\S]*?<\/usage>/g, '')
            .trim() || null;
    }

    _renderToolResultBlock(block) {
        const div = document.createElement('div');
        div.className = 'sv-tool-result sv-tool-compact';
        if (block.is_error) div.classList.add('sv-tool-result--error');

        const content = block.content || '';
        const extractedText = this._extractResultText(content);
        const errorClass = block.is_error ? ' sv-tool-output--error' : '';
        const icon = block.is_error ? '✕' : '✓';

        // Line count hint for the inline summary
        const sourceForLines = extractedText || content;
        const lineCount = sourceForLines ? sourceForLines.split('\n').filter(l => l.length).length : 0;
        const lineHint = lineCount > 1 ? `${lineCount} lines` : '';

        if (extractedText && !block.is_error) {
            const preview = this._plainTextPreview(extractedText, 90);

            div.innerHTML = `
                <button class="sv-tool-toggle">
                    <span class="sv-tool-icon">${icon}</span>
                    <span class="sv-tool-name">result</span>
                    <span class="sv-tool-sep">·</span>
                    <span class="sv-tool-summary">${this._escapeHtml(preview)}</span>
                    ${lineHint ? `<span class="sv-tool-meta">${lineHint}</span>` : ''}
                    <span class="sv-tool-chevron">▸</span>
                </button>
                <div class="sv-tool-body">
                    <div class="sv-text markdown-body sv-result-text"></div>
                </div>
            `;

            const textEl = div.querySelector('.sv-result-text');
            if (typeof marked !== 'undefined') {
                try {
                    textEl.innerHTML = marked.parse(extractedText);
                } catch (e) {
                    textEl.textContent = extractedText;
                }
            } else {
                textEl.textContent = extractedText;
            }
        } else {
            const displayContent = this._formatJsonContent(content);
            const preview = this._plainTextPreview(content, 90);

            div.innerHTML = `
                <button class="sv-tool-toggle">
                    <span class="sv-tool-icon">${icon}</span>
                    <span class="sv-tool-name">result</span>
                    <span class="sv-tool-sep">·</span>
                    <span class="sv-tool-summary">${this._escapeHtml(preview)}</span>
                    ${lineHint ? `<span class="sv-tool-meta">${lineHint}</span>` : ''}
                    <span class="sv-tool-chevron">▸</span>
                </button>
                <div class="sv-tool-body">
                    <pre class="sv-tool-output${errorClass}">${this._escapeHtml(displayContent)}</pre>
                </div>
            `;
        }

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

    _updateProgressWidget(view, event) {
        const p = event.progress;
        const text = p
            ? `${p.hook_event || p.type || ''} ${p.hook_name || ''}`.trim()
            : '';

        if (view.currentProgressWidget) {
            // Update existing widget
            const w = view.currentProgressWidget;
            w.count++;
            w.el.querySelector('.sv-progress-widget-text').textContent = text;
            w.el.querySelector('.sv-progress-widget-count').textContent = w.count;

            // Re-trigger blink animation
            const dot = w.el.querySelector('.sv-progress-widget-dot');
            dot.classList.remove('sv-blink');
            void dot.offsetWidth; // force reflow
            dot.classList.add('sv-blink');
        } else {
            // Create new widget
            const div = document.createElement('div');
            div.className = 'sv-progress-widget';
            div.innerHTML = `
                <span class="sv-progress-widget-dot sv-blink"></span>
                <span class="sv-progress-widget-text">${this._escapeHtml(text)}</span>
                <span class="sv-progress-widget-count">1</span>
            `;
            view.messagesEl.appendChild(div);
            view.currentProgressWidget = { el: div, count: 1 };
        }
    }

    _renderTodoWriteBlock(block) {
        const todos = (block.tool_input && block.tool_input.todos) || [];
        if (!Array.isArray(todos) || todos.length === 0) {
            return this._renderToolUseBlock(block);
        }

        const completed = todos.filter(t => t.status === 'completed').length;
        const total = todos.length;
        const allDone = completed === total;

        const div = document.createElement('div');
        div.className = 'sv-todo-list';
        if (allDone) div.classList.add('sv-todo-list--done');

        const header = document.createElement('div');
        header.className = 'sv-todo-header';
        header.innerHTML = `
            <span class="sv-todo-header-icon">${allDone ? '&#10003;' : '&#9776;'}</span>
            <span class="sv-todo-header-title">Task list</span>
            <span class="sv-todo-header-count">${completed}/${total}</span>
        `;
        div.appendChild(header);

        const list = document.createElement('ul');
        list.className = 'sv-todo-items';

        for (const todo of todos) {
            const status = todo.status || 'pending';
            const li = document.createElement('li');
            li.className = `sv-todo-item sv-todo-${status}`;

            const text = (status === 'in_progress' && todo.activeForm)
                ? todo.activeForm
                : (todo.content || '');

            let iconHTML;
            if (status === 'completed') {
                iconHTML = '<span class="sv-todo-check sv-todo-check--done">&#10003;</span>';
            } else if (status === 'in_progress') {
                iconHTML = '<span class="sv-todo-check sv-todo-check--active"></span>';
            } else {
                iconHTML = '<span class="sv-todo-check sv-todo-check--pending"></span>';
            }

            li.innerHTML = `${iconHTML}<span class="sv-todo-text">${this._escapeHtml(text)}</span>`;
            list.appendChild(li);
        }

        div.appendChild(list);
        return div;
    }

    _renderAskUserBlock(block, view) {
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
            qDiv.dataset.question = q.question || '';

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
                    optEl.dataset.label = opt.label || '';

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

        // Register so the matching tool_result can highlight the chosen options
        if (view?.toolIdMap && block.tool_id) {
            view.toolIdMap.set(block.tool_id, {
                tool_name: 'AskUserQuestion',
                cardEl: div,
                questions,
            });
        }

        return div;
    }

    /**
     * Parse the tool_result string from AskUserQuestion and update the card:
     * highlight the option that matches each answer; if the answer doesn't
     * match any option label, render it as a free-text answer below.
     */
    /**
     * Split an AskUserQuestion tool_result into {q, a} pairs.
     *
     * Claude Code renders each pair as `"<question>"="<answer>"` joined with
     * ", " and escapes nothing, so a question or label containing a double
     * quote makes the shape ambiguous. Locating the known question texts turns
     * it back into an unambiguous parse: each answer is whatever sits between
     * its own `"=` and the start of the next pair.
     */
    _parseAskUserPairs(content, questions) {
        const anchors = [];
        for (const q of (Array.isArray(questions) ? questions : [])) {
            const text = q && q.question;
            if (typeof text !== 'string' || text === '') continue;
            const marker = `"${text}"="`;
            const at = content.indexOf(marker);
            if (at >= 0) anchors.push({ q: text, start: at, valueAt: at + marker.length });
        }

        if (anchors.length > 0) {
            anchors.sort((a, b) => a.start - b.start);
            return anchors.map((anchor, i) => {
                const next = anchors[i + 1];
                // The answer ends at the closing quote before the ", " that
                // introduces the next pair — or the last quote of the run.
                const slice = content.slice(anchor.valueAt, next ? next.start : undefined);
                const end = next ? slice.lastIndexOf('", ') : slice.lastIndexOf('"');
                return { q: anchor.q, a: end >= 0 ? slice.slice(0, end) : slice };
            });
        }

        // No known question matched (older transcript, truncated result, …).
        const pairs = [];
        const re = /"([^"]+)"\s*=\s*"((?:[^"\\]|\\.)*)"/g;
        let m;
        while ((m = re.exec(content)) !== null) {
            pairs.push({ q: m[1], a: m[2] });
        }
        return pairs;
    }

    _applyAskUserAnswers(meta, content) {
        if (!content || !meta?.cardEl) return;

        // Format: ...answered: "Q1"="A1", "Q2"="A2". ...
        // Neither the question nor the answer is escaped, so a quote inside
        // either one derails a generic regex. Anchor on the question texts we
        // already know from the tool_use block instead, and only fall back to
        // the regex when none of them appear.
        const pairs = this._parseAskUserPairs(content, meta.questions);
        if (pairs.length === 0) return;

        const qNodes = Array.from(meta.cardEl.querySelectorAll('.sv-ask-question'));

        // Group answers by question (multiSelect: multiple pairs share same Q)
        const byQ = new Map();
        for (const { q, a } of pairs) {
            if (!byQ.has(q)) byQ.set(q, []);
            byQ.get(q).push(a);
        }

        for (const [questionText, answers] of byQ.entries()) {
            const qNode = qNodes.find(n => (n.dataset.question || '') === questionText);
            if (!qNode) continue;

            const optionEls = Array.from(qNode.querySelectorAll('.sv-ask-option'));
            const labels = optionEls.map(el => (el.dataset.label || '').trim());

            // Multi-select answers arrive as one ", "-joined string. Split them
            // only when every part is a real option label, so a free-text answer
            // that happens to contain ", " is left whole.
            const expanded = [];
            for (const answer of answers) {
                const parts = answer.split(', ');
                if (parts.length > 1 && !labels.includes(answer.trim()) && parts.every(p => labels.includes(p.trim()))) {
                    expanded.push(...parts);
                } else {
                    expanded.push(answer);
                }
            }

            const unmatched = [];

            for (const answer of expanded) {
                // Strip the " (Recommended)" suffix the platform appends to the
                // first option's label when echoing the answer back.
                const cleaned = answer.replace(/\s*\(Recommended\)\s*$/, '').trim();
                const matchEl = optionEls.find(el => {
                    const lbl = (el.dataset.label || '').trim();
                    return lbl && (lbl === cleaned || lbl === answer.trim());
                });
                if (matchEl) {
                    matchEl.classList.add('sv-ask-option--selected');
                } else if (answer.trim()) {
                    unmatched.push(answer.trim());
                }
            }

            if (unmatched.length > 0) {
                // Drop next to the highlighted option, or at the end if none
                const lastSelected = qNode.querySelector('.sv-ask-option--selected:last-of-type');
                const answerEl = document.createElement('div');
                answerEl.className = 'sv-ask-answer-text';
                answerEl.textContent = unmatched.join('\n');
                if (lastSelected) {
                    lastSelected.insertAdjacentElement('afterend', answerEl);
                } else {
                    qNode.appendChild(answerEl);
                }
            }
        }

        meta.cardEl.classList.add('sv-ask-user--answered');
    }

    _updateInputState(view, event) {
        const isMobile = window.innerWidth <= 768;
        const stateEl = this._getStatusElement(view);
        if (!stateEl) return;

        const msg = event.message;
        let label = null; // null => idle

        if (event.type === 'progress') {
            label = 'working';
        } else if (msg) {
            if (msg.role === 'assistant') {
                if (msg.stop_reason === 'end_turn') {
                    label = null;
                    const textarea = isMobile
                        ? document.getElementById('mobile-terminal-input')
                        : view.textarea;
                    if (textarea) textarea.placeholder = isMobile ? 'Message...' : 'Send a message...';
                } else if (msg.stop_reason === 'tool_use') {
                    label = 'using tools';
                } else {
                    label = 'responding';
                }
            } else if (msg.role === 'user') {
                label = 'thinking';
            }
        }

        this._setStatusIndicator(view, stateEl, label);
    }

    _getStatusElement(view) {
        return window.innerWidth <= 768
            ? document.getElementById('mobile-input-state')
            : view.inputArea?.querySelector('.sv-input-state');
    }

    _canRenderStatus(view, stateEl) {
        return stateEl?.id !== 'mobile-input-state' || this.activeSessionId === view.sessionId;
    }

    _syncStatusIndicator(view) {
        const stateEl = this._getStatusElement(view);
        if (!stateEl || !this._canRenderStatus(view, stateEl)) return;
        this._renderStatusIndicator(view, stateEl, view.statusLabel);
    }

    _setStatusIndicator(view, stateEl, label) {
        if (view._statusIdleTimer) {
            clearTimeout(view._statusIdleTimer);
            view._statusIdleTimer = null;
        }

        view.statusLabel = label || null;

        // The mobile input bar is shared by every session. Background session
        // events must update only that session's state, never the visible DOM.
        if (this._canRenderStatus(view, stateEl)) {
            this._renderStatusIndicator(view, stateEl, view.statusLabel);
        }

        if (!view.statusLabel) return;

        // Auto-clear if nothing new arrives for a while (e.g. session crashed).
        // The timer owns only this view's state and clears the shared mobile
        // element only when this is still the selected session.
        view._statusIdleTimer = setTimeout(() => {
            view._statusIdleTimer = null;
            view.statusLabel = null;
            const currentStateEl = this._getStatusElement(view);
            if (currentStateEl && this._canRenderStatus(view, currentStateEl)) {
                this._renderStatusIndicator(view, currentStateEl, null);
            }
        }, 45000);
    }

    _renderStatusIndicator(view, stateEl, label) {
        if (!label) {
            stateEl.innerHTML = '';
            stateEl.classList.remove('sv-state-active');
            delete stateEl.dataset.label;
            return;
        }

        if (stateEl.dataset.label !== label) {
            stateEl.dataset.label = label;
            stateEl.innerHTML = `
                <span class="sv-state-dots" aria-hidden="true">
                    <span></span><span></span><span></span>
                </span>
                <span class="sv-state-text">${view.codexMode ? 'Codex' : 'Claude'} is ${this._escapeHtml(label)}</span>
            `;
        }
        stateEl.classList.add('sv-state-active');
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

    // Escape for BOTH text and attribute contexts: the textContent -> innerHTML
    // trick this used to do does NOT escape the double quote, so interpolating
    // into an attribute truncated the value at the first `"` it contained.
    _escapeHtml(str) {
        if (str === null || str === undefined) return '';
        return String(str)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

    /**
     * Generate a clean preview from text, stripping markdown, ANSI escape
     * codes, and collapsing whitespace.
     */
    _plainTextPreview(text, maxLen) {
        if (!text) return '';
        const plain = text
            // Strip ANSI CSI sequences (e.g. PowerShell coloured output)
            .replace(/\x1b\[[0-9;]*[A-Za-z]/g, '')
            // [ESC missing] orphaned codes like "[32;1m" left after PTY mangling
            .replace(/\[[0-9]+(;[0-9]+)*m/g, '')
            .replace(/#{1,6}\s+/g, '')                 // markdown headings
            .replace(/\*{1,2}([^*]+)\*{1,2}/g, '$1')   // bold/italic
            .replace(/`([^`]+)`/g, '$1')               // inline code
            .replace(/\n+/g, ' ')                       // collapse newlines
            .replace(/\s+/g, ' ')                       // collapse whitespace
            .trim();
        return plain.length > maxLen ? plain.substring(0, maxLen) + '…' : plain;
    }

    /**
     * Format JSON content for display, pretty-printing if valid JSON.
     * Strips ANSI escape sequences so PowerShell/colorized output is readable.
     */
    _formatJsonContent(content) {
        if (!content) return '';
        const trimmed = content.trim();
        try {
            const parsed = JSON.parse(trimmed);
            return JSON.stringify(parsed, null, 2);
        } catch (e) {
            return content
                .replace(/\x1b\[[0-9;]*[A-Za-z]/g, '')
                .replace(/\[[0-9]+(;[0-9]+)*m/g, '');
        }
    }

    _toolInputSummary(toolName, input) {
        if (!input) return '';
        if (typeof input === 'string') return input.substring(0, 90);

        // Strip workspace prefix so file paths stay readable
        const shortPath = (p) => {
            if (!p) return '';
            return p.replace(/^\/Users\/[^/]+\//, '~/').replace(/^.*\/([^/]+\/[^/]+\/[^/]+)$/, '…/$1');
        };

        switch (toolName) {
            case 'Bash':
                return (input.command || '').replace(/\s+/g, ' ').substring(0, 90);
            case 'Read':
                return shortPath(input.file_path);
            case 'Write':
                return shortPath(input.file_path);
            case 'Edit':
                return shortPath(input.file_path);
            case 'Grep':
                return `${input.pattern || ''}${input.path ? ' in ' + shortPath(input.path) : ''}`.trim();
            case 'Glob':
                return input.pattern || '';
            case 'Skill':
                return input.skill || '';
            case 'Task':
            case 'Agent':
                return input.description || input.subagent_type || '';
            case 'TaskCreate':
            case 'TaskUpdate':
                return input.subject || input.taskId || '';
            case 'WebFetch':
            case 'WebSearch':
                return input.url || input.query || '';
            case 'AskUserQuestion': {
                const q = input.questions?.[0]?.question;
                return q ? q.substring(0, 90) : '';
            }
            default: {
                const keys = Object.keys(input);
                if (keys.length === 0) return '';
                // Prefer string fields with semantic names
                for (const k of ['name', 'title', 'description', 'subject', 'query', 'url', 'path']) {
                    if (typeof input[k] === 'string') return input[k].substring(0, 90);
                }
                const val = input[keys[0]];
                if (typeof val === 'string') return val.substring(0, 90);
                return '';
            }
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
