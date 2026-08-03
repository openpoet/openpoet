// File Browser and Transfer
class FileBrowser {
    constructor() {
        this.panel = document.getElementById('file-browser-panel');
        this.fileList = document.getElementById('file-list');
        this.currentPath = '';
        // Multi-image state
        this.pendingImages = []; // { id, blob, dataUrl }
        this.imageIdCounter = 0;
        this.isUploading = false;
        this.MAX_IMAGES = 10;

        this.setupEventListeners();
    }

    setupEventListeners() {
        // Toggle button (desktop)
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

        // Clipboard paste for images - use capture phase to intercept before terminal
        document.addEventListener('paste', (e) => {
            // Check if we're in terminal view OR image paste modal is open
            const dialog = document.getElementById('image-paste-dialog');
            const modalOpen = dialog && !dialog.classList.contains('hidden');
            const terminalView = document.getElementById('view-terminal');
            const inTerminalView = terminalView && terminalView.classList.contains('active');

            if (!modalOpen && !inTerminalView) {
                return;
            }

            // Don't intercept inputs/textareas that are outside the terminal view
            // (e.g. AI assistant, project name, search fields)
            const activeEl = document.activeElement;
            if (activeEl && (activeEl.tagName === 'INPUT' || activeEl.tagName === 'TEXTAREA')) {
                const insideTerminal = activeEl.closest('#view-terminal') ||
                    activeEl.id === 'image-paste-prompt';
                if (!insideTerminal) {
                    return;
                }
            }

            const items = e.clipboardData?.items;
            if (!items) return;

            for (const item of items) {
                if (item.type.indexOf('image') !== -1) {
                    const blob = item.getAsFile();
                    if (blob) {
                        e.preventDefault();
                        e.stopPropagation();
                        this.showImagePasteModal(blob);
                        break;
                    }
                }
            }
        }, true); // capture phase

        // Image picker buttons (desktop + mobile) - open file chooser
        const imagePickerInput = document.getElementById('image-picker-input');
        document.getElementById('btn-image-picker')?.addEventListener('click', () => {
            imagePickerInput?.click();
        });
        document.getElementById('btn-mobile-image-picker')?.addEventListener('click', () => {
            imagePickerInput?.click();
        });

        // Handle file selection from image picker (supports multiple)
        imagePickerInput?.addEventListener('change', (e) => {
            const files = e.target.files;
            if (!files || files.length === 0) return;
            for (const file of files) {
                if (file.type.startsWith('image/')) {
                    this.showImagePasteModal(file);
                }
            }
            // Reset so the same file can be selected again
            e.target.value = '';
        });

        // Voice button in the image paste modal
        document.getElementById('image-paste-voice-btn')?.addEventListener('click', () => {
            if (!window.voiceInput) return;
            const voiceBtn = document.getElementById('image-paste-voice-btn');
            if (window.voiceInput.isRecording) {
                window.voiceInput.stopRecording();
                voiceBtn?.classList.remove('recording');
                return;
            }
            voiceBtn?.classList.add('recording');
            window.voiceInput.startRecordingWithCallback((text) => {
                voiceBtn?.classList.remove('recording');
                const prompt = document.getElementById('image-paste-prompt');
                if (prompt) {
                    // Append transcribed text (add space if existing text)
                    const current = prompt.value;
                    prompt.value = current ? current + ' ' + text : text;
                    prompt.focus();
                }
            });
        });

        // Image paste modal buttons
        document.getElementById('image-paste-send')?.addEventListener('click', () => {
            this.sendImageWithPrompt();
        });

        document.getElementById('image-paste-cancel')?.addEventListener('click', () => {
            this.hideImagePasteModal();
        });

        document.getElementById('image-paste-close')?.addEventListener('click', () => {
            this.hideImagePasteModal();
        });

        // Add Image button in modal
        document.getElementById('image-paste-add-btn')?.addEventListener('click', () => {
            document.getElementById('image-picker-input')?.click();
        });

        // Keyboard shortcuts in the modal
        document.getElementById('image-paste-prompt')?.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault();
                this.sendImageWithPrompt();
            } else if (e.key === 'Escape') {
                e.preventDefault();
                this.hideImagePasteModal();
            }
        });

        // Escape key on overlay
        document.getElementById('image-paste-dialog')?.addEventListener('keydown', (e) => {
            if (e.key === 'Escape') {
                e.preventDefault();
                this.hideImagePasteModal();
            }
        });
    }

    showImagePasteModal(blob) {
        if (this.isUploading) return;
        if (this.pendingImages.length >= this.MAX_IMAGES) {
            app.showToast('Warning', `Maximum ${this.MAX_IMAGES} images allowed`, 'warning');
            return;
        }

        const id = ++this.imageIdCounter;
        const reader = new FileReader();
        reader.onload = () => {
            this.pendingImages.push({ id, blob, dataUrl: reader.result });
            this.renderImageGrid();

            const dialog = document.getElementById('image-paste-dialog');
            if (dialog && dialog.classList.contains('hidden')) {
                dialog.classList.remove('hidden');
                // Clear and focus the prompt textarea only on first open
                const prompt = document.getElementById('image-paste-prompt');
                if (prompt) {
                    prompt.value = '';
                    setTimeout(() => prompt.focus(), 100);
                }
            }
        };
        reader.readAsDataURL(blob);
    }

    renderImageGrid() {
        const grid = document.getElementById('image-paste-grid');
        if (!grid) return;

        grid.innerHTML = this.pendingImages.map(img => `
            <div class="image-paste-thumb" data-id="${img.id}">
                <img src="${img.dataUrl}" alt="Image ${img.id}" />
                <button class="image-paste-thumb-remove" onclick="fileBrowser.removeImage(${img.id})" title="Remove">&times;</button>
            </div>
        `).join('');

        // Update counter
        const count = document.getElementById('image-paste-count');
        if (count) {
            count.textContent = `${this.pendingImages.length}/${this.MAX_IMAGES} images`;
        }
    }

    removeImage(id) {
        if (this.isUploading) return;
        this.pendingImages = this.pendingImages.filter(img => img.id !== id);
        if (this.pendingImages.length === 0) {
            this.hideImagePasteModal();
        } else {
            this.renderImageGrid();
        }
    }

    hideImagePasteModal() {
        const dialog = document.getElementById('image-paste-dialog');
        if (dialog) {
            dialog.classList.add('hidden');
        }

        this.pendingImages = [];
        this.isUploading = false;

        const grid = document.getElementById('image-paste-grid');
        if (grid) grid.innerHTML = '';

        const count = document.getElementById('image-paste-count');
        if (count) count.textContent = '';

        const progress = document.getElementById('image-paste-progress');
        if (progress) progress.classList.add('hidden');

        // Re-enable buttons
        this.setModalButtonsEnabled(true);

        // Cancel any ongoing voice recording in the modal
        if (window.voiceInput && window.voiceInput.isRecording && window.voiceInput.targetCallback) {
            window.voiceInput.cancelRecording();
        }
        document.getElementById('image-paste-voice-btn')?.classList.remove('recording');

        // Refocus terminal
        if (window.terminalManager) {
            window.terminalManager.focus();
        }
    }

    setModalButtonsEnabled(enabled) {
        const sendBtn = document.getElementById('image-paste-send');
        const cancelBtn = document.getElementById('image-paste-cancel');
        const addBtn = document.getElementById('image-paste-add-btn');
        if (sendBtn) sendBtn.disabled = !enabled;
        if (cancelBtn) cancelBtn.disabled = !enabled;
        if (addBtn) addBtn.disabled = !enabled;
    }

    // Compress image using canvas to reduce size for upload
    compressImage(dataUrl, maxWidth = 1920, quality = 0.8) {
        return new Promise((resolve) => {
            const img = new Image();
            img.onload = () => {
                let w = img.width;
                let h = img.height;

                // Scale down if wider than maxWidth
                if (w > maxWidth) {
                    h = Math.round(h * maxWidth / w);
                    w = maxWidth;
                }

                const canvas = document.createElement('canvas');
                canvas.width = w;
                canvas.height = h;
                const ctx = canvas.getContext('2d');
                ctx.drawImage(img, 0, 0, w, h);

                resolve(canvas.toDataURL('image/jpeg', quality));
            };
            img.onerror = () => {
                // If compression fails, return original
                resolve(dataUrl);
            };
            img.src = dataUrl;
        });
    }

    async sendImageWithPrompt() {
        if (this.pendingImages.length === 0 || this.isUploading) return;

        const sessionId = app.currentSession;
        if (!sessionId) {
            app.showToast('Error', 'No active session', 'error');
            this.hideImagePasteModal();
            return;
        }

        const promptEl = document.getElementById('image-paste-prompt');
        const userPrompt = promptEl ? promptEl.value.trim() : '';

        // Lock UI during upload
        this.isUploading = true;
        this.setModalButtonsEnabled(false);

        const progress = document.getElementById('image-paste-progress');
        const progressText = document.getElementById('image-paste-progress-text');
        if (progress) progress.classList.remove('hidden');

        const totalImages = this.pendingImages.length;
        const uploadedPaths = [];
        let message = null;

        try {
            // Upload images sequentially
            for (let i = 0; i < totalImages; i++) {
                if (progressText) {
                    progressText.textContent = `Uploading ${i + 1}/${totalImages}...`;
                }

                const img = this.pendingImages[i];
                let imageData = img.dataUrl;

                // Compress if > 500KB
                if (imageData.length > 500000) {
                    imageData = await this.compressImage(imageData);
                    console.log(`Image ${i+1} compressed: ${Math.round(img.dataUrl.length/1024)}KB -> ${Math.round(imageData.length/1024)}KB`);
                }

                const response = await fetch(`/api/sessions/${sessionId}/files/paste`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({
                        data: imageData,
                        dir: '.openpoet/images'
                    })
                });

                if (!response.ok) {
                    const ct = response.headers.get('content-type') || '';
                    if (ct.indexOf('application/json') !== -1) {
                        const error = await response.json();
                        throw new Error(error.error || `Upload failed for image ${i+1}`);
                    } else {
                        throw new Error(`Upload failed (HTTP ${response.status}). Image ${i+1} may be too large.`);
                    }
                }

                const result = await response.json();
                uploadedPaths.push(result.path);
            }

            app.showToast('Success', `${uploadedPaths.length} image(s) uploaded`, 'success');

            // Build the formatted message for Claude Code
            if (uploadedPaths.length === 1) {
                if (userPrompt) {
                    message = `Read the image file at ${uploadedPaths[0]} and then: ${userPrompt}`;
                } else {
                    message = `Read and analyze the image file at ${uploadedPaths[0]}`;
                }
            } else {
                const pathList = uploadedPaths.map(p => `  - ${p}`).join('\n');
                if (userPrompt) {
                    message = `Read the following ${uploadedPaths.length} image files:\n${pathList}\nThen: ${userPrompt}`;
                } else {
                    message = `Read and analyze the following ${uploadedPaths.length} image files:\n${pathList}`;
                }
            }

            // Send image prompt hint to backend for evaluation context
            // (fire-and-forget, don't block the terminal submission)
            fetch(`/api/sessions/${sessionId}/image-prompt-hint`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    user_prompt: userPrompt || '(image analysis requested)',
                    image_count: uploadedPaths.length
                })
            }).catch(err => console.warn('Image prompt hint failed:', err));

        } catch (error) {
            console.error('Image paste error:', error);
            app.showToast('Error', error.message, 'error');
        }

        // Hide modal and clean up first — closing the modal triggers browser
        // focus restore which sends \x1b[I (focus-in) to the PTY.
        const dialog = document.getElementById('image-paste-dialog');
        if (dialog) dialog.classList.add('hidden');

        this.pendingImages = [];
        this.isUploading = false;

        const grid = document.getElementById('image-paste-grid');
        if (grid) grid.innerHTML = '';
        const count = document.getElementById('image-paste-count');
        if (count) count.textContent = '';
        if (progress) progress.classList.add('hidden');
        this.setModalButtonsEnabled(true);

        // Send text AFTER modal close so the focus-in event settles first.
        if (message && window.terminalManager && sessionId) {
            setTimeout(() => {
                if (!userPrompt) {
                    // Auto-generated text must be "typed" char-by-char to avoid
                    // readline paste detection which turns \r into a newline.
                    window.terminalManager.typeAndSubmitLine(sessionId, message);
                } else {
                    window.terminalManager.submitTerminalLine(sessionId, message);
                }
            }, 200);
        }
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
                const escapedPath = file.path.replace(/'/g, "\\'");
                const escapedName = file.name.replace(/'/g, "\\'");
                html += `
                    <div class="file-item" onclick="fileBrowser.openFile('${escapedPath}', '${escapedName}')">
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

    openFile(path, name) {
        if (window.fileViewer && FileViewer.isViewable(name)) {
            window.fileViewer.show(path);
        } else {
            this.downloadFile(path);
        }
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

    // Escape for BOTH text and attribute contexts: the textContent -> innerHTML
    // trick this used to do does NOT escape the double quote, so interpolating
    // into an attribute truncated the value at the first `"` it contained.
    escapeHtml(text) {
        if (text === null || text === undefined) return '';
        return String(text)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }
}

// Initialize file browser
window.fileBrowser = new FileBrowser();
