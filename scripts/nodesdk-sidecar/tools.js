/**
 * OpenPoet Tools — MCP server for the Node.js Agent SDK sidecar.
 *
 * Each tool proxies its execution to the OpenPoet Go backend via HTTP POST
 * to /api/ai/execute-tool. This avoids duplicating tool logic and keeps the
 * Go backend as the single source of truth.
 */
import { createSdkMcpServer, tool } from '@anthropic-ai/claude-agent-sdk';
import { z } from 'zod';

/**
 * Calls the OpenPoet Go backend to execute a tool.
 * @param {string} apiURL - Base URL of the OpenPoet backend
 * @param {string} toolName - Name of the tool to execute
 * @param {object} args - Tool arguments
 * @param {number} conversationID - Conversation ID for context
 * @returns {Promise<{content: Array, isError?: boolean}>}
 */
async function callOpenPoet(apiURL, toolName, args, conversationID = 0) {
    try {
        const resp = await fetch(`${apiURL}/api/ai/execute-tool`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                name: toolName,
                args: args,
                conversation_id: conversationID,
            }),
        });
        const data = await resp.json();
        if (data.error) {
            return {
                content: [{ type: 'text', text: `Error: ${data.error}` }],
                isError: true,
            };
        }
        return {
            content: [{ type: 'text', text: data.result || 'Done' }],
        };
    } catch (err) {
        return {
            content: [{ type: 'text', text: `Connection error: ${err.message}` }],
            isError: true,
        };
    }
}

/**
 * Builds the OpenPoet MCP server with all tools registered.
 * Tools proxy to the Go backend via HTTP.
 * @param {string} apiURL - Base URL of the OpenPoet Go backend
 * @param {number} conversationID - Conversation ID for context
 * @returns {McpSdkServer}
 */
export function buildOpenPoetTools(apiURL, conversationID = 0) {
    const proxy = (name, args) => callOpenPoet(apiURL, name, args, conversationID);

    return createSdkMcpServer({
        name: 'openpoet',
        version: '1.0.0',
        tools: [
            // Skill management
            tool('create_skill', 'Create a new skill in OpenPoet', {
                name: z.string().describe('Unique name, lowercase with hyphens'),
                content: z.string().describe('Markdown content with instructions'),
                category: z.string().optional().describe('Category label'),
            }, (args) => proxy('create_skill', args)),

            tool('update_skill', 'Update an existing skill by ID', {
                id: z.string().describe('Skill ID'),
                name: z.string().optional().describe('New name'),
                content: z.string().optional().describe('New content'),
                category: z.string().optional().describe('New category'),
            }, (args) => proxy('update_skill', args)),

            tool('delete_skill', 'Delete a skill by ID', {
                id: z.string().describe('Skill ID'),
            }, (args) => proxy('delete_skill', args)),

            tool('list_skills', 'List all skills in OpenPoet', {},
                () => proxy('list_skills', {})),

            // Project management
            tool('list_projects', 'List all projects in OpenPoet', {},
                () => proxy('list_projects', {})),

            // MCP servers
            tool('list_mcp_servers', 'List all MCP servers', {},
                () => proxy('list_mcp_servers', {})),

            tool('create_mcp_server', 'Create a new MCP server configuration', {
                name: z.string().describe('Server name'),
                command: z.string().describe('Command to run'),
                args: z.string().optional().describe('JSON array of arguments'),
                env: z.string().optional().describe('JSON object of env vars'),
            }, (args) => proxy('create_mcp_server', args)),

            // Settings
            tool('update_setting', 'Update a OpenPoet setting', {
                key: z.string().describe('Setting key'),
                value: z.string().describe('Setting value'),
            }, (args) => proxy('update_setting', args)),

            tool('sync_config', 'Sync configuration to all projects', {},
                () => proxy('sync_config', {})),

            // Memory docs
            tool('get_memory_doc', 'Get the memory doc (CLAUDE.md) for a project', {
                project_id: z.string().describe('Project ID'),
            }, (args) => proxy('get_memory_doc', args)),

            tool('update_memory_doc', 'Propose changes to the memory doc', {
                project_id: z.string().describe('Project ID'),
                content: z.string().describe('Full markdown content'),
                summary: z.string().optional().describe('Brief summary of changes'),
            }, (args) => proxy('update_memory_doc', args)),

            // Task management
            tool('list_tasks', 'List all tasks for a project', {
                project_id: z.string().describe('Project ID'),
            }, (args) => proxy('list_tasks', args)),

            tool('create_task', 'Create a new task in a project', {
                project_id: z.string().describe('Project ID'),
                title: z.string().describe('Task title'),
                description: z.string().optional().describe('Task description'),
                status: z.string().optional().describe('Status: todo, in_progress, awaiting_approval, done'),
                priority: z.string().optional().describe('Priority: low, medium, high, urgent'),
                due_date: z.string().optional().describe('Due date ISO 8601'),
                parent_id: z.string().optional().describe('Parent task ID for subtasks'),
            }, (args) => proxy('create_task', args)),

            tool('update_task', 'Update an existing task', {
                project_id: z.string().describe('Project ID'),
                task_id: z.string().describe('Task ID'),
                title: z.string().optional().describe('New title'),
                description: z.string().optional().describe('New description'),
                status: z.string().optional().describe('New status'),
                priority: z.string().optional().describe('New priority'),
                due_date: z.string().optional().describe('New due date'),
            }, (args) => proxy('update_task', args)),

            tool('delete_task', 'Delete a task and its subtasks', {
                project_id: z.string().describe('Project ID'),
                task_id: z.string().describe('Task ID'),
            }, (args) => proxy('delete_task', args)),

            tool('get_task_report', 'Get task summary report for a project', {
                project_id: z.string().describe('Project ID'),
            }, (args) => proxy('get_task_report', args)),

            // Documents
            tool('create_document', 'Create a temporary markdown document', {
                title: z.string().describe('Short document title'),
                content: z.string().describe('Full markdown content'),
            }, (args) => proxy('create_document', args)),

            // File exploration
            tool('list_directory', 'List files and directories in a project', {
                project_id: z.string().describe('Project ID'),
                path: z.string().optional().describe('Relative path'),
            }, (args) => proxy('list_directory', args)),

            tool('read_file', 'Read the content of a file from a project', {
                project_id: z.string().describe('Project ID'),
                path: z.string().describe('Relative file path'),
                offset: z.string().optional().describe('Line number to start from'),
                limit: z.string().optional().describe('Number of lines to read'),
            }, (args) => proxy('read_file', args)),

            tool('find_files', 'Find files matching a glob pattern in a project', {
                project_id: z.string().describe('Project ID'),
                pattern: z.string().describe('Glob pattern'),
            }, (args) => proxy('find_files', args)),

            tool('grep_content', 'Search file contents using regex in a project', {
                project_id: z.string().describe('Project ID'),
                pattern: z.string().describe('Regex pattern'),
                path: z.string().optional().describe('Subdirectory to restrict search'),
                glob: z.string().optional().describe('File name filter'),
            }, (args) => proxy('grep_content', args)),

        ],
    });
}
