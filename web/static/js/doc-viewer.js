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

        // Long-press tooltip for icon buttons on mobile
        this._setupLongPressTooltip();
    }

    _setupLongPressTooltip() {
        let pressTimer = null;
        let tooltip = null;
        const LONG_PRESS_MS = 400;

        const showTooltip = (btn) => {
            const text = btn.dataset.tooltip || btn.title;
            if (!text) return;
            removeTooltip();
            tooltip = document.createElement('div');
            tooltip.className = 'icon-tooltip';
            tooltip.textContent = text;
            document.body.appendChild(tooltip);
            const rect = btn.getBoundingClientRect();
            tooltip.style.left = rect.left + rect.width / 2 + 'px';
            tooltip.style.top = rect.top - 8 + 'px';
            requestAnimationFrame(() => tooltip?.classList.add('visible'));
        };

        const removeTooltip = () => {
            if (tooltip) {
                tooltip.remove();
                tooltip = null;
            }
        };

        const onTouchStart = (e) => {
            const btn = e.target.closest('.btn-icon-round[data-tooltip]');
            if (!btn) return;
            pressTimer = setTimeout(() => {
                showTooltip(btn);
                // Prevent the click from firing after long-press
                btn.addEventListener('click', preventClick, { once: true, capture: true });
            }, LONG_PRESS_MS);
        };

        const preventClick = (e) => { e.stopImmediatePropagation(); e.preventDefault(); };

        const onTouchEnd = () => {
            clearTimeout(pressTimer);
            pressTimer = null;
            setTimeout(removeTooltip, 1200);
        };

        this.overlay?.addEventListener('touchstart', onTouchStart, { passive: true });
        this.overlay?.addEventListener('touchend', onTouchEnd, { passive: true });
        this.overlay?.addEventListener('touchcancel', onTouchEnd, { passive: true });
    }

    async open(docId) {
        try {
            const resp = await fetch(`/api/documents/${docId}`);
            if (!resp.ok) throw new Error('Document not found');
            const doc = await resp.json();

            const isPendingMemoryDoc = doc.title && doc.title.startsWith('Memory Doc:');
            const isTaskDelete = doc.title && doc.title.startsWith('Excluir Tarefa:');
            const isTaskUpdate = doc.title && doc.title.startsWith('Atualizar Tarefa:');
            const isTaskProposal = isTaskDelete || isTaskUpdate ||
                (doc.title && doc.title.startsWith('Tarefa:'));
            const isSkillProposal = doc.title && doc.title.startsWith('Skill:');

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
                const approveLabel = isTaskDelete ? 'Aprovar Exclusão' :
                                     isTaskUpdate ? 'Aprovar Alteração' :
                                     'Aprovar Tarefa';
                const approveClass = isTaskDelete ? 'btn btn-danger' : 'btn btn-primary';

                this.openWithContent(doc.title, doc.content, {
                    actions: [
                        {
                            label: 'Cancelar',
                            class: 'btn btn-secondary',
                            onClick: async () => {
                                try {
                                    await fetch(`/api/task-proposal/reject/${docId}`, { method: 'POST' });
                                    this.close();
                                    window.aiChat?.updateDocCardStatus(docId, 'rejected');
                                    window.app?.showToast('Proposta rejeitada.', 'info');
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
                                        if (data.created) parts.push(`${data.created} criada(s)`);
                                        if (data.updated) parts.push(`${data.updated} atualizada(s)`);
                                        if (data.deleted) parts.push(`${data.deleted} excluída(s)`);
                                        const msg = parts.length > 0
                                            ? `Aprovado! ${parts.join(', ')}.`
                                            : 'Aprovado!';
                                        window.app?.showToast(msg, 'success');
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
            } else if (isSkillProposal) {
                this.openWithContent(doc.title, doc.content, {
                    actions: [
                        {
                            label: 'Cancelar',
                            class: 'btn btn-secondary',
                            onClick: async () => {
                                try {
                                    await fetch(`/api/skill-proposal/reject/${docId}`, { method: 'POST' });
                                    this.close();
                                    window.aiChat?.updateDocCardStatus(docId, 'rejected');
                                    window.app?.showToast('Proposta rejeitada.', 'info');
                                } catch (e) {
                                    this.close();
                                }
                            }
                        },
                        {
                            label: 'Aprovar Skill',
                            class: 'btn btn-primary',
                            onClick: async () => {
                                try {
                                    const r = await fetch(`/api/skill-proposal/approve/${docId}`, { method: 'POST' });
                                    if (r.ok) {
                                        this.close();
                                        window.aiChat?.updateDocCardStatus(docId, 'approved');
                                        window.app?.showToast('Skill aprovada e salva no projeto.', 'success');
                                    } else {
                                        const err = await r.json();
                                        window.app?.showToast(err.error || 'Erro ao aprovar skill', 'error');
                                    }
                                } catch (e) {
                                    window.app?.showToast('Erro ao aprovar skill', 'error');
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
        this.nameEl.textContent = title || 'Documento';
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
