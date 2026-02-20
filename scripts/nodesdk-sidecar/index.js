/**
 * OpenPoet Node.js Agent SDK Sidecar
 *
 * This is an HTTP server that wraps the official @anthropic-ai/claude-agent-sdk.
 * The OpenPoet Go backend communicates with this sidecar via HTTP to leverage
 * the full Agent SDK capabilities (session management, tool execution, etc.).
 *
 * Usage: node index.js --port 9999 --api-url http://localhost:8080
 */
import http from 'node:http';
import { query } from '@anthropic-ai/claude-agent-sdk';
import { buildOpenPoetTools } from './tools.js';

// Parse CLI arguments
const args = process.argv.slice(2);
let PORT = 9999;
let API_URL = 'http://localhost:8080';

for (let i = 0; i < args.length; i++) {
    if (args[i] === '--port' && args[i + 1]) PORT = parseInt(args[i + 1]);
    if (args[i] === '--api-url' && args[i + 1]) API_URL = args[i + 1];
}

// Session map: conversationID -> sessionID
const sessions = new Map();

/**
 * Read HTTP request body as string.
 */
function readBody(req) {
    return new Promise((resolve, reject) => {
        const chunks = [];
        req.on('data', (chunk) => chunks.push(chunk));
        req.on('end', () => resolve(Buffer.concat(chunks).toString()));
        req.on('error', reject);
    });
}

/**
 * Send an SSE event to the response.
 */
function sendSSE(res, type, data) {
    res.write(`data: ${JSON.stringify({ type, data })}\n\n`);
}

/**
 * Handle POST /query — main chat endpoint.
 * Streams responses as SSE events compatible with OpenPoet's frontend.
 */
async function handleQuery(req, res) {
    let body;
    try {
        body = JSON.parse(await readBody(req));
    } catch {
        res.writeHead(400, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ error: 'Invalid JSON' }));
        return;
    }

    const {
        conversation_id,
        message,
        system_prompt,
        model,
        session_id: requestSessionID,
    } = body;

    if (!message) {
        res.writeHead(400, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ error: 'Message is required' }));
        return;
    }

    // SSE headers
    res.writeHead(200, {
        'Content-Type': 'text/event-stream',
        'Cache-Control': 'no-cache',
        'Connection': 'keep-alive',
    });

    // Build OpenPoet MCP tools for this conversation
    const openpoetMcp = buildOpenPoetTools(API_URL, conversation_id);

    const options = {
        permissionMode: 'bypass_permissions',
        maxTurns: 15,
        sdkMcpServers: { openpoet: openpoetMcp },
        allowedTools: ['mcp__openpoet__*'],
    };

    if (system_prompt) options.systemPrompt = system_prompt;
    if (model) options.model = model;

    // Resume existing session if available
    const existingSession = requestSessionID || sessions.get(conversation_id);
    if (existingSession) {
        options.resume = existingSession;
    }

    try {
        for await (const msg of query({ prompt: message, options })) {
            if (msg.type === 'assistant') {
                // Assistant message — stream text and tool use events
                if (msg.content) {
                    for (const block of msg.content) {
                        if (block.type === 'text') {
                            sendSSE(res, 'text', { text: block.text });
                        } else if (block.type === 'tool_use') {
                            sendSSE(res, 'tool_start', {
                                id: block.tool_use_id || block.id,
                                name: block.name,
                            });
                        }
                    }
                }
            } else if (msg.type === 'user') {
                // User message with tool results
                if (msg.content && Array.isArray(msg.content)) {
                    for (const block of msg.content) {
                        if (block.type === 'tool_result') {
                            const resultText = typeof block.content === 'string'
                                ? block.content
                                : JSON.stringify(block.content);
                            sendSSE(res, 'tool_result', {
                                id: block.tool_use_id,
                                name: '',
                                result: resultText,
                            });
                        }
                    }
                }
            } else if (msg.type === 'system') {
                // System messages — capture session ID
                if (msg.subtype === 'init' && msg.session_id) {
                    sessions.set(conversation_id, msg.session_id);
                }
            } else if (msg.type === 'result' || 'result' in msg) {
                // Result message — extract usage and session
                const sessionID = msg.session_id || msg.sessionId;
                if (sessionID) {
                    sessions.set(conversation_id, sessionID);
                }

                sendSSE(res, 'done', {
                    conversation_id,
                    session_id: sessionID || '',
                    usage: msg.usage || {},
                    cost_usd: msg.total_cost_usd || msg.totalCostUsd || 0,
                    num_turns: msg.num_turns || msg.numTurns || 0,
                    model: msg.model || '',
                });
            }
        }
    } catch (err) {
        console.error(`[sidecar] Query error: ${err.message}`);
        sendSSE(res, 'error', { message: err.message });
    }

    res.end();
}

/**
 * Handle POST /sessions/:id/close — close a session.
 */
function handleCloseSession(req, res, conversationID) {
    sessions.delete(conversationID);
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ ok: true }));
}

/**
 * Handle GET /health — health check.
 */
function handleHealth(req, res) {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({
        status: 'ok',
        sessions: sessions.size,
        uptime: process.uptime(),
    }));
}

/**
 * Handle POST /shutdown — graceful shutdown.
 */
function handleShutdown(req, res) {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ ok: true }));
    console.log('[sidecar] Shutdown requested');
    setTimeout(() => process.exit(0), 500);
}

// --- HTTP Server ---

const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, `http://localhost:${PORT}`);
    const path = url.pathname;

    try {
        if (req.method === 'POST' && path === '/query') {
            await handleQuery(req, res);
        } else if (req.method === 'POST' && path.startsWith('/sessions/') && path.endsWith('/close')) {
            const id = parseInt(path.split('/')[2]);
            handleCloseSession(req, res, id);
        } else if (req.method === 'GET' && path === '/health') {
            handleHealth(req, res);
        } else if (req.method === 'POST' && path === '/shutdown') {
            handleShutdown(req, res);
        } else {
            res.writeHead(404, { 'Content-Type': 'application/json' });
            res.end(JSON.stringify({ error: 'Not found' }));
        }
    } catch (err) {
        console.error(`[sidecar] Unhandled error: ${err.message}`);
        if (!res.headersSent) {
            res.writeHead(500, { 'Content-Type': 'application/json' });
            res.end(JSON.stringify({ error: err.message }));
        }
    }
});

server.listen(PORT, '127.0.0.1', () => {
    console.log(`[sidecar] OpenPoet Node.js Agent SDK sidecar listening on port ${PORT}`);
    console.log(`[sidecar] API URL: ${API_URL}`);
});

// Graceful shutdown on signals
process.on('SIGTERM', () => {
    console.log('[sidecar] SIGTERM received');
    server.close(() => process.exit(0));
});
process.on('SIGINT', () => {
    console.log('[sidecar] SIGINT received');
    server.close(() => process.exit(0));
});
