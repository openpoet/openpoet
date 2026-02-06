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
        fitAddon.fit();

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

        // Handle window resize
        const resizeHandler = () => {
            if (this.activeSessionId === sessionId) {
                fitAddon.fit();
            }
        };
        window.addEventListener('resize', resizeHandler);

        // Store resize handler for cleanup
        this.terminals.get(sessionId).resizeHandler = resizeHandler;
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

        // Handle terminal input
        terminal.onData((data) => {
            if (ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ type: 'input', data: data }));
            }
        });

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

        // Re-fit terminal to ensure proper dimensions
        setTimeout(() => {
            termData.fitAddon.fit();
            termData.terminal.focus();
        }, 0);
    }

    // Close a terminal session
    disconnect(sessionId) {
        const termData = this.terminals.get(sessionId);
        if (!termData) {
            console.warn(`Session ${sessionId} not found for disconnect`);
            return;
        }

        console.log(`Disconnecting session: ${sessionId}`);

        // Remove resize handler
        if (termData.resizeHandler) {
            window.removeEventListener('resize', termData.resizeHandler);
        }

        // Close WebSocket
        if (termData.ws) {
            termData.ws.onclose = null; // Remove handler to prevent events
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

    // Send input to active terminal
    sendInput(data) {
        if (this.activeSessionId) {
            const termData = this.terminals.get(this.activeSessionId);
            if (termData && termData.ws && termData.ws.readyState === WebSocket.OPEN) {
                termData.ws.send(JSON.stringify({ type: 'input', data: data }));
            }
        }
    }

    // Focus active terminal
    focus() {
        if (this.activeSessionId) {
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
            const text = e.clipboardData.getData('text');
            if (text && window.terminalManager) {
                window.terminalManager.paste(text);
                e.preventDefault();
            }
        });
    }
});
