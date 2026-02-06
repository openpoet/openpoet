// File Browser and Transfer
class FileBrowser {
    constructor() {
        this.panel = document.getElementById('file-browser-panel');
        this.fileList = document.getElementById('file-list');
        this.currentPath = '';

        this.setupEventListeners();
    }

    setupEventListeners() {
        // Toggle button
        document.getElementById('btn-file-browser')?.addEventListener('click', () => {
            this.toggle();
        });

        // Close button
        document.getElementById('btn-close-files')?.addEventListener('click', () => {
            this.hide();
        });

        // Upload button
        document.getElementById('btn-upload-file')?.addEventListener('click', () => {
            document.getElementById('file-upload-input')?.click();
        });

        // File input change
        document.getElementById('file-upload-input')?.addEventListener('change', (e) => {
            this.uploadFiles(e.target.files);
        });

        // Drag and drop on terminal
        const terminal = document.getElementById('terminal-container');
        if (terminal) {
            terminal.addEventListener('dragover', (e) => {
                e.preventDefault();
                e.dataTransfer.dropEffect = 'copy';
            });

            terminal.addEventListener('drop', (e) => {
                e.preventDefault();
                if (e.dataTransfer.files.length > 0) {
                    this.uploadFiles(e.dataTransfer.files);
                }
            });
        }

        // Clipboard paste for images
        document.addEventListener('paste', (e) => {
            const items = e.clipboardData?.items;
            if (!items) return;

            for (const item of items) {
                if (item.type.indexOf('image') !== -1) {
                    const blob = item.getAsFile();
                    if (blob) {
                        this.uploadPastedImage(blob);
                        e.preventDefault();
                        break;
                    }
                }
            }
        });
    }

    toggle() {
        if (this.panel?.classList.contains('hidden')) {
            this.show();
        } else {
            this.hide();
        }
    }

    show() {
        this.panel?.classList.remove('hidden');
        this.loadFiles();
    }

    hide() {
        this.panel?.classList.add('hidden');
    }

    async loadFiles(path = '') {
        const sessionId = app.currentSession;
        if (!sessionId) return;

        this.currentPath = path;

        try {
            const response = await fetch(`/api/sessions/${sessionId}/files?path=${encodeURIComponent(path)}`);
            if (!response.ok) throw new Error('Failed to load files');

            const files = await response.json();
            this.renderFiles(files);
        } catch (error) {
            console.error('Error loading files:', error);
            this.fileList.innerHTML = '<div class="text-muted" style="padding: 16px;">Failed to load files</div>';
        }
    }

    renderFiles(files) {
        if (!this.fileList) return;

        let html = '';

        // Parent directory link
        if (this.currentPath) {
            const parentPath = this.currentPath.split('/').slice(0, -1).join('/');
            html += `
                <div class="file-item directory" onclick="fileBrowser.loadFiles('${parentPath}')">
                    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                        <polyline points="15 18 9 12 15 6"></polyline>
                    </svg>
                    <span class="file-item-name">..</span>
                </div>
            `;
        }

        for (const file of files) {
            if (file.is_dir) {
                html += `
                    <div class="file-item directory" onclick="fileBrowser.loadFiles('${file.path}')">
                        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                            <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"></path>
                        </svg>
                        <span class="file-item-name">${this.escapeHtml(file.name)}</span>
                    </div>
                `;
            } else {
                html += `
                    <div class="file-item" onclick="fileBrowser.downloadFile('${file.path}')">
                        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                            <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"></path>
                            <polyline points="14 2 14 8 20 8"></polyline>
                        </svg>
                        <span class="file-item-name">${this.escapeHtml(file.name)}</span>
                        <span class="file-item-size">${this.formatSize(file.size)}</span>
                    </div>
                `;
            }
        }

        if (files.length === 0 && !this.currentPath) {
            html = '<div class="text-muted" style="padding: 16px;">No files</div>';
        }

        this.fileList.innerHTML = html;
    }

    downloadFile(path) {
        const sessionId = app.currentSession;
        if (!sessionId) return;

        const url = `/api/sessions/${sessionId}/files/${path}`;
        window.open(url, '_blank');
    }

    async uploadFiles(files) {
        const sessionId = app.currentSession;
        if (!sessionId || !files || files.length === 0) return;

        const formData = new FormData();
        for (const file of files) {
            formData.append('files', file);
        }

        try {
            app.showToast('Info', 'Uploading files...', 'info');

            const response = await fetch(`/api/sessions/${sessionId}/files?dir=${encodeURIComponent(this.currentPath)}`, {
                method: 'POST',
                body: formData
            });

            if (!response.ok) {
                const error = await response.json();
                throw new Error(error.error || 'Upload failed');
            }

            const result = await response.json();
            app.showToast('Success', `Uploaded ${result.uploaded.length} file(s)`, 'success');
            this.loadFiles(this.currentPath);

        } catch (error) {
            console.error('Upload error:', error);
            app.showToast('Error', error.message, 'error');
        }

        // Reset file input
        const input = document.getElementById('file-upload-input');
        if (input) input.value = '';
    }

    async uploadPastedImage(blob) {
        const sessionId = app.currentSession;
        if (!sessionId) return;

        // Convert blob to base64
        const reader = new FileReader();
        reader.onload = async () => {
            const base64 = reader.result;

            try {
                app.showToast('Info', 'Uploading image...', 'info');

                const response = await fetch(`/api/sessions/${sessionId}/files/paste`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({
                        data: base64,
                        dir: this.currentPath
                    })
                });

                if (!response.ok) {
                    const error = await response.json();
                    throw new Error(error.error || 'Upload failed');
                }

                const result = await response.json();
                app.showToast('Success', `Image saved: ${result.path}`, 'success');

                // Optionally insert path into terminal
                if (window.terminalManager) {
                    window.terminalManager.sendInput(result.path + ' ');
                }

                this.loadFiles(this.currentPath);

            } catch (error) {
                console.error('Paste upload error:', error);
                app.showToast('Error', error.message, 'error');
            }
        };
        reader.readAsDataURL(blob);
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

// Initialize file browser
window.fileBrowser = new FileBrowser();
