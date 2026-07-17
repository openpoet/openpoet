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
        this.downloadBtn = document.getElementById('doc-review-download');
        this.downloadPDFBtn = document.getElementById('doc-review-download-pdf');

        if (!this.overlay) return;
        this._history = [];
        this._currentContent = null;
        this._currentTitle = null;
        this._currentURLState = null;

        this.closeBtn?.addEventListener('click', () => this.close());
        this.downloadBtn?.addEventListener('click', () => this._downloadDocument());
        this.downloadPDFBtn?.addEventListener('click', () => this._downloadPDF());
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

    async open(docId, opts = {}) {
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
            const openDocument = (extra = {}) => this.openWithContent(doc.title || 'Document', doc.content, {
                ...opts,
                ...extra,
                urlState: opts.urlState || { kind: 'document', id: docId, projectId: doc.project_id || null }
            });

            if (isPendingMemoryDoc) {
                openDocument({
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

                openDocument({
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
            } else if (doc.title && doc.title.startsWith('Tool:') && doc.status !== 'approved' && doc.status !== 'rejected') {
                openDocument({
                    actions: [
                        {
                            label: 'Cancel',
                            class: 'btn btn-secondary',
                            onClick: async () => {
                                try {
                                    await fetch(`/api/tool-proposal/reject/${docId}`, { method: 'POST' });
                                    this.close();
                                    window.aiChat?.updateDocCardStatus(docId, 'rejected');
                                    window.app?.showToast('Tool execution cancelled.', 'info');
                                } catch (e) {
                                    this.close();
                                }
                            }
                        },
                        {
                            label: 'Run Tool',
                            class: 'btn btn-primary',
                            onClick: async () => {
                                this.close();
                                window.aiChat?.setDocCardRunning(docId);
                                try {
                                    const resp = await fetch(`/api/tool-proposal/approve/${docId}`, { method: 'POST' });
                                    const reader = resp.body.getReader();
                                    const decoder = new TextDecoder();
                                    let buffer = '';

                                    while (true) {
                                        const { done, value } = await reader.read();
                                        if (done) break;
                                        buffer += decoder.decode(value, { stream: true });
                                        const events = buffer.split('\n\n');
                                        buffer = events.pop();
                                        for (const evt of events) {
                                            let evtType = '', evtData = '';
                                            for (const line of evt.split('\n')) {
                                                if (line.startsWith('event: ')) evtType = line.slice(7);
                                                else if (line.startsWith('data: ')) evtData = line.slice(6);
                                            }
                                            if (!evtData) continue;
                                            try {
                                                const d = JSON.parse(evtData);
                                                if (evtType === 'output') {
                                                    window.aiChat?.setDocCardOutputLine(docId, d.line);
                                                } else if (evtType === 'done') {
                                                    window.aiChat?.setDocCardCompleted(docId, d.exit_code);
                                                } else if (evtType === 'error') {
                                                    window.aiChat?.setDocCardError(docId, d.message);
                                                }
                                            } catch (_) {}
                                        }
                                    }
                                    // If stream ended without a done event
                                    if (!document.querySelector(`.ai-chat-doc-card[data-doc-id="${docId}"] .ai-chat-doc-card-badge-approved`)) {
                                        window.aiChat?.setDocCardCompleted(docId, 0);
                                    }
                                } catch (e) {
                                    window.aiChat?.setDocCardError(docId, 'Connection error');
                                }
                            }
                        }
                    ]
                });
            } else if (isSkillProposal) {
                openDocument({
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
                openDocument();
            }
        } catch (e) {
            console.error('DocViewer: failed to open document', e);
        }
    }

    openWithContent(title, content, opts = {}) {
        if (!this.overlay) return;

        // If overlay is already visible, push current state to history stack
        if (!this.overlay.classList.contains('hidden') && opts.recordHistory !== false) {
            // Move footer children to a fragment to preserve event listeners
            const footerFragment = document.createDocumentFragment();
            while (this.footerEl.firstChild) {
                footerFragment.appendChild(this.footerEl.firstChild);
            }
            this._history.push({
                title: this.nameEl.textContent,
                html: this.contentEl.innerHTML,
                rawContent: this._currentContent,
                rawTitle: this._currentTitle,
                footerFragment,
                footerHidden: this.footerEl.classList.contains('hidden'),
                footerVersion: this.footerEl.dataset.version || '',
                onClose: this._onClose || null,
                urlState: this._currentURLState
            });
        }

        this._onClose = opts.onClose || null;
        this._currentContent = content || '';
        this._currentTitle = title || 'Document';
        this._currentURLState = opts.urlState || null;
        this.nameEl.textContent = this._currentTitle;
        this.contentEl.innerHTML = this._renderMarkdown(this._currentContent);
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
                btn.className = 'btn-icon-round' + (action.aiAction ? ' ai-action' : '') + (action.danger ? ' danger-action' : '');
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
        if (opts.updateURL !== false) {
            this._notifyURLStateChanged({ replace: !!opts.replaceURL });
        }
    }

    close(options = {}) {
        if (!this.overlay) return;

        // If there's a previous state, restore it instead of closing
        if (this._history.length > 0) {
            const prev = this._history.pop();
            this.nameEl.textContent = prev.title;
            this.contentEl.innerHTML = prev.html;
            this._currentContent = prev.rawContent || null;
            this._currentTitle = prev.rawTitle || null;
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
            this._currentURLState = prev.urlState || null;
            if (options.updateURL !== false) this._notifyURLStateChanged();
            return;
        }

        this.dismiss(options);
    }

    dismiss(options = {}) {
        if (!this.overlay) return;

        if (this._onClose) {
            this._onClose();
            this._onClose = null;
        }
        this._currentContent = null;
        this._currentTitle = null;
        this._currentURLState = null;
        this.overlay.classList.add('hidden');
        this._history = [];
        if (options.updateURL !== false) this._notifyURLStateChanged();
    }

    getURLState() {
        if (!this.overlay || this.overlay.classList.contains('hidden')) return null;
        return this._currentURLState;
    }

    _notifyURLStateChanged(options = {}) {
        window.app?._onDocumentURLStateChanged?.(options);
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

    _downloadDocument() {
        if (!this._currentContent) return;
        const safeName = this._documentFileStem();
        const blob = new Blob([this._currentContent], { type: 'text/markdown;charset=utf-8' });
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = `${safeName}.md`;
        document.body.appendChild(a);
        a.click();
        document.body.removeChild(a);
        setTimeout(() => URL.revokeObjectURL(url), 1000);
    }

    _documentFileStem() {
        return (this._currentTitle || 'document')
            .replace(/[^a-zA-Z0-9\s-]/g, '')
            .replace(/\s+/g, '-')
            .toLowerCase()
            .substring(0, 60) || 'document';
    }

    async _downloadPDF() {
        if (!this._currentContent || !this.contentEl) return;

        const iframe = document.createElement('iframe');
        iframe.title = `PDF preview: ${this._currentTitle || 'Document'}`;
        iframe.style.position = 'fixed';
        iframe.style.width = '1px';
        iframe.style.height = '1px';
        iframe.style.right = '0';
        iframe.style.bottom = '0';
        iframe.style.opacity = '0';
        iframe.style.pointerEvents = 'none';
        document.body.appendChild(iframe);

        const printWindow = iframe.contentWindow;
        const printDoc = iframe.contentDocument;
        if (!printWindow || !printDoc) {
            iframe.remove();
            window.app?.showToast?.('Error', 'Unable to prepare PDF', 'error');
            return;
        }

        printDoc.open();
        printDoc.write('<!doctype html><html><head></head><body></body></html>');
        printDoc.close();
        printDoc.title = `${this._documentFileStem()}.pdf`;

        const base = printDoc.createElement('base');
        base.href = `${window.location.origin}/`;
        printDoc.head.appendChild(base);

        const style = printDoc.createElement('style');
        style.textContent = `
            @page { size: A4; margin: 18mm 16mm; }
            * { box-sizing: border-box; }
            html, body { color: #171717; background: #fff; }
            body { margin: 0; font: 11pt/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
            h1 { margin: 0 0 18pt; font-size: 22pt; line-height: 1.2; }
            h2, h3, h4 { break-after: avoid; margin: 18pt 0 8pt; line-height: 1.3; }
            p, blockquote, pre, table, ul, ol { margin: 0 0 10pt; }
            img, svg { max-width: 100%; height: auto; break-inside: avoid; }
            pre, code { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
            pre { white-space: pre-wrap; overflow-wrap: anywhere; padding: 10pt; background: #f5f5f5; border-radius: 4pt; }
            code { background: #f3f3f3; padding: 1pt 3pt; border-radius: 2pt; }
            pre code { background: transparent; padding: 0; }
            blockquote { padding-left: 10pt; border-left: 3pt solid #bbb; color: #444; }
            table { width: 100%; border-collapse: collapse; break-inside: auto; }
            tr { break-inside: avoid; }
            th, td { padding: 5pt 6pt; border: 0.5pt solid #bbb; text-align: left; vertical-align: top; }
            a { color: #1558a6; text-decoration: underline; overflow-wrap: anywhere; }
            button, input, textarea, select, .task-verification-spinner { display: none !important; }
        `;
        printDoc.head.appendChild(style);

        const heading = printDoc.createElement('h1');
        heading.textContent = this._currentTitle || 'Document';
        printDoc.body.appendChild(heading);

        const main = printDoc.createElement('main');
        main.innerHTML = this.contentEl.innerHTML;
        main.querySelectorAll('script').forEach(el => el.remove());
        printDoc.body.appendChild(main);

        const imageLoads = Array.from(main.querySelectorAll('img')).map(img => {
            if (img.complete) return Promise.resolve();
            return new Promise(resolve => {
                img.addEventListener('load', resolve, { once: true });
                img.addEventListener('error', resolve, { once: true });
                setTimeout(resolve, 1500);
            });
        });
        await Promise.all(imageLoads);
        if (printDoc.fonts?.ready) await printDoc.fonts.ready.catch(() => {});

        let cleaned = false;
        const cleanup = () => {
            if (cleaned) return;
            cleaned = true;
            iframe.remove();
        };
        printWindow.addEventListener('afterprint', cleanup, { once: true });
        setTimeout(cleanup, 60000);

        try {
            printWindow.focus();
            printWindow.print();
        } catch (error) {
            cleanup();
            window.app?.showToast?.('Error', 'Unable to open PDF save dialog', 'error');
        }
    }

    _escapeHtml(str) {
        if (!str) return '';
        return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }
}

document.addEventListener('DOMContentLoaded', () => {
    window.docViewer = new DocViewer();
});
