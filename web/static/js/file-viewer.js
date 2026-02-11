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

    static IMAGE_EXTENSIONS = new Set([
        '.png', '.jpg', '.jpeg', '.gif', '.webp', '.svg',
        '.bmp', '.ico', '.avif', '.apng', '.tiff', '.tif',
    ]);

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
        this.imageContainerEl = document.getElementById('file-viewer-image');
        this.imageEl = document.getElementById('file-viewer-image-el');

        this.currentPath = '';
        this.imageZoom = 1;
        this.imagePanX = 0;
        this.imagePanY = 0;
        this._pinchStartDist = 0;
        this._pinchStartZoom = 1;
        this._panStartX = 0;
        this._panStartY = 0;
        this._panStartPanX = 0;
        this._panStartPanY = 0;
        this._isPanning = false;
        this.setupEventListeners();
        this.setupImageZoom();
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

    setupImageZoom() {
        const container = this.imageContainerEl;
        const img = this.imageEl;
        if (!container || !img) return;

        // Keyboard zoom (+, -, 0)
        this._keyHandler = (e) => {
            if (this.overlay.classList.contains('hidden')) return;
            if (this.imageContainerEl.classList.contains('hidden')) return;
            if (e.key === '+' || e.key === '=') { this.zoomImage(1.25); e.preventDefault(); }
            else if (e.key === '-') { this.zoomImage(0.8); e.preventDefault(); }
            else if (e.key === '0') { this.resetImageZoom(); e.preventDefault(); }
        };
        document.addEventListener('keydown', this._keyHandler);

        // Mouse wheel zoom
        container.addEventListener('wheel', (e) => {
            e.preventDefault();
            const factor = e.deltaY < 0 ? 1.1 : 0.9;
            this.zoomImage(factor);
        }, { passive: false });

        // Double-click to toggle zoom
        img.addEventListener('dblclick', (e) => {
            e.preventDefault();
            if (this.imageZoom > 1.05) {
                this.resetImageZoom();
            } else {
                this.zoomImage(2.5);
            }
        });

        // Pinch-to-zoom (mobile)
        container.addEventListener('touchstart', (e) => {
            if (e.touches.length === 2) {
                e.preventDefault();
                this._pinchStartDist = this._touchDist(e.touches);
                this._pinchStartZoom = this.imageZoom;
            } else if (e.touches.length === 1 && this.imageZoom > 1.05) {
                this._isPanning = true;
                this._panStartX = e.touches[0].clientX;
                this._panStartY = e.touches[0].clientY;
                this._panStartPanX = this.imagePanX;
                this._panStartPanY = this.imagePanY;
            }
        }, { passive: false });

        container.addEventListener('touchmove', (e) => {
            if (e.touches.length === 2) {
                e.preventDefault();
                const dist = this._touchDist(e.touches);
                const scale = dist / this._pinchStartDist;
                this.setImageZoom(this._pinchStartZoom * scale);
            } else if (e.touches.length === 1 && this._isPanning) {
                e.preventDefault();
                const dx = e.touches[0].clientX - this._panStartX;
                const dy = e.touches[0].clientY - this._panStartY;
                this.imagePanX = this._panStartPanX + dx;
                this.imagePanY = this._panStartPanY + dy;
                this.applyImageTransform();
            }
        }, { passive: false });

        container.addEventListener('touchend', (e) => {
            if (e.touches.length < 2) {
                this._pinchStartDist = 0;
            }
            if (e.touches.length === 0) {
                this._isPanning = false;
            }
        });

        // Mouse drag to pan when zoomed (desktop)
        container.addEventListener('mousedown', (e) => {
            if (this.imageZoom <= 1.05) return;
            e.preventDefault();
            this._isPanning = true;
            this._panStartX = e.clientX;
            this._panStartY = e.clientY;
            this._panStartPanX = this.imagePanX;
            this._panStartPanY = this.imagePanY;
            container.style.cursor = 'grabbing';
        });

        document.addEventListener('mousemove', (e) => {
            if (!this._isPanning) return;
            const dx = e.clientX - this._panStartX;
            const dy = e.clientY - this._panStartY;
            this.imagePanX = this._panStartPanX + dx;
            this.imagePanY = this._panStartPanY + dy;
            this.applyImageTransform();
        });

        document.addEventListener('mouseup', () => {
            if (this._isPanning) {
                this._isPanning = false;
                container.style.cursor = this.imageZoom > 1.05 ? 'grab' : '';
            }
        });
    }

    _touchDist(touches) {
        const dx = touches[0].clientX - touches[1].clientX;
        const dy = touches[0].clientY - touches[1].clientY;
        return Math.sqrt(dx * dx + dy * dy);
    }

    zoomImage(factor) {
        this.setImageZoom(this.imageZoom * factor);
    }

    setImageZoom(newZoom) {
        this.imageZoom = Math.max(0.5, Math.min(10, newZoom));
        if (this.imageZoom <= 1.05 && this.imageZoom >= 0.95) {
            this.imageZoom = 1;
            this.imagePanX = 0;
            this.imagePanY = 0;
        }
        this.applyImageTransform();
        this.imageContainerEl.style.cursor = this.imageZoom > 1.05 ? 'grab' : '';
    }

    resetImageZoom() {
        this.imageZoom = 1;
        this.imagePanX = 0;
        this.imagePanY = 0;
        this.applyImageTransform();
        this.imageContainerEl.style.cursor = '';
    }

    applyImageTransform() {
        this.imageEl.style.transform = `translate(${this.imagePanX}px, ${this.imagePanY}px) scale(${this.imageZoom})`;
    }

    static isViewable(filename) {
        if (!filename) return false;
        if (FileViewer.isImage(filename)) return true;
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

    static isImage(filename) {
        if (!filename) return false;
        const lower = filename.toLowerCase();
        const lastDotIdx = lower.lastIndexOf('.');
        if (lastDotIdx >= 0) {
            return FileViewer.IMAGE_EXTENSIONS.has(lower.substring(lastDotIdx));
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

        // Images: use the download endpoint directly as img src
        if (FileViewer.isImage(filename)) {
            const url = `/api/sessions/${sessionId}/files/${filePath}`;
            this.renderImage(url);
            return;
        }

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

    renderImage(url) {
        this.resetImageZoom();
        this.imageEl.onload = () => {
            this.sizeEl.textContent = `${this.imageEl.naturalWidth} × ${this.imageEl.naturalHeight}`;
        };
        this.imageEl.onerror = () => {
            this.showError('Failed to load image.');
        };
        this.imageEl.src = url;
        this.showState('image');
    }

    renderMarkdown(content) {
        if (typeof marked !== 'undefined') {
            FileViewer._initMarked();
            this.markdownEl.innerHTML = marked.parse(content);
            FileViewer.renderMermaid(this.markdownEl);
        } else {
            // Fallback: render as code if marked is not available
            this.renderCode(content);
            return;
        }
        this.showState('markdown');
    }

    static _markedInitialized = false;

    static _initMarked() {
        if (FileViewer._markedInitialized) return;
        FileViewer._markedInitialized = true;
        const renderer = new marked.Renderer();
        const originalCode = renderer.code.bind(renderer);
        renderer.code = function (code, language, escaped) {
            if (language === 'mermaid' || language === 'mermaid-shard') {
                return `<div class="mermaid">${code}</div>`;
            }
            return originalCode(code, language, escaped);
        };
        marked.setOptions({ renderer, breaks: true, gfm: true });
    }

    static renderMermaid(container) {
        if (typeof mermaid !== 'undefined' && container.querySelector('.mermaid')) {
            mermaid.run({ nodes: container.querySelectorAll('.mermaid') });
        }
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
        this.imageContainerEl.classList.add('hidden');

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
            case 'image':
                this.imageContainerEl.classList.remove('hidden');
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
        this.imageEl.src = '';
        this.imageEl.onload = null;
        this.imageEl.onerror = null;
        this.resetImageZoom();
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
