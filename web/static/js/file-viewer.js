// File Viewer - displays text files in an overlay with Markdown rendering or line numbers
class FileViewer {
    static VIEWABLE_EXTENSIONS = new Set([
        '.md', '.markdown', '.txt', '.log', '.csv',
        '.js', '.ts', '.jsx', '.tsx', '.mjs', '.cjs',
        '.go', '.py', '.rb', '.java', '.c', '.cpp', '.h', '.cs', '.rs', '.swift',
        '.sh', '.bash', '.zsh', '.fish',
        '.yaml', '.yml', '.toml', '.json', '.xml', '.html', '.css', '.scss', '.less',
        '.sql', '.lua', '.php', '.vue', '.svelte',
        '.dockerfile', '.makefile', '.gitignore',
        '.ini', '.conf', '.cfg', '.env.example',
        '.r', '.m', '.kt', '.scala', '.zig', '.nim',
        '.graphql', '.proto', '.tf', '.hcl',
        '.bat', '.ps1', '.cmd',
    ]);

    static MARKDOWN_EXTENSIONS = new Set(['.md', '.markdown']);

    constructor() {
        this.overlay = document.getElementById('file-viewer-overlay');
        this.nameEl = document.getElementById('file-viewer-name');
        this.sizeEl = document.getElementById('file-viewer-size');
        this.contentEl = document.getElementById('file-viewer-content');
        this.loadingEl = document.getElementById('file-viewer-loading');
        this.errorEl = document.getElementById('file-viewer-error');
        this.errorMsgEl = document.getElementById('file-viewer-error-msg');
        this.markdownEl = document.getElementById('file-viewer-markdown');
        this.codeEl = document.getElementById('file-viewer-code');
        this.codeBodyEl = document.getElementById('file-viewer-code-body');

        this.currentPath = '';
        this.setupEventListeners();
    }

    setupEventListeners() {
        // Close button
        document.getElementById('file-viewer-close')?.addEventListener('click', () => this.hide());

        // Download button
        document.getElementById('file-viewer-download')?.addEventListener('click', () => this.download());

        // Error download button
        document.getElementById('file-viewer-error-download')?.addEventListener('click', () => this.download());

        // Click on overlay background to close
        this.overlay?.addEventListener('click', (e) => {
            if (e.target === this.overlay) {
                this.hide();
            }
        });

        // Escape key to close
        document.addEventListener('keydown', (e) => {
            if (e.key === 'Escape' && this.overlay && !this.overlay.classList.contains('hidden')) {
                this.hide();
            }
        });
    }

    static isViewable(filename) {
        if (!filename) return false;
        const lower = filename.toLowerCase();
        // Check for exact filenames (Dockerfile, Makefile, etc.)
        const baseName = lower.split('/').pop();
        if (baseName === 'dockerfile' || baseName === 'makefile' || baseName === '.gitignore' || baseName === '.env.example') {
            return true;
        }
        const ext = '.' + baseName.split('.').slice(1).join('.');
        if (FileViewer.VIEWABLE_EXTENSIONS.has(ext)) return true;
        // Try just the last extension
        const lastDotIdx = baseName.lastIndexOf('.');
        if (lastDotIdx >= 0) {
            return FileViewer.VIEWABLE_EXTENSIONS.has(baseName.substring(lastDotIdx));
        }
        return false;
    }

    static isMarkdown(filename) {
        if (!filename) return false;
        const lower = filename.toLowerCase();
        const lastDotIdx = lower.lastIndexOf('.');
        if (lastDotIdx >= 0) {
            return FileViewer.MARKDOWN_EXTENSIONS.has(lower.substring(lastDotIdx));
        }
        return false;
    }

    async show(filePath) {
        this.currentPath = filePath;
        const sessionId = app.currentSession;
        if (!sessionId) return;

        // Show overlay with loading
        this.overlay.classList.remove('hidden');
        this.showState('loading');

        // Set filename
        const filename = filePath.split('/').pop();
        this.nameEl.textContent = filename;
        this.sizeEl.textContent = '';

        try {
            const response = await fetch(`/api/sessions/${sessionId}/files/view/${filePath}`);

            if (!response.ok) {
                const ct = response.headers.get('content-type') || '';
                let errorMsg = 'Failed to load file';
                if (ct.includes('application/json')) {
                    const err = await response.json();
                    errorMsg = err.error || errorMsg;
                }

                if (response.status === 413) {
                    errorMsg = 'File is too large to view (max 2MB). Use download instead.';
                } else if (response.status === 415) {
                    errorMsg = 'Binary file cannot be viewed. Use download instead.';
                }

                this.showError(errorMsg);
                return;
            }

            const data = await response.json();

            // Update header info
            this.nameEl.textContent = data.name;
            this.sizeEl.textContent = this.formatSize(data.size);

            // Render content
            if (FileViewer.isMarkdown(data.name)) {
                this.renderMarkdown(data.content);
            } else {
                this.renderCode(data.content);
            }
        } catch (error) {
            console.error('FileViewer error:', error);
            this.showError('Failed to load file: ' + error.message);
        }
    }

    renderMarkdown(content) {
        if (typeof marked !== 'undefined') {
            // Configure marked for safe rendering
            marked.setOptions({
                breaks: true,
                gfm: true,
            });
            this.markdownEl.innerHTML = marked.parse(content);
        } else {
            // Fallback: render as code if marked is not available
            this.renderCode(content);
            return;
        }
        this.showState('markdown');
    }

    renderCode(content) {
        const lines = content.split('\n');
        // Remove trailing empty line if file ends with newline
        if (lines.length > 1 && lines[lines.length - 1] === '') {
            lines.pop();
        }

        let html = '';
        for (let i = 0; i < lines.length; i++) {
            html += `<tr><td class="line-number">${i + 1}</td><td class="line-content">${this.escapeHtml(lines[i])}</td></tr>`;
        }
        this.codeBodyEl.innerHTML = html;
        this.showState('code');
    }

    showState(state) {
        this.loadingEl.classList.add('hidden');
        this.errorEl.classList.add('hidden');
        this.markdownEl.classList.add('hidden');
        this.codeEl.classList.add('hidden');

        switch (state) {
            case 'loading':
                this.loadingEl.classList.remove('hidden');
                break;
            case 'error':
                this.errorEl.classList.remove('hidden');
                break;
            case 'markdown':
                this.markdownEl.classList.remove('hidden');
                break;
            case 'code':
                this.codeEl.classList.remove('hidden');
                break;
        }
    }

    showError(message) {
        this.errorMsgEl.textContent = message;
        this.showState('error');
    }

    download() {
        if (!this.currentPath) return;
        const sessionId = app.currentSession;
        if (!sessionId) return;

        const url = `/api/sessions/${sessionId}/files/${this.currentPath}`;
        window.open(url, '_blank');
    }

    showPlanContent(content, title) {
        this.overlay.classList.remove('hidden');
        this.currentPath = '';
        this.nameEl.textContent = title || 'Plan';
        this.sizeEl.textContent = this.formatSize(content.length);
        this.renderMarkdown(content);
    }

    hide() {
        this.overlay?.classList.add('hidden');
        // Clean up content
        this.markdownEl.innerHTML = '';
        this.codeBodyEl.innerHTML = '';
        this.currentPath = '';

        // Refocus terminal
        if (window.terminalManager) {
            window.terminalManager.focus();
        }
    }

    formatSize(bytes) {
        if (bytes === 0) return '0 B';
        const k = 1024;
        const sizes = ['B', 'KB', 'MB', 'GB'];
        const i = Math.floor(Math.log(bytes) / Math.log(k));
        return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
    }

    escapeHtml(text) {
        const div = document.createElement('div');
        div.textContent = text || '';
        return div.innerHTML;
    }
}

// Initialize file viewer
window.fileViewer = new FileViewer();
