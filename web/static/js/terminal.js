// Terminal Manager using xterm.js - Multi-Terminal Support
class TerminalManager {
    constructor(containerWrapperId) {
        this.containerWrapperId = containerWrapperId || 'terminal-containers-wrapper';
        this.terminals = new Map(); // sessionId -> { terminal, container, ws, sessionName, fitAddon, status }
        this.activeSessionId = null;
        this.structuredViewActive = new Map(); // sessionId -> boolean
        console.log('TerminalManager initialized for multi-terminal support');
    }

    // Create a new terminal container for a session
    createTerminalContainer(sessionId) {
        const wrapper = document.getElementById(this.containerWrapperId);
        if (!wrapper) {
            console.error(`Terminal wrapper ${this.containerWrapperId} not found!`);
            return null;
        }

        const container = document.createElement('div');
        container.id = `terminal-container-${sessionId}`;
        container.className = 'terminal-container';
        wrapper.appendChild(container);
        return container;
    }

    showConnectingOverlay(sessionId) {
        const container = document.getElementById(`terminal-container-${sessionId}`);
        if (!container) return;
        const overlay = document.createElement('div');
        overlay.className = 'terminal-connecting-overlay';
        overlay.id = `terminal-connecting-${sessionId}`;
        overlay.innerHTML = `
            <div class="terminal-connecting-box">
                <div class="terminal-connecting-spinner"></div>
                <div class="terminal-connecting-text">Starting session...</div>
                <div class="terminal-connecting-sub">Establishing connection</div>
            </div>`;
        container.appendChild(overlay);
    }

    dismissConnectingOverlay(sessionId) {
        const overlay = document.getElementById(`terminal-connecting-${sessionId}`);
        if (overlay) {
            overlay.classList.add('fade-out');
            setTimeout(() => overlay.remove(), 300);
        }
    }

    // Connect to a session (create new terminal if doesn't exist)
    async connect(sessionId, sessionName) {
        console.log('TerminalManager.connect called with:', sessionId, sessionName);

        // If already connected to this session, just switch to it
        if (this.terminals.has(sessionId)) {
            console.log('Terminal for session already exists, switching to it');
            this.switchToSession(sessionId);
            return;
        }

        // Create new terminal instance
        const container = this.createTerminalContainer(sessionId);
        if (!container) return;

        // Check if Terminal class exists
        if (typeof Terminal === 'undefined') {
            console.error('xterm.js Terminal class not found!');
            return;
        }

        const termTheme = window.openPoetTheme ? window.openPoetTheme.getTerminalTheme() : {
            background: '#0f0f1a', foreground: '#e4e4e7', cursor: '#6366f1',
            selection: 'rgba(99, 102, 241, 0.3)', black: '#1a1a2e', red: '#ef4444',
            green: '#22c55e', yellow: '#f59e0b', blue: '#3b82f6', magenta: '#a855f7',
            cyan: '#06b6d4', white: '#e4e4e7', brightBlack: '#3a3a5a', brightRed: '#f87171',
            brightGreen: '#4ade80', brightYellow: '#fbbf24', brightBlue: '#60a5fa',
            brightMagenta: '#c084fc', brightCyan: '#22d3ee', brightWhite: '#ffffff'
        };
        const terminal = new Terminal({
            theme: termTheme,
            fontFamily: '"Monaco", "Menlo", "Ubuntu Mono", "Consolas", monospace',
            fontSize: 14,
            lineHeight: 1.2,
            cursorBlink: true,
            cursorStyle: 'bar',
            scrollback: 10000,
            tabStopWidth: 4
        });

        // Load addons
        const fitAddon = new FitAddon.FitAddon();
        terminal.loadAddon(fitAddon);

        const webLinksAddon = new WebLinksAddon.WebLinksAddon();
        terminal.loadAddon(webLinksAddon);

        // Open terminal in container
        terminal.open(container);

        // Show connecting overlay (dismissed on buffer fetch, first output, or timeout)
        this.showConnectingOverlay(sessionId);
        setTimeout(() => this.dismissConnectingOverlay(sessionId), 10000);

        // Create WebSocket connection
        const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        const ws = new WebSocket(`${protocol}//${window.location.host}/ws/session/${sessionId}`);

        // Store terminal instance
        this.terminals.set(sessionId, {
            terminal,
            container,
            ws,
            sessionName: sessionName || sessionId.substring(0, 8),
            fitAddon,
            status: 'connecting'
        });

        // Set up WebSocket handlers
        this.setupWebSocket(sessionId, ws, terminal);

        // Switch to this terminal
        this.switchToSession(sessionId);

        // Use ResizeObserver to re-fit terminal whenever the wrapper changes size.
        // This handles: initial layout settling, orientation changes, bars appearing,
        // window resize, and any other layout shift.
        const wrapper = document.getElementById(this.containerWrapperId);
        let fitDebounce = null;
        const resizeObserver = new ResizeObserver(() => {
            if (this.activeSessionId === sessionId) {
                clearTimeout(fitDebounce);
                fitDebounce = setTimeout(() => {
                    const td = this.terminals.get(sessionId);
                    if (td) this.safeFit(td);
                }, 50);
            }
        });
        if (wrapper) {
            resizeObserver.observe(wrapper);
        }

        // Store for cleanup
        this.terminals.get(sessionId).resizeObserver = resizeObserver;

        // Initial fit after layout has settled
        requestAnimationFrame(() => {
            requestAnimationFrame(() => {
                const td = this.terminals.get(sessionId);
                if (td) this.safeFit(td);
            });
        });

        // On mobile, schedule additional fits to handle late layout settling.
        // The initial double-rAF may fire before fonts load or bars finish
        // animating, causing a column mismatch with the PTY.
        if (window.innerWidth <= 768) {
            setTimeout(() => {
                const td = this.terminals.get(sessionId);
                if (td) this.safeFit(td);
            }, 300);
            setTimeout(() => {
                const td = this.terminals.get(sessionId);
                if (td) this.safeFit(td);
            }, 800);
        }

        // Setup mobile touch scroll with momentum
        if (window.innerWidth <= 768) {
            this.setupMobileTouchScroll(sessionId);
        }
    }

    // Mobile touch scroll with momentum/inertia
    setupMobileTouchScroll(sessionId) {
        const termData = this.terminals.get(sessionId);
        if (!termData) return;

        const container = termData.container;
        const terminal = termData.terminal;

        let touchStartY = 0;
        let lastTouchY = 0;
        let lastTouchTime = 0;
        let velocity = 0;
        let momentumRAF = null;
        let trackingPoints = []; // Store recent touch points for velocity calculation

        const cancelMomentum = () => {
            if (momentumRAF) {
                cancelAnimationFrame(momentumRAF);
                momentumRAF = null;
            }
        };

        container.addEventListener('touchstart', (e) => {
            cancelMomentum();
            if (e.touches.length !== 1) return;
            touchStartY = e.touches[0].clientY;
            lastTouchY = touchStartY;
            lastTouchTime = Date.now();
            velocity = 0;
            trackingPoints = [{ y: touchStartY, time: lastTouchTime }];
        }, { passive: true });

        container.addEventListener('touchmove', (e) => {
            if (e.touches.length !== 1) return;
            const currentY = e.touches[0].clientY;
            const now = Date.now();
            const deltaY = lastTouchY - currentY; // positive = scroll down

            // Scroll by pixel amount converted to lines (approx 17px per line)
            const lineHeight = 17;
            if (Math.abs(deltaY) >= lineHeight) {
                const lines = Math.round(deltaY / lineHeight);
                terminal.scrollLines(lines);
                lastTouchY = currentY;
            }

            // Track points for velocity (keep last 100ms of points)
            trackingPoints.push({ y: currentY, time: now });
            while (trackingPoints.length > 0 && now - trackingPoints[0].time > 100) {
                trackingPoints.shift();
            }

            lastTouchTime = now;
        }, { passive: true });

        container.addEventListener('touchend', (e) => {
            if (trackingPoints.length < 2) return;

            // Calculate velocity from recent tracking points
            const first = trackingPoints[0];
            const last = trackingPoints[trackingPoints.length - 1];
            const dt = last.time - first.time;
            if (dt === 0) return;

            velocity = (first.y - last.y) / dt; // pixels per ms (positive = scrolling down)

            // Only apply momentum if velocity is significant
            if (Math.abs(velocity) < 0.3) return;

            // Cap velocity
            velocity = Math.max(-8, Math.min(8, velocity));

            const lineHeight = 17;
            const friction = 0.95;
            const minVelocity = 0.05;

            const applyMomentum = () => {
                velocity *= friction;

                if (Math.abs(velocity) < minVelocity) {
                    momentumRAF = null;
                    return;
                }

                // Convert velocity to lines
                const pixelDelta = velocity * 16; // 16ms per frame
                const lines = Math.round(pixelDelta / lineHeight);
                if (lines !== 0) {
                    terminal.scrollLines(lines);
                }

                momentumRAF = requestAnimationFrame(applyMomentum);
            };

            momentumRAF = requestAnimationFrame(applyMomentum);
        }, { passive: true });

        // Store cleanup function
        termData._cancelMomentum = cancelMomentum;
    }

    // Setup WebSocket handlers
    setupWebSocket(sessionId, ws, terminal) {
        const termData = this.terminals.get(sessionId);
        if (!termData) return;

        ws.onopen = async () => {
            console.log(`WebSocket connected for session: ${sessionId}`);
            terminal.writeln('\x1b[32mConnected to session\x1b[0m');
            ws.send(JSON.stringify({ type: 'subscribe', channel: `session:${sessionId}` }));

            // Update status to running
            this.updateSessionStatus(sessionId, 'running');

            // Fetch buffered output to restore history
            try {
                console.log('Fetching output buffer for session:', sessionId);
                const response = await fetch(`/api/sessions/${sessionId}/output`);
                console.log('Buffer fetch response:', response.status, response.ok);
                if (response.ok) {
                    const arrayBuffer = await response.arrayBuffer();
                    console.log('Buffer size:', arrayBuffer.byteLength, 'bytes');
                    if (arrayBuffer.byteLength > 0) {
                        const text = new TextDecoder().decode(arrayBuffer);
                        console.log('Buffer text length:', text.length, 'chars');
                        // Clear terminal and reset cursor before replaying buffer
                        terminal.write('\x1b[2J\x1b[H');
                        terminal.writeln('\x1b[33m--- Session History ---\x1b[0m');
                        terminal.write(text);
                        // Ensure we land at the latest output after replaying history
                        terminal.scrollToBottom();
                    } else {
                        console.log('Buffer is empty');
                    }
                } else {
                    console.warn('Buffer fetch failed with status:', response.status);
                }
            } catch (err) {
                console.error('Failed to fetch session output buffer:', err);
            }

            // Buffer fetch done (or failed) — dismiss the connecting overlay.
            // Previously the overlay only dismissed on the first session_output
            // WebSocket message, which never arrives if the session is idle.
            this.dismissConnectingOverlay(sessionId);

            // Recover pending hook state from backend
            try {
                const hookResp = await fetch(`/api/hooks/pending/${sessionId}`);
                if (hookResp.ok) {
                    const hookState = await hookResp.json();
                    if (window.hookManager) {
                        window.hookManager.restoreFromServer(hookState, sessionId);
                    }
                }
            } catch (err) {
                console.error('Failed to fetch hook state:', err);
            }

            // Fetch persisted plan for this session
            if (window.hookManager) {
                window.hookManager.fetchPlan(sessionId);
            }
        };

        ws.onmessage = async (event) => {
            let data = event.data;

            // Decrypt if tunnel encryption is active
            if (typeof data === 'string' && data.includes('"_encrypted":true')) {
                try {
                    const enc = JSON.parse(data);
                    if (enc._encrypted && window.app && window.app._tunnelCryptoKey) {
                        data = await window.app._decryptTunnelMessage(enc);
                    }
                } catch (e) {
                    // Not encrypted or decryption failed, use raw data
                }
            }

            const msg = JSON.parse(data);
            if (msg.type === 'session_output' && msg.data) {
                this.dismissConnectingOverlay(sessionId);
                terminal.write(msg.data);
                // Sync terminal prompt line to mobile input (debounced)
                this._scheduleMobileInputSync(sessionId);
                // Clear pending input tracking for activity bar
                const _td = this.terminals.get(sessionId);
                if (_td && _td._awaitingOutput) {
                    _td._awaitingOutput = false;
                    window.networkFeedback?.requestFinished();
                }
            } else if (msg.type === 'session_status') {
                if (msg.data.status === 'completed' || msg.data.status === 'error' || msg.data.status === 'stopped') {
                    this.dismissConnectingOverlay(sessionId);
                    terminal.writeln(`\r\n\x1b[33mSession ${msg.data.status}\x1b[0m`);
                    this.updateSessionStatus(sessionId, msg.data.status);
                }
            } else if (msg.type && msg.type.startsWith('hook_')) {
                // Route hook messages to HookManager
                if (window.hookManager) {
                    window.hookManager.handleMessage(msg);
                }
            } else if (msg.type === 'session_event' && msg.data) {
                // Route structured view events
                if (window.structuredView) {
                    window.structuredView.appendEvent(sessionId, msg.data);
                }
            }
        };

        ws.onerror = (error) => {
            console.error(`WebSocket error for session ${sessionId}:`, error);
            // Don't update status here — onerror always fires before onclose,
            // and onclose handles reconnection. Firing 'error' here would
            // trigger auto-close before reconnection has a chance.
        };

        ws.onclose = () => {
            console.log(`WebSocket closed for session: ${sessionId}`);
            const termData = this.terminals.get(sessionId);
            if (!termData) return;

            // If this was an intentional disconnect (terminal disposed), don't reconnect
            if (termData._intentionalDisconnect) return;

            termData.terminal.writeln('\r\n\x1b[33mConnection lost, reconnecting...\x1b[0m');
            this.updateSessionStatus(sessionId, 'disconnected');
            this._reconnect(sessionId);
        };

        // Handle terminal input (desktop only - mobile uses input bar)
        if (window.innerWidth > 768) {
            terminal.onData((data) => {
                if (ws.readyState === WebSocket.OPEN) {
                    ws.send(JSON.stringify({ type: 'input', data: data }));
                    this.clearScrollLock(termData);
                    // Track pending output for activity bar
                    if (!termData._awaitingOutput) {
                        termData._awaitingOutput = true;
                        window.networkFeedback?.requestStarted();
                    }
                }
            });
        }

        // Handle resize
        terminal.onResize(({ cols, rows }) => {
            if (ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ type: 'resize', data: { cols, rows } }));
            }
        });

        // Desktop scroll system — mirrors the mobile approach.
        // Mobile: pointer-events:none on .xterm-screen + custom touch handler
        //         using terminal.scrollLines() at 60fps.
        // Desktop: overflow-y:hidden on .xterm-viewport + custom wheel handler
        //          using terminal.scrollLines().
        // By disabling native CSS scroll on the viewport, xterm.js can no
        // longer fight our scroll position through scrollTop adjustments after
        // write processing.  All scrolling is programmatic, giving us full
        // control — exactly like mobile.
        if (window.innerWidth > 768) {
            const viewportEl = terminal.element?.querySelector('.xterm-viewport');
            if (viewportEl) {
                viewportEl.style.overflowY = 'hidden';
            }

            termData._userScrolledUp = false;
            termData._scrollLockY = null;
            let _userInitiatedScroll = false;
            let _restoringScroll = false;
            let _wheelAccum = 0;

            // Wheel handler — converts wheel deltas to scrollLines() calls
            // (mirrors mobile's touchmove → scrollLines() pattern)
            termData.container.addEventListener('wheel', (e) => {
                e.preventDefault();
                let pixelDelta = e.deltaY;
                if (e.deltaMode === 1) pixelDelta *= 17;       // line mode
                else if (e.deltaMode === 2) pixelDelta *= terminal.rows * 17; // page mode

                // Accumulate sub-line deltas for smooth trackpad scrolling
                if ((_wheelAccum > 0 && pixelDelta < 0) || (_wheelAccum < 0 && pixelDelta > 0)) {
                    _wheelAccum = 0; // direction changed
                }
                _wheelAccum += pixelDelta;
                const lineHeight = 17;
                const lines = Math.trunc(_wheelAccum / lineHeight);
                if (lines !== 0) {
                    _wheelAccum -= lines * lineHeight;
                    _userInitiatedScroll = true;
                    terminal.scrollLines(lines);
                    _userInitiatedScroll = false; // safety: clear in case onScroll didn't fire
                }
            }, { passive: false });

            // Custom key handler: scroll, copy/paste (Windows/Linux fix)
            terminal.attachCustomKeyEventHandler((domEvent) => {
                // Scroll: Shift+PgUp/PgDn
                if (domEvent.type === 'keydown' && domEvent.shiftKey &&
                    (domEvent.key === 'PageUp' || domEvent.key === 'PageDown')) {
                    _userInitiatedScroll = true;
                    requestAnimationFrame(() => { _userInitiatedScroll = false; });
                }

                // Copy: Ctrl+C with selection → copy instead of interrupt (fixes Windows)
                if (domEvent.type === 'keydown' && domEvent.ctrlKey && !domEvent.shiftKey &&
                    domEvent.key === 'c' && terminal.hasSelection()) {
                    navigator.clipboard.writeText(terminal.getSelection());
                    terminal.clearSelection();
                    return false; // prevent xterm from sending \x03
                }

                // Copy: Ctrl+Shift+C always copies (Linux terminal convention)
                if (domEvent.type === 'keydown' && domEvent.ctrlKey && domEvent.shiftKey &&
                    domEvent.key === 'C') {
                    if (terminal.hasSelection()) {
                        navigator.clipboard.writeText(terminal.getSelection());
                        terminal.clearSelection();
                    }
                    return false;
                }

                // Paste: Ctrl+Shift+V (Linux terminal convention)
                if (domEvent.type === 'keydown' && domEvent.ctrlKey && domEvent.shiftKey &&
                    domEvent.key === 'V') {
                    navigator.clipboard.readText().then(text => {
                        if (text && window.terminalManager) {
                            window.terminalManager.paste(text);
                        }
                    });
                    return false;
                }

                return true;
            });

            // Central scroll state manager — fires on every viewportY change.
            // Distinguishes user scroll (wheel/keyboard) from programmatic
            // scroll (xterm.js cursor-follow after write).
            terminal.onScroll(() => {
                if (_restoringScroll) return;

                const buf = terminal.buffer.active;

                if (_userInitiatedScroll) {
                    // User-initiated scroll — update lock state
                    _userInitiatedScroll = false;
                    if (buf.viewportY >= buf.baseY) {
                        this.clearScrollLock(termData);
                    } else {
                        termData._userScrolledUp = true;
                        termData._scrollLockY = buf.viewportY;
                    }
                    return;
                }

                // Programmatic scroll (xterm.js auto-follow after write)
                if (!termData._userScrolledUp) return;
                if (termData._scrollLockY == null) return;

                // Buffer reset (TUI full redraw shrinks baseY) — release lock
                if (termData._scrollLockY > buf.baseY) {
                    this.clearScrollLock(termData);
                    return;
                }

                // Restore the user's scroll position
                if (buf.viewportY !== termData._scrollLockY) {
                    _restoringScroll = true;
                    terminal.scrollLines(termData._scrollLockY - buf.viewportY);
                    _restoringScroll = false;
                }
            });
        } else {
            // Mobile — state tracked by touch handler, no onScroll guard needed
            termData._userScrolledUp = false;
            termData._scrollLockY = null;
        }
    }

    // Reconnect a terminal WebSocket after a connection drop.
    // Uses exponential backoff (1s, 2s, 4s, 8s, max 8s) up to 10 attempts.
    _reconnect(sessionId) {
        const termData = this.terminals.get(sessionId);
        if (!termData || termData._intentionalDisconnect) return;

        const maxAttempts = 10;
        const attempt = (termData._reconnectAttempt || 0) + 1;
        termData._reconnectAttempt = attempt;

        if (attempt > maxAttempts) {
            termData.terminal.writeln('\r\n\x1b[31mFailed to reconnect after multiple attempts.\x1b[0m');
            this.updateSessionStatus(sessionId, 'error');
            return;
        }

        const delay = Math.min(1000 * Math.pow(2, attempt - 1), 8000);
        console.log(`Reconnecting session ${sessionId} (attempt ${attempt}/${maxAttempts}) in ${delay}ms`);

        termData._reconnectTimer = setTimeout(() => {
            // Check again — session may have been closed by user during the wait
            const td = this.terminals.get(sessionId);
            if (!td || td._intentionalDisconnect) return;

            const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
            const ws = new WebSocket(`${protocol}//${window.location.host}/ws/session/${sessionId}`);

            ws.onopen = async () => {
                console.log(`Reconnected WebSocket for session: ${sessionId}`);
                td.ws = ws;
                td._reconnectAttempt = 0;
                td.terminal.writeln('\r\n\x1b[32mReconnected\x1b[0m');
                ws.send(JSON.stringify({ type: 'subscribe', channel: `session:${sessionId}` }));
                this.updateSessionStatus(sessionId, 'running');
            };

            ws.onmessage = async (event) => {
                let data = event.data;
                if (typeof data === 'string' && data.includes('"_encrypted":true')) {
                    try {
                        const enc = JSON.parse(data);
                        if (enc._encrypted && window.app && window.app._tunnelCryptoKey) {
                            data = await window.app._decryptTunnelMessage(enc);
                        }
                    } catch (e) {}
                }
                const msg = JSON.parse(data);
                if (msg.type === 'session_output' && msg.data) {
                    td.terminal.write(msg.data);
                    // Clear pending input tracking for activity bar
                    if (td._awaitingOutput) {
                        td._awaitingOutput = false;
                        window.networkFeedback?.requestFinished();
                    }
                } else if (msg.type === 'session_status') {
                    if (msg.data.status === 'completed' || msg.data.status === 'error' || msg.data.status === 'stopped') {
                        td.terminal.writeln(`\r\n\x1b[33mSession ${msg.data.status}\x1b[0m`);
                        this.updateSessionStatus(sessionId, msg.data.status);
                    }
                } else if (msg.type && msg.type.startsWith('hook_')) {
                    if (window.hookManager) {
                        window.hookManager.handleMessage(msg);
                    }
                } else if (msg.type === 'session_event' && msg.data) {
                    if (window.structuredView) {
                        window.structuredView.appendEvent(sessionId, msg.data);
                    }
                }
            };

            ws.onerror = (error) => {
                console.error(`WebSocket reconnect error for session ${sessionId}:`, error);
            };

            ws.onclose = () => {
                const td2 = this.terminals.get(sessionId);
                if (!td2 || td2._intentionalDisconnect) return;
                console.log(`WebSocket closed again for session ${sessionId}, retrying...`);
                this._reconnect(sessionId);
            };

            // Re-bind terminal input to new WebSocket
            if (window.innerWidth > 768) {
                td.terminal.onData((data) => {
                    if (ws.readyState === WebSocket.OPEN) {
                        ws.send(JSON.stringify({ type: 'input', data: data }));
                        this.clearScrollLock(td);
                        if (!td._awaitingOutput) {
                            td._awaitingOutput = true;
                            window.networkFeedback?.requestStarted();
                        }
                    }
                });
            }

            td.terminal.onResize(({ cols, rows }) => {
                if (ws.readyState === WebSocket.OPEN) {
                    ws.send(JSON.stringify({ type: 'resize', data: { cols, rows } }));
                }
            });
        }, delay);
    }

    // Clear scroll lock state — resumes auto-scroll to bottom.
    clearScrollLock(termData) {
        termData._userScrolledUp = false;
        termData._scrollLockY = null;
    }

    // Update session status and notify UI
    updateSessionStatus(sessionId, status) {
        const termData = this.terminals.get(sessionId);
        if (termData) {
            termData.status = status;

            // Dispatch custom event for UI to update
            window.dispatchEvent(new CustomEvent('session-status-changed', {
                detail: { sessionId, status }
            }));
        }
    }

    getSessionStatus(sessionId) {
        const termData = this.terminals.get(sessionId);
        return termData ? termData.status : 'unknown';
    }

    // Switch active terminal display
    switchToSession(sessionId) {
        if (!this.terminals.has(sessionId)) {
            console.warn(`Session ${sessionId} not found in terminals map`);
            return;
        }

        console.log(`Switching to session: ${sessionId}`);

        // Hide all terminals and structured views
        this.terminals.forEach((termData, sid) => {
            termData.container.classList.remove('active');
        });
        if (window.structuredView) {
            window.structuredView.views.forEach((view, sid) => {
                view.container.classList.remove('active');
            });
        }

        this.activeSessionId = sessionId;

        // Check if structured view is active for this session
        if (this.structuredViewActive.get(sessionId) && window.structuredView) {
            window.structuredView.show(sessionId);
            this._updateStructuredViewButton(true);
        } else {
            // Show terminal
            const termData = this.terminals.get(sessionId);
            termData.container.classList.add('active');
            this._updateStructuredViewButton(false);

            // Re-fit terminal after layout settles (double rAF ensures paint is done)
            requestAnimationFrame(() => {
                requestAnimationFrame(() => {
                    this.safeFit(termData);
                    this.clearScrollLock(termData);
                    termData.terminal.scrollToBottom();
                    if (window.innerWidth > 768) {
                        termData.terminal.focus();
                    }
                });
            });
        }

        // Notify hook manager to refresh panel/badge for the new session
        if (window.hookManager) {
            window.hookManager.onSessionSwitch();
        }
    }

    // Toggle between terminal and structured view for the active session
    toggleStructuredView() {
        const sessionId = this.activeSessionId;
        if (!sessionId || !window.structuredView) return;

        const isActive = this.structuredViewActive.get(sessionId) || false;
        const isMobile = window.innerWidth <= 768;

        if (isActive) {
            // Switch back to terminal — sync textarea to terminal prompt
            const termData = this.terminals.get(sessionId);

            if (isMobile) {
                // On mobile, sync from the shared mobile input
                const mobileInput = document.getElementById('mobile-terminal-input');
                const svText = mobileInput?.value ?? '';
                const svOriginal = this._svOriginalValue ?? '';

                if (svText !== svOriginal && termData?.ws?.readyState === WebSocket.OPEN) {
                    this.sendInputToSession(sessionId, '\x15');
                    if (svText) {
                        setTimeout(() => {
                            this.sendInputToSession(sessionId, svText);
                        }, 50);
                    }
                }
            } else {
                // On desktop, sync from the SV textarea
                const svView = window.structuredView.views.get(sessionId);
                const svText = svView?.textarea?.value ?? '';
                const svOriginal = svView?._originalValue ?? '';

                if (svText !== svOriginal && termData?.ws?.readyState === WebSocket.OPEN) {
                    this.sendInputToSession(sessionId, '\x15');
                    if (svText) {
                        setTimeout(() => {
                            this.sendInputToSession(sessionId, svText);
                        }, 50);
                    }
                }
            }

            this.structuredViewActive.set(sessionId, false);
            window.structuredView.hide(sessionId);

            if (termData) {
                termData.container.classList.add('active');
                requestAnimationFrame(() => {
                    requestAnimationFrame(() => {
                        this.safeFit(termData);
                        termData.terminal.scrollToBottom();
                        if (window.innerWidth > 768) {
                            termData.terminal.focus();
                        }
                    });
                });
            }
            this._updateStructuredViewButton(false);
        } else {
            // Switch to structured view
            this.structuredViewActive.set(sessionId, true);

            const termData = this.terminals.get(sessionId);
            if (termData) {
                termData.container.classList.remove('active');
            }

            window.structuredView.show(sessionId);
            this._updateStructuredViewButton(true);

            // Populate the appropriate textarea from terminal's current line content
            const lineContent = this.getSessionLineContent(sessionId);

            if (isMobile) {
                // On mobile, just update the shared mobile input value — no style changes
                const mobileInput = document.getElementById('mobile-terminal-input');
                if (mobileInput) {
                    mobileInput.value = lineContent;
                    mobileInput._lastSyncedValue = lineContent;
                    this._svOriginalValue = lineContent;
                }
            } else {
                const svView = window.structuredView.views.get(sessionId);
                if (svView?.textarea) {
                    svView.textarea.value = lineContent;
                    svView._originalValue = lineContent;
                    svView.textarea.style.height = 'auto';
                    svView.textarea.style.height = Math.min(svView.textarea.scrollHeight, 150) + 'px';
                    requestAnimationFrame(() => svView.textarea.focus());
                }
            }
        }

        // Persist preference
        try {
            const prefs = JSON.parse(localStorage.getItem('sv_prefs') || '{}');
            prefs[sessionId] = this.structuredViewActive.get(sessionId);
            localStorage.setItem('sv_prefs', JSON.stringify(prefs));
        } catch (e) { /* ignore */ }
    }

    // Update the toggle button visual state
    _updateStructuredViewButton(active) {
        const btn = document.getElementById('btn-structured-view');
        if (btn) {
            btn.classList.toggle('active', active);
        }
    }

    // Close a terminal session
    disconnect(sessionId) {
        const termData = this.terminals.get(sessionId);
        if (!termData) {
            console.warn(`Session ${sessionId} not found for disconnect`);
            return;
        }

        console.log(`Disconnecting session: ${sessionId}`);

        // Mark as intentional so reconnect logic doesn't fire
        termData._intentionalDisconnect = true;

        // Cancel any pending reconnect timer
        if (termData._reconnectTimer) {
            clearTimeout(termData._reconnectTimer);
        }

        // Remove resize observer
        if (termData.resizeObserver) {
            termData.resizeObserver.disconnect();
        }

        // Close WebSocket
        if (termData.ws) {
            termData.ws.onmessage = null; // Prevent writes to disposed terminal
            termData.ws.onerror = null;
            termData.ws.onclose = null;
            termData.ws.close();
        }

        // Dispose terminal
        if (termData.terminal) {
            termData.terminal.dispose();
        }

        // Remove container from DOM
        if (termData.container) {
            termData.container.remove();
        }

        // Clean up structured view
        if (window.structuredView) {
            window.structuredView.dispose(sessionId);
        }
        this.structuredViewActive.delete(sessionId);

        // Remove from map
        this.terminals.delete(sessionId);

        // If this was active, switch to another tab
        if (this.activeSessionId === sessionId) {
            const remainingSessions = Array.from(this.terminals.keys());
            if (remainingSessions.length > 0) {
                this.switchToSession(remainingSessions[0]);
            } else {
                this.activeSessionId = null;
            }
        }
    }

    // Rename a session tab
    renameSession(sessionId, newName) {
        const termData = this.terminals.get(sessionId);
        if (termData) {
            termData.sessionName = newName;
        }
    }

    // Get session name
    getSessionName(sessionId) {
        const termData = this.terminals.get(sessionId);
        return termData ? termData.sessionName : null;
    }

    // Check if session is open in a tab
    hasSession(sessionId) {
        return this.terminals.has(sessionId);
    }

    // Get all open session IDs
    getOpenSessions() {
        return Array.from(this.terminals.keys());
    }

    // Get active session ID
    getActiveSessionId() {
        return this.activeSessionId;
    }

    // Write to active terminal
    write(data) {
        if (this.activeSessionId) {
            const termData = this.terminals.get(this.activeSessionId);
            if (termData && termData.terminal) {
                termData.terminal.write(data);
            }
        }
    }

    // Sync the xterm viewport's scroll area height with the actual buffer state.
    // After repeated fit/resize operations, the .xterm-scroll-area height can
    // drift from the real content height, preventing wheel scrolling from
    // reaching the bottom even though scrollToBottom() (xterm.js internal) works.
    syncViewport(termData) {
        if (!termData?.terminal?.element) return;
        const term = termData.terminal;
        const viewportEl = term.element.querySelector('.xterm-viewport');
        const scrollArea = term.element.querySelector('.xterm-scroll-area');
        if (!viewportEl || !scrollArea) return;

        const buf = term.buffer.active;
        if (buf.baseY === 0) return; // Nothing in scrollback

        // Cell height = visible viewport height / visible rows
        const cellHeight = viewportEl.offsetHeight / term.rows;
        const expectedHeight = Math.ceil((buf.baseY + term.rows) * cellHeight);
        const currentHeight = parseInt(scrollArea.style.height, 10) || 0;

        // Only fix if there's a meaningful drift (> 1 line)
        if (Math.abs(expectedHeight - currentHeight) > cellHeight) {
            scrollArea.style.height = expectedHeight + 'px';
        }
    }

    // Fit the terminal to its container, applying a safety margin on mobile.
    // On mobile browsers, sub-pixel font rendering can cause fitAddon to
    // calculate 1-2 more columns than actually fit visually. When the PTY
    // has more cols than the rendered terminal, TUI redraws (ANSI escapes)
    // wrap at the wrong position, causing garbled/duplicated lines.
    safeFit(termData) {
        if (!termData || !termData.fitAddon || !termData.terminal) return;
        const term = termData.terminal;
        const buf = term.buffer.active;
        const wasAtBottom = buf.viewportY >= buf.baseY;
        const prevViewportY = buf.viewportY;
        termData.fitAddon.fit();

        if (window.innerWidth <= 768) {
            const safeCols = Math.max(20, term.cols - 1);
            if (safeCols !== term.cols) {
                term.resize(safeCols, term.rows);
            }
        }

        this.syncViewport(termData);

        if (wasAtBottom) {
            term.scrollToBottom();
        } else {
            const targetY = Math.min(prevViewportY, buf.baseY);
            if (buf.viewportY !== targetY) {
                term.scrollLines(targetY - buf.viewportY);
            }
            if (termData._userScrolledUp) {
                termData._scrollLockY = buf.viewportY;
            }
        }
    }

    // Re-fit the active terminal to ensure cols/rows match the container.
    // Call this before sending navigation keys on mobile to prevent
    // rendering glitches from column mismatch between xterm.js and PTY.
    ensureFit() {
        if (this.activeSessionId) {
            const termData = this.terminals.get(this.activeSessionId);
            if (termData) {
                this.safeFit(termData);
            }
        }
    }

    // Send input to active terminal.
    // Blocks input if a session transition is in progress (pendingSessionOpen)
    // to prevent stray input from reaching the wrong session.
    sendInput(data) {
        if (window.app && window.app.pendingSessionOpen) {
            console.warn('[sendInput] Blocked: session transition in progress for', window.app.pendingSessionOpen.sessionId);
            return;
        }
        if (this.activeSessionId) {
            this.sendInputToSession(this.activeSessionId, data);
        }
    }

    // Send input to a specific session by ID (safe against session switches)
    sendInputToSession(sessionId, data) {
        const termData = this.terminals.get(sessionId);
        if (termData && termData.ws && termData.ws.readyState === WebSocket.OPEN) {
            termData.ws.send(JSON.stringify({ type: 'input', data: data }));
            // Track pending output for activity bar
            if (!termData._awaitingOutput) {
                termData._awaitingOutput = true;
                window.networkFeedback?.requestStarted();
            }
        }
    }

    /**
     * Debounced sync of terminal prompt line content to the mobile input.
     * Only runs on mobile, in terminal mode (not SV), when the input isn't focused.
     */
    _scheduleMobileInputSync(sessionId) {
        if (window.innerWidth > 768) return; // Desktop — no sync needed
        if (sessionId !== this.activeSessionId) return; // Not the visible session

        clearTimeout(this._mobileInputSyncTimer);
        this._mobileInputSyncTimer = setTimeout(() => {
            const mobileInput = document.getElementById('mobile-terminal-input');
            if (!mobileInput || document.activeElement === mobileInput) return; // User is typing

            const lineContent = this.getSessionLineContent(sessionId);
            if (mobileInput.value !== lineContent) {
                mobileInput.value = lineContent;
                mobileInput._lastSyncedValue = lineContent;
                // Keep single-line height unless content is multi-line
                if (!lineContent || lineContent.indexOf('\n') === -1) {
                    mobileInput.style.height = '44px';
                    mobileInput.style.overflow = 'hidden';
                }
            }
        }, 100);
    }

    /**
     * Read the current terminal input content for a session.
     * Claude Code's TUI places the cursor below the input line, so we scan
     * backwards from the cursor to find the prompt line (starts with ❯).
     * Placeholder/suggestion text (e.g. 'Try "fix lint errors"') is rendered
     * with the dim attribute — we detect and ignore it.
     */
    getSessionLineContent(sessionId) {
        const termData = this.terminals.get(sessionId);
        if (!termData?.terminal) return '';
        const buf = termData.terminal.buffer.active;
        const cursorRow = buf.cursorY + buf.baseY;

        // Scan backwards (up to 10 rows) looking for the prompt line
        for (let i = cursorRow; i >= Math.max(0, cursorRow - 10); i--) {
            const line = buf.getLine(i);
            if (!line) continue;
            const text = line.translateToString(true).trimEnd();
            // Claude Code prompt: ❯ <user input>
            const match = text.match(/^❯\s*(.*)/);
            if (match) {
                const afterPrompt = match[1] || '';
                if (!afterPrompt) return '';
                // Check if the text after ❯ is placeholder (dim attribute).
                // The first char at cursor position may not be dim, so check
                // the second non-space character after ❯ for the dim flag.
                const promptIdx = text.indexOf('❯');
                let dimChecked = false;
                let charsChecked = 0;
                for (let col = promptIdx + 1; col < line.length; col++) {
                    const cell = line.getCell(col);
                    if (!cell) break;
                    const ch = cell.getChars();
                    if (!ch || !ch.trim()) continue;
                    charsChecked++;
                    // Skip the first char (cursor position is never dim)
                    if (charsChecked >= 2) {
                        dimChecked = true;
                        if (cell.isDim()) return ''; // Placeholder text — ignore
                        break;
                    }
                }
                // If only 1 char typed, it's real input (no placeholder is 1 char)
                if (!dimChecked) return afterPrompt;
                return afterPrompt;
            }
        }
        // Fallback: try cursor line, strip common prompts
        const cursorLine = buf.getLine(cursorRow);
        if (!cursorLine) return '';
        const text = cursorLine.translateToString(true).trimEnd();
        return text.replace(/^[\s]*[❯>$%#]\s*/, '');
    }

    // Focus active terminal (desktop only - mobile terminal is read-only)
    focus() {
        if (this.activeSessionId && window.innerWidth > 768) {
            const termData = this.terminals.get(this.activeSessionId);
            if (termData && termData.terminal) {
                termData.terminal.focus();
            }
        }
    }

    // Clear active terminal
    clear() {
        if (this.activeSessionId) {
            const termData = this.terminals.get(this.activeSessionId);
            if (termData && termData.terminal) {
                termData.terminal.clear();
            }
        }
    }

    // Get text content of the current line in the active terminal
    getActiveLineContent() {
        if (this.activeSessionId) {
            const termData = this.terminals.get(this.activeSessionId);
            if (termData && termData.terminal) {
                const buf = termData.terminal.buffer.active;
                const line = buf.getLine(buf.cursorY + buf.baseY);
                return line ? line.translateToString(true) : '';
            }
        }
        return '';
    }

    // Paste to active terminal
    paste(text) {
        if (this.activeSessionId) {
            this.sendInput(text);
        }
    }

    // Dispose all terminals
    disposeAll() {
        this.terminals.forEach((termData, sessionId) => {
            this.disconnect(sessionId);
        });
        this.terminals.clear();
        this.activeSessionId = null;
    }
}

// Initialize terminal manager with new wrapper ID
window.terminalManager = new TerminalManager('terminal-containers-wrapper');

// Handle paste events in terminal wrapper
document.addEventListener('DOMContentLoaded', () => {
    const wrapper = document.getElementById('terminal-containers-wrapper');
    if (wrapper) {
        wrapper.addEventListener('paste', (e) => {
            // If clipboard contains an image, let files.js handle it
            const items = e.clipboardData?.items;
            if (items) {
                for (const item of items) {
                    if (item.type.indexOf('image') !== -1) {
                        return;
                    }
                }
            }
            // If image paste modal is open, don't interfere
            const imgDialog = document.getElementById('image-paste-dialog');
            if (imgDialog && !imgDialog.classList.contains('hidden')) {
                return;
            }
            const text = e.clipboardData.getData('text');
            if (text && window.terminalManager) {
                window.terminalManager.paste(text);
                e.preventDefault();
            }
        });
    }

    // Mobile scroll navigation buttons
    const btnScrollTop = document.getElementById('btn-scroll-top');
    const btnScrollBottom = document.getElementById('btn-scroll-bottom');

    if (btnScrollTop) {
        btnScrollTop.addEventListener('click', () => {
            const tm = window.terminalManager;
            if (tm && tm.activeSessionId) {
                const termData = tm.terminals.get(tm.activeSessionId);
                if (termData && termData.terminal) {
                    // Cancel any ongoing momentum
                    if (termData._cancelMomentum) termData._cancelMomentum();
                    termData.terminal.scrollToTop();
                }
            }
        });
    }

    if (btnScrollBottom) {
        btnScrollBottom.addEventListener('click', () => {
            const tm = window.terminalManager;
            if (tm && tm.activeSessionId) {
                const termData = tm.terminals.get(tm.activeSessionId);
                if (termData && termData.terminal) {
                    if (termData._cancelMomentum) termData._cancelMomentum();
                    tm.syncViewport(termData);
                    tm.clearScrollLock(termData);
                    termData.terminal.scrollToBottom();
                }
            }
        });
    }
});
