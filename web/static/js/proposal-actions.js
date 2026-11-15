/**
 * ProposalActions — the buttons that decide a proposal of the chat assistant.
 *
 * A proposal waits for the user: a change to a project's memory document,
 * tasks to create, change or delete, a project skill, a project tool to run.
 * It is an artifact with a review, read in the artifact viewer, which gets
 * its buttons here.
 *
 *   ProposalActions.build({ id, kind, title, status, close })
 *     -> [{ label, class, onClick }]   (empty once the proposal was decided)
 *
 * id is what the approve and reject routes take (the bare id the chat card
 * carries), kind is "memory", "task", "skill" or "tool", and close() closes
 * the viewer that shows the proposal.
 */
const ProposalActions = {
    build({ id, kind, title, status, close }) {
        if (!id || status === 'approved' || status === 'rejected') return [];
        const done = typeof close === 'function' ? close : () => {};
        const toast = (message, type) => window.app?.showToast(message, type);
        const card = (cardStatus) => window.aiChat?.updateDocCardStatus(id, cardStatus);
        const text = String(title || '');

        // Each kind is decided on its own routes. They are spelled out, one
        // request each, so every route the UI can call is written somewhere.
        const key = encodeURIComponent(id);
        const requests = {
            memory: {
                approve: () => fetch(`/api/memory-doc/approve/${key}`, { method: 'POST' }),
                reject: () => fetch(`/api/memory-doc/reject/${key}`, { method: 'POST' }),
            },
            task: {
                approve: () => fetch(`/api/task-proposal/approve/${key}`, { method: 'POST' }),
                reject: () => fetch(`/api/task-proposal/reject/${key}`, { method: 'POST' }),
            },
            skill: {
                approve: () => fetch(`/api/skill-proposal/approve/${key}`, { method: 'POST' }),
                reject: () => fetch(`/api/skill-proposal/reject/${key}`, { method: 'POST' }),
            },
            tool: {
                reject: () => fetch(`/api/tool-proposal/reject/${key}`, { method: 'POST' }),
            },
        }[kind];
        if (!requests) return [];

        const reject = (label, message) => ({
            label,
            class: 'btn btn-secondary',
            onClick: async () => {
                try {
                    await requests.reject();
                    done();
                    card('rejected');
                    toast(message, 'info');
                } catch (e) {
                    done();
                }
            },
        });
        // Approving reports what the server answers.
        const approve = (label, cssClass, success, failure) => ({
            label,
            class: cssClass,
            onClick: async () => {
                try {
                    const response = await requests.approve();
                    if (response.ok) {
                        const data = await response.json().catch(() => ({}));
                        done();
                        card('approved');
                        toast(success(data), 'success');
                    } else {
                        const error = await response.json().catch(() => ({}));
                        toast(error.error || failure, 'error');
                    }
                } catch (e) {
                    toast(failure, 'error');
                }
            },
        });

        switch (kind) {
            case 'memory':
                return [
                    reject('Reject', 'Proposal rejected.'),
                    approve('Approve', 'btn btn-primary', () => 'Memory doc approved and saved.', 'Error approving'),
                ];
            case 'task': {
                const isDelete = text.startsWith('Delete Task:');
                const isUpdate = text.startsWith('Update Task:');
                const label = isDelete ? 'Approve Deletion' : isUpdate ? 'Approve Change' : 'Approve Task';
                return [
                    reject('Cancel', 'Proposal rejected.'),
                    approve(label, isDelete ? 'btn btn-danger' : 'btn btn-primary', (data) => {
                        const parts = [];
                        if (data.created) parts.push(`${data.created} created`);
                        if (data.updated) parts.push(`${data.updated} updated`);
                        if (data.deleted) parts.push(`${data.deleted} deleted`);
                        return parts.length > 0 ? `Approved! ${parts.join(', ')}.` : 'Approved!';
                    }, 'Error approving'),
                ];
            }
            case 'skill':
                return [
                    reject('Cancel', 'Proposal rejected.'),
                    approve('Approve Skill', 'btn btn-primary', () => 'Skill approved and saved to project.', 'Error approving skill'),
                ];
            case 'tool':
                return [
                    reject('Cancel', 'Tool execution cancelled.'),
                    { label: 'Run Tool', class: 'btn btn-primary', onClick: () => ProposalActions._runTool(id, done) },
                ];
        }
        return [];
    },

    // Approving a tool proposal runs the tool: the viewer closes and the
    // output is streamed into the proposal's card in the chat.
    async _runTool(id, done) {
        done();
        window.aiChat?.setDocCardRunning(id);
        try {
            const response = await fetch(`/api/tool-proposal/approve/${encodeURIComponent(id)}`, { method: 'POST' });
            const reader = response.body.getReader();
            const decoder = new TextDecoder();
            let buffer = '';
            while (true) {
                const { done: finished, value } = await reader.read();
                if (finished) break;
                buffer += decoder.decode(value, { stream: true });
                const events = buffer.split('\n\n');
                buffer = events.pop();
                for (const event of events) {
                    let type = '', payload = '';
                    for (const line of event.split('\n')) {
                        if (line.startsWith('event: ')) type = line.slice(7);
                        else if (line.startsWith('data: ')) payload = line.slice(6);
                    }
                    if (!payload) continue;
                    try {
                        const data = JSON.parse(payload);
                        if (type === 'output') window.aiChat?.setDocCardOutputLine(id, data.line);
                        else if (type === 'done') window.aiChat?.setDocCardCompleted(id, data.exit_code);
                        else if (type === 'error') window.aiChat?.setDocCardError(id, data.message);
                    } catch (_) {}
                }
            }
            // The stream ended without a done event.
            const cards = document.querySelectorAll('.ai-chat-doc-card');
            const own = Array.from(cards).find(el => el.dataset.docId === id);
            if (!own || !own.querySelector('.ai-chat-doc-card-badge-approved')) {
                window.aiChat?.setDocCardCompleted(id, 0);
            }
        } catch (e) {
            window.aiChat?.setDocCardError(id, 'Connection error');
        }
    },
};

window.ProposalActions = ProposalActions;
