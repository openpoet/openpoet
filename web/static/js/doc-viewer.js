/**
 * DocViewer — reusable document viewer overlay.
 * Usage:
 *   window.docViewer.open(docId)                          // fetch from /api/documents/:id
 *   window.docViewer.openWithContent(title, content, opts) // open with raw content
 *
 * Options:
 *   actions: [{label, class, onClick}]  — custom footer buttons
 */
class DocViewer {
    constructor() {
        this.overlay = document.getElementById('doc-review-overlay');
        this.nameEl = document.getElementById('doc-review-name');
        this.contentEl = document.getElementById('doc-review-content');
        this.footerEl = document.getElementById('doc-review-footer');
        this.closeBtn = document.getElementById('doc-review-close');

        if (!this.overlay) return;
        this._history = [];

        this.closeBtn?.addEventListener('click', () => this.close());
        this.overlay.addEventListener('click', (e) => {
            if (e.target === this.overlay) this.close();
        });
        this._escHandler = (e) => {
            if (e.key === 'Escape' && !this.overlay.classList.contains('hidden')) {
                this.close();
            }
        };
        document.addEventListener('keydown', this._escHandler);

        // Event delegation for data-action="open-doc" links inside content
        this.contentEl?.addEventListener('click', (e) => {
            const link = e.target.closest('a[data-action="open-doc"]');
            if (link) {
                e.preventDefault();
                const docId = link.dataset.docId;
                const docType = link.dataset.docType || 'document';
                if (docId && window.app) {
                    window.app.openTaskDoc(docId, docType);
                }
            }
            // Event delegation for skip-verification button
            const skipBtn = e.target.closest('[data-action="skip-verification"]');
            if (skipBtn) {
                const card = skipBtn.closest('.task-verification-spinner');
                if (card) {
                    card.remove();
                }
            }
        });

    }

    async open(docId) {
        try {
            const resp = await fetch(`/api/documents/${docId}`);
            if (!resp.ok) throw new Error('Document not found');
            const doc = await resp.json();

            const isPendingMemoryDoc = doc.title && doc.title.startsWith('Memory Doc:');
            const isTaskDelete = doc.title && doc.title.startsWith('Delete Task:');
            const isTaskUpdate = doc.title && doc.title.startsWith('Update Task:');
            const isTaskProposal = isTaskDelete || isTaskUpdate ||
                (doc.title && doc.title.startsWith('Task:'));
            const isSkillProposal = doc.title && doc.title.startsWith('Skill:');

            if (isPendingMemoryDoc) {
                this.openWithContent(doc.title, doc.content, {
                    actions: [
                        {
                            label: 'Reject',
                            class: 'btn btn-secondary',
                            onClick: async () => {
                                try {
                                    await fetch(`/api/memory-doc/reject/${docId}`, { method: 'POST' });
                                    this.close();
                                    window.aiChat?.updateDocCardStatus(docId, 'rejected');
                                    window.app?.showToast('Proposal rejected.', 'info');
                                } catch (e) {
                                    this.close();
                                }
                            }
                        },
                        {
                            label: 'Approve',
                            class: 'btn btn-primary',
                            onClick: async () => {
                                try {
                                    const r = await fetch(`/api/memory-doc/approve/${docId}`, { method: 'POST' });
                                    if (r.ok) {
                                        this.close();
                                        window.aiChat?.updateDocCardStatus(docId, 'approved');
                                        window.app?.showToast('Memory doc approved and saved.', 'success');
                                    } else {
                                        const err = await r.json();
                                        window.app?.showToast(err.error || 'Error approving', 'error');
                                    }
                                } catch (e) {
                                    window.app?.showToast('Error approving', 'error');
                                }
                            }
                        }
                    ]
                });
            } else if (isTaskProposal) {
                const approveLabel = isTaskDelete ? 'Approve Deletion' :
                                     isTaskUpdate ? 'Approve Change' :
                                     'Approve Task';
                const approveClass = isTaskDelete ? 'btn btn-danger' : 'btn btn-primary';

                this.openWithContent(doc.title, doc.content, {
                    actions: [
                        {
                            label: 'Cancel',
                            class: 'btn btn-secondary',
                            onClick: async () => {
                                try {
                                    await fetch(`/api/task-proposal/reject/${docId}`, { method: 'POST' });
                                    this.close();
                                    window.aiChat?.updateDocCardStatus(docId, 'rejected');
                                    window.app?.showToast('Proposal rejected.', 'info');
                                } catch (e) {
                                    this.close();
                                }
                            }
                        },
                        {
                            label: approveLabel,
                            class: approveClass,
                            onClick: async () => {
                                try {
                                    const r = await fetch(`/api/task-proposal/approve/${docId}`, { method: 'POST' });
                                    if (r.ok) {
                                        const data = await r.json();
                                        this.close();
                                        window.aiChat?.updateDocCardStatus(docId, 'approved');
                                        const parts = [];
                                        if (data.created) parts.push(`${data.created} created`);
                                        if (data.updated) parts.push(`${data.updated} updated`);
                                        if (data.deleted) parts.push(`${data.deleted} deleted`);
                                        const msg = parts.length > 0
                                            ? `Approved! ${parts.join(', ')}.`
                                            : 'Approved!';
                                        window.app?.showToast(msg, 'success');
                                    } else {
                                        const err = await r.json();
                                        window.app?.showToast(err.error || 'Error approving', 'error');
                                    }
                                } catch (e) {
                                    window.app?.showToast('Error approving', 'error');
                                }
                            }
                        }
                    ]
                });
            } else if (isSkillProposal) {
                this.openWithContent(doc.title, doc.content, {
                    actions: [
                        {
                            label: 'Cancel',
                            class: 'btn btn-secondary',
                            onClick: async () => {
                                try {
                                    await fetch(`/api/skill-proposal/reject/${docId}`, { method: 'POST' });
                                    this.close();
                                    window.aiChat?.updateDocCardStatus(docId, 'rejected');
                                    window.app?.showToast('Proposal rejected.', 'info');
                                } catch (e) {
                                    this.close();
                                }
                            }
                        },
                        {
                            label: 'Approve Skill',
                            class: 'btn btn-primary',
                            onClick: async () => {
                                try {
                                    const r = await fetch(`/api/skill-proposal/approve/${docId}`, { method: 'POST' });
                                    if (r.ok) {
                                        this.close();
                                        window.aiChat?.updateDocCardStatus(docId, 'approved');
                                        window.app?.showToast('Skill approved and saved to project.', 'success');
                                    } else {
                                        const err = await r.json();
                                        window.app?.showToast(err.error || 'Error approving skill', 'error');
                                    }
                                } catch (e) {
                                    window.app?.showToast('Error approving skill', 'error');
                                }
                            }
                        }
                    ]
                });
            } else {
                this.openWithContent(doc.title || 'Document', doc.content);
            }
        } catch (e) {
            console.error('DocViewer: failed to open document', e);
        }
    }

    openWithContent(title, content, opts = {}) {
        if (!this.overlay) return;

        // If overlay is already visible, push current state to history stack
        if (!this.overlay.classList.contains('hidden')) {
            // Move footer children to a fragment to preserve event listeners
            const footerFragment = document.createDocumentFragment();
            while (this.footerEl.firstChild) {
                footerFragment.appendChild(this.footerEl.firstChild);
            }
            this._history.push({
                title: this.nameEl.textContent,
                html: this.contentEl.innerHTML,
                footerFragment,
                footerHidden: this.footerEl.classList.contains('hidden'),
                footerVersion: this.footerEl.dataset.version || '',
                onClose: this._onClose || null
            });
        }

        this._onClose = opts.onClose || null;
        this.nameEl.textContent = title || 'Document';
        this.contentEl.innerHTML = this._renderMarkdown(content || '');
        // Defer mermaid rendering to ensure DOM is stable
        requestAnimationFrame(() => {
            if (typeof FileViewer !== 'undefined') FileViewer.renderMermaid(this.contentEl);
        });

        // Prepend/append custom DOM elements into content
        if (opts.prependElement) {
            this.contentEl.insertBefore(opts.prependElement, this.contentEl.firstChild);
        }
        if (opts.appendElement) {
            this.contentEl.appendChild(opts.appendElement);
        }

        // Build footer based on mode
        this.footerEl.innerHTML = '';
        delete this.footerEl.dataset.version;
        const footerMode = opts.footerMode || 'default';

        if (footerMode === 'hidden') {
            this.footerEl.classList.add('hidden');
        } else if (footerMode === 'split' && opts.actions) {
            this.footerEl.dataset.version = 'A';
            const primary = opts.actions.filter(a => a.role === 'primary');
            const secondary = opts.actions.filter(a => a.role !== 'primary');

            const primaryDiv = document.createElement('div');
            primaryDiv.className = 'footer-primary';
            for (const action of primary) {
                const btn = document.createElement('button');
                btn.className = action.class || 'btn btn-secondary';
                btn.textContent = action.label;
                btn.addEventListener('click', action.onClick);
                primaryDiv.appendChild(btn);
            }
            const secondaryDiv = document.createElement('div');
            secondaryDiv.className = 'footer-secondary';
            for (const action of secondary) {
                const btn = document.createElement('button');
                btn.className = 'btn-icon-round' + (action.aiAction ? ' ai-action' : '');
                btn.title = action.label;
                btn.dataset.tooltip = action.label;
                btn.innerHTML = (action.icon || '') + `<span class="icon-label">${this._escapeHtml(action.label)}</span>`;
                btn.addEventListener('click', action.onClick);
                secondaryDiv.appendChild(btn);
            }
            this.footerEl.appendChild(primaryDiv);
            this.footerEl.appendChild(secondaryDiv);
            this.footerEl.classList.remove('hidden');
        } else if (opts.actions && opts.actions.length > 0) {
            // Default mode: flat button list
            for (const action of opts.actions) {
                const btn = document.createElement('button');
                btn.className = action.class || 'btn btn-secondary';
                btn.textContent = action.label;
                btn.addEventListener('click', action.onClick);
                this.footerEl.appendChild(btn);
            }
            this.footerEl.classList.remove('hidden');
        } else {
            this.footerEl.classList.add('hidden');
        }

        this.overlay.classList.remove('hidden');
    }

    close() {
        if (!this.overlay) return;

        // If there's a previous state, restore it instead of closing
        if (this._history.length > 0) {
            const prev = this._history.pop();
            this.nameEl.textContent = prev.title;
            this.contentEl.innerHTML = prev.html;
            this.footerEl.innerHTML = '';
            this.footerEl.appendChild(prev.footerFragment);
            if (prev.footerVersion) {
                this.footerEl.dataset.version = prev.footerVersion;
            } else {
                delete this.footerEl.dataset.version;
            }
            if (prev.footerHidden) {
                this.footerEl.classList.add('hidden');
            } else {
                this.footerEl.classList.remove('hidden');
            }
            this._onClose = prev.onClose;
            return;
        }

        if (this._onClose) {
            this._onClose();
            this._onClose = null;
        }
        this.overlay.classList.add('hidden');
        this._history = [];
    }

    _renderMarkdown(text) {
        if (!text) return '';
        if (typeof marked !== 'undefined') {
            try {
                // Ensure the mermaid-aware renderer is initialized
                if (typeof FileViewer !== 'undefined') FileViewer._initMarked();
                return marked.parse(text);
            } catch (e) {
                return this._escapeHtml(text);
            }
        }
        return this._escapeHtml(text).replace(/\n/g, '<br>');
    }

    _escapeHtml(str) {
        if (!str) return '';
        return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }
}

document.addEventListener('DOMContentLoaded', () => {
    window.docViewer = new DocViewer();
});
