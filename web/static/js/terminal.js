// Terminal Manager using xterm.js - Multi-Terminal Support
class TerminalManager {
    constructor(containerWrapperId) {
        this.containerWrapperId = containerWrapperId || 'terminal-containers-wrapper';
        this.terminals = new Map(); // sessionId -> { terminal, container, ws, sessionName, fitAddon, status }
        this.activeSessionId = null;
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

        const terminal = new Terminal({
            theme: {
                background: '#0f0f1a',
                foreground: '#e4e4e7',
                cursor: '#6366f1',
                selection: 'rgba(99, 102, 241, 0.3)',
                black: '#1a1a2e',
                red: '#ef4444',
                green: '#22c55e',
                yellow: '#f59e0b',
                blue: '#3b82f6',
                magenta: '#a855f7',
                cyan: '#06b6d4',
                white: '#e4e4e7',
                brightBlack: '#3a3a5a',
                brightRed: '#f87171',
                brightGreen: '#4ade80',
                brightYellow: '#fbbf24',
                brightBlue: '#60a5fa',
                brightMagenta: '#c084fc',
                brightCyan: '#22d3ee',
                brightWhite: '#ffffff'
            },
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
                    } else {
                        console.log('Buffer is empty');
                    }
                } else {
                    console.warn('Buffer fetch failed with status:', response.status);
                }
            } catch (err) {
                console.error('Failed to fetch session output buffer:', err);
            }

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
        };

        ws.onmessage = (event) => {
            const msg = JSON.parse(event.data);
            if (msg.type === 'session_output' && msg.data) {
                terminal.write(msg.data);
            } else if (msg.type === 'session_status') {
                if (msg.data.status === 'completed' || msg.data.status === 'error' || msg.data.status === 'stopped') {
                    terminal.writeln(`\r\n\x1b[33mSession ${msg.data.status}\x1b[0m`);
                    this.updateSessionStatus(sessionId, msg.data.status);
                }
            } else if (msg.type && msg.type.startsWith('hook_')) {
                // Route hook messages to HookManager
                if (window.hookManager) {
                    window.hookManager.handleMessage(msg);
                }
            }
        };

        ws.onerror = (error) => {
            console.error(`WebSocket error for session ${sessionId}:`, error);
            this.updateSessionStatus(sessionId, 'error');
        };

        ws.onclose = () => {
            console.log(`WebSocket closed for session: ${sessionId}`);
            const termData = this.terminals.get(sessionId);
            if (termData && termData.terminal) {
                termData.terminal.writeln('\r\n\x1b[31mDisconnected\x1b[0m');
            }
            this.updateSessionStatus(sessionId, 'disconnected');
        };

        // Handle terminal input (desktop only - mobile uses input bar)
        if (window.innerWidth > 768) {
            terminal.onData((data) => {
                if (ws.readyState === WebSocket.OPEN) {
                    ws.send(JSON.stringify({ type: 'input', data: data }));
                }
            });
        }

        // Handle resize
        terminal.onResize(({ cols, rows }) => {
            if (ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ type: 'resize', data: { cols, rows } }));
            }
        });
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

        // Hide all terminals
        this.terminals.forEach((termData, sid) => {
            termData.container.classList.remove('active');
        });

        // Show the selected terminal
        const termData = this.terminals.get(sessionId);
        termData.container.classList.add('active');
        this.activeSessionId = sessionId;

        // Re-fit terminal after layout settles (double rAF ensures paint is done)
        requestAnimationFrame(() => {
            requestAnimationFrame(() => {
                this.safeFit(termData);
                if (window.innerWidth > 768) {
                    termData.terminal.focus();
                }
            });
        });

        // Notify hook manager to refresh panel/badge for the new session
        if (window.hookManager) {
            window.hookManager.onSessionSwitch();
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

    // Fit the terminal to its container, applying a safety margin on mobile.
    // On mobile browsers, sub-pixel font rendering can cause fitAddon to
    // calculate 1-2 more columns than actually fit visually. When the PTY
    // has more cols than the rendered terminal, TUI redraws (ANSI escapes)
    // wrap at the wrong position, causing garbled/duplicated lines.
    safeFit(termData) {
        if (!termData || !termData.fitAddon || !termData.terminal) return;
        termData.fitAddon.fit();
        if (window.innerWidth <= 768) {
            const term = termData.terminal;
            const safeCols = Math.max(20, term.cols - 1);
            if (safeCols !== term.cols) {
                term.resize(safeCols, term.rows);
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

    // Send input to active terminal
    sendInput(data) {
        if (this.activeSessionId) {
            const termData = this.terminals.get(this.activeSessionId);
            if (termData && termData.ws && termData.ws.readyState === WebSocket.OPEN) {
                termData.ws.send(JSON.stringify({ type: 'input', data: data }));
            }
        }
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
                    // Cancel any ongoing momentum
                    if (termData._cancelMomentum) termData._cancelMomentum();
                    termData.terminal.scrollToBottom();
                }
            }
        });
    }
});
