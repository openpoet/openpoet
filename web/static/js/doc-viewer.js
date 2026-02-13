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
    }

    async open(docId) {
        try {
            const resp = await fetch(`/api/documents/${docId}`);
            if (!resp.ok) throw new Error('Document not found');
            const doc = await resp.json();

            const isPendingMemoryDoc = doc.title && doc.title.startsWith('Memory Doc:');
            const isTaskProposal = doc.title && (doc.title.startsWith('Tarefa:') || doc.title.startsWith('Planejamento:'));

            if (isPendingMemoryDoc) {
                this.openWithContent(doc.title, doc.content, {
                    actions: [
                        {
                            label: 'Rejeitar',
                            class: 'btn btn-secondary',
                            onClick: async () => {
                                try {
                                    await fetch(`/api/memory-doc/reject/${docId}`, { method: 'POST' });
                                    this.close();
                                    window.aiChat?.updateDocCardStatus(docId, 'rejected');
                                    window.app?.showToast('Proposta rejeitada.', 'info');
                                } catch (e) {
                                    this.close();
                                }
                            }
                        },
                        {
                            label: 'Aprovar',
                            class: 'btn btn-primary',
                            onClick: async () => {
                                try {
                                    const r = await fetch(`/api/memory-doc/approve/${docId}`, { method: 'POST' });
                                    if (r.ok) {
                                        this.close();
                                        window.aiChat?.updateDocCardStatus(docId, 'approved');
                                        window.app?.showToast('Memory doc aprovado e salvo.', 'success');
                                    } else {
                                        const err = await r.json();
                                        window.app?.showToast(err.error || 'Erro ao aprovar', 'error');
                                    }
                                } catch (e) {
                                    window.app?.showToast('Erro ao aprovar', 'error');
                                }
                            }
                        }
                    ]
                });
            } else if (isTaskProposal) {
                this.openWithContent(doc.title, doc.content, {
                    actions: [
                        {
                            label: 'Rejeitar',
                            class: 'btn btn-secondary',
                            onClick: async () => {
                                try {
                                    await fetch(`/api/task-proposal/reject/${docId}`, { method: 'POST' });
                                    this.close();
                                    window.aiChat?.updateDocCardStatus(docId, 'rejected');
                                    window.app?.showToast('Proposta de tarefa rejeitada.', 'info');
                                } catch (e) {
                                    this.close();
                                }
                            }
                        },
                        {
                            label: 'Aprovar Tarefa',
                            class: 'btn btn-primary',
                            onClick: async () => {
                                try {
                                    const r = await fetch(`/api/task-proposal/approve/${docId}`, { method: 'POST' });
                                    if (r.ok) {
                                        const data = await r.json();
                                        this.close();
                                        window.aiChat?.updateDocCardStatus(docId, 'approved');
                                        const msg = `Tarefa aprovada! ${data.created || 0} criada(s), ${data.updated || 0} atualizada(s).`;
                                        window.app?.showToast(msg, 'success');
                                    } else {
                                        const err = await r.json();
                                        window.app?.showToast(err.error || 'Erro ao aprovar tarefa', 'error');
                                    }
                                } catch (e) {
                                    window.app?.showToast('Erro ao aprovar tarefa', 'error');
                                }
                            }
                        }
                    ]
                });
            } else {
                this.openWithContent(doc.title || 'Documento', doc.content);
            }
        } catch (e) {
            console.error('DocViewer: failed to open document', e);
        }
    }

    openWithContent(title, content, opts = {}) {
        if (!this.overlay) return;

        this.nameEl.textContent = title || 'Documento';
        this.contentEl.innerHTML = this._renderMarkdown(content || '');
        // Defer mermaid rendering to ensure DOM is stable
        requestAnimationFrame(() => {
            if (typeof FileViewer !== 'undefined') FileViewer.renderMermaid(this.contentEl);
        });

        // Build footer actions
        this.footerEl.innerHTML = '';
        if (opts.actions && opts.actions.length > 0) {
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
        if (this.overlay) {
            this.overlay.classList.add('hidden');
        }
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
