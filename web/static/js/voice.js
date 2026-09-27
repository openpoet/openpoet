// Voice Input using MediaRecorder and OpenAI Whisper
//
// A recording must never be lost. The public entrypoint sits behind a proxy
// that rejects request bodies over 1 MiB, so recordings are:
//   1. encoded at a speech bitrate (Opus ~32 kbps ≈ 240 KB/min);
//   2. persisted to IndexedDB every second while recording, so a reload,
//      crash or failed upload can always be resumed (see VoiceStore);
//   3. uploaded in small idempotent byte slices that the server reassembles
//      into the original file before transcribing (/api/voice/uploads/…).

// IndexedDB persistence for in-progress and unsent recordings. Every method
// swallows storage errors (private mode, quota): persistence is a safety net,
// recording and uploading keep working in memory without it.
const VoiceStore = {
    dbPromise: null,

    open() {
        if (this.dbPromise) return this.dbPromise;
        this.dbPromise = new Promise((resolve, reject) => {
            if (!window.indexedDB) { reject(new Error('IndexedDB unavailable')); return; }
            const request = indexedDB.open('openpoet-voice', 1);
            request.onupgradeneeded = () => {
                const db = request.result;
                if (!db.objectStoreNames.contains('recordings')) db.createObjectStore('recordings', { keyPath: 'id' });
                if (!db.objectStoreNames.contains('chunks')) db.createObjectStore('chunks', { keyPath: ['id', 'seq'] });
            };
            request.onsuccess = () => resolve(request.result);
            request.onerror = () => reject(request.error);
        });
        this.dbPromise.catch(() => {});
        return this.dbPromise;
    },

    async tx(stores, mode, fn) {
        const db = await this.open();
        return new Promise((resolve, reject) => {
            const tx = db.transaction(stores, mode);
            let result;
            Promise.resolve(fn(tx)).then(value => { result = value; });
            tx.oncomplete = () => resolve(result);
            tx.onerror = () => reject(tx.error);
            tx.onabort = () => reject(tx.error);
        });
    },

    async putRecording(record) {
        try {
            await this.tx(['recordings'], 'readwrite', tx => { tx.objectStore('recordings').put(record); });
        } catch (err) { console.warn('[voice] could not persist recording metadata:', err); }
    },

    async updateRecording(id, fields) {
        try {
            await this.tx(['recordings'], 'readwrite', tx => {
                const store = tx.objectStore('recordings');
                const get = store.get(id);
                get.onsuccess = () => {
                    if (get.result) store.put({ ...get.result, ...fields, updatedAt: Date.now() });
                };
            });
        } catch (err) { console.warn('[voice] could not update recording metadata:', err); }
    },

    async appendChunk(id, seq, blob) {
        try {
            await this.tx(['chunks', 'recordings'], 'readwrite', tx => {
                tx.objectStore('chunks').put({ id, seq, blob });
                const store = tx.objectStore('recordings');
                const get = store.get(id);
                get.onsuccess = () => {
                    if (get.result) store.put({ ...get.result, updatedAt: Date.now() });
                };
            });
        } catch (err) { console.warn('[voice] could not persist audio chunk:', err); }
    },

    async listRecordings() {
        try {
            return await this.tx(['recordings'], 'readonly', tx => new Promise(resolve => {
                const req = tx.objectStore('recordings').getAll();
                req.onsuccess = () => resolve(req.result || []);
                req.onerror = () => resolve([]);
            }));
        } catch (_) { return []; }
    },

    async loadBlob(record) {
        try {
            const range = IDBKeyRange.bound([record.id, 0], [record.id, Number.MAX_SAFE_INTEGER]);
            const chunks = await this.tx(['chunks'], 'readonly', tx => new Promise(resolve => {
                const req = tx.objectStore('chunks').getAll(range);
                req.onsuccess = () => resolve(req.result || []);
                req.onerror = () => resolve([]);
            }));
            if (!chunks.length) return null;
            chunks.sort((a, b) => a.seq - b.seq);
            return new Blob(chunks.map(c => c.blob), { type: record.mimeType || 'audio/webm' });
        } catch (_) { return null; }
    },

    async remove(id) {
        if (!id) return;
        try {
            await this.tx(['chunks', 'recordings'], 'readwrite', tx => {
                tx.objectStore('chunks').delete(IDBKeyRange.bound([id, 0], [id, Number.MAX_SAFE_INTEGER]));
                tx.objectStore('recordings').delete(id);
            });
        } catch (err) { console.warn('[voice] could not remove persisted recording:', err); }
    }
};

class VoiceInput {
    constructor() {
        this.mediaRecorder = null;
        this.audioChunks = [];
        this.isRecording = false;
        this.submitAfterTranscribe = false;
        this.targetCallback = null; // When set, transcribed text goes here instead of terminal

        this.indicator = document.getElementById('voice-indicator');
        this.statusLabel = document.getElementById('voice-status-label');
        this.retryBtn = document.getElementById('voice-retry');
        this.discardBtn = document.getElementById('voice-discard');
        this.startBtn = document.getElementById('btn-voice-input');
        this.mobileBtn = document.getElementById('btn-mobile-voice-input');
        this.stopBtn = document.getElementById('voice-stop-btn');
        this.sendBtn = document.getElementById('voice-send');
        this.cancelBtn = document.getElementById('voice-cancel');
        this.timerDisplay = document.getElementById('voice-timer');
        this.pulseEl = this.indicator?.querySelector('.voice-indicator-pulse');
        this.cancelled = false;
        this.recordingTimer = null;
        this.timeoutTimer = null;
        this.recordingSeconds = 0;
        this.recordingId = null;
        this.recordingMimeType = null;
        this.chunkSeq = 0;
        // Held between attempts so retries don't lose the recording.
        this.pendingAudioBlob = null;
        this.pendingRecordingId = null; // doubles as the server upload id
        this.pendingMimeType = null;
        this.pendingTargetCallback = null; // destination bound to the in-flight upload
        this.pendingSubmit = false;
        this.uploadedSlices = new Set(); // slices of the pending upload the server already has
        this.uploading = false;
        this.uploadAttemptCount = 0;

        this.maxRecordingSeconds = 20 * 60;
        this.audioBitsPerSecond = 32000;
        this.sliceBytes = 256 * 1024; // ~350 KB as base64 JSON, far below the proxy's 1 MiB

        this.setupEventListeners();
        this.recoverPendingRecordings();
    }

    setupEventListeners() {
        this.startBtn?.addEventListener('click', () => this.toggleRecording());
        this.mobileBtn?.addEventListener('click', () => this.toggleRecording());
        this.stopBtn?.addEventListener('click', () => this.stopRecording(false));
        this.sendBtn?.addEventListener('click', () => this.stopRecording(true));
        this.cancelBtn?.addEventListener('click', () => this.cancelRecording());
        this.retryBtn?.addEventListener('click', () => this.retryUpload());
        this.discardBtn?.addEventListener('click', () => this.discardPendingAudio());
    }

    toggleRecording() {
        if (!this.isSupported()) {
            const isHTTP = window.location.protocol === 'http:' && window.location.hostname !== 'localhost';
            if (isHTTP) {
                app.showToast('Error', 'Microphone requires HTTPS. Access via https:// or localhost.', 'error');
            } else {
                app.showToast('Error', 'Microphone not supported in this browser.', 'error');
            }
            return;
        }
        if (this.isRecording) {
            this.stopRecording(false);
        } else {
            this.startRecording();
        }
    }

    newRecordingId() {
        if (window.crypto?.randomUUID) return crypto.randomUUID();
        return `rec-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`;
    }

    createMediaRecorder(stream) {
        const mimeType = this.getSupportedMimeType();
        try {
            return new MediaRecorder(stream, { mimeType, audioBitsPerSecond: this.audioBitsPerSecond });
        } catch (err) {
            console.warn('[voice] MediaRecorder rejected options, using defaults:', err);
            return new MediaRecorder(stream);
        }
    }

    async startRecording() {
        try {
            const stream = await navigator.mediaDevices.getUserMedia({ audio: true });

            this.audioChunks = [];
            this.chunkSeq = 0;
            this.mediaRecorder = this.createMediaRecorder(stream);
            const recordingId = this.newRecordingId();
            this.recordingId = recordingId;
            this.recordingMimeType = this.mediaRecorder.mimeType || this.getSupportedMimeType();
            VoiceStore.putRecording({
                id: recordingId,
                mimeType: this.recordingMimeType,
                status: 'recording',
                createdAt: Date.now(),
                updatedAt: Date.now()
            });

            this.mediaRecorder.ondataavailable = (event) => {
                if (event.data.size > 0) {
                    this.audioChunks.push(event.data);
                    VoiceStore.appendChunk(recordingId, this.chunkSeq++, event.data);
                }
            };

            this.mediaRecorder.onstop = async () => {
                stream.getTracks().forEach(track => track.stop());
                if (this.cancelled) {
                    this.cancelled = false;
                    this.audioChunks = [];
                    VoiceStore.remove(recordingId);
                    return;
                }
                await this.processRecording(recordingId);
            };

            // A timeslice flushes audio every second so it reaches IndexedDB
            // while the user is still talking, not only at stop().
            this.mediaRecorder.start(1000);
            this.isRecording = true;
            this.showIndicator();
            this.startTimers();

        } catch (error) {
            console.error('Failed to start recording:', error);
            app.showToast('Error', 'Could not access microphone', 'error');
        }
    }

    stopRecording(autoSubmit = false) {
        if (this.mediaRecorder && this.isRecording) {
            this.submitAfterTranscribe = autoSubmit;
            this.clearTimers();
            this.mediaRecorder.stop();
            this.isRecording = false;
            // Transition straight into the uploading state so there's no flash
            // where the indicator disappears between stop and upload-start.
            this.showUploading();
            this.setStatusLabel('Preparing…');
        }
    }

    autoStopRecording() {
        if (!this.mediaRecorder || !this.isRecording) return;
        // Stop and transcribe (not cancel/discard)
        this.stopRecording(false);
        // Audible beep notification
        this.playStopBeep();
        // Vibrate on mobile if supported
        if (navigator.vibrate) {
            navigator.vibrate([200, 100, 200]);
        }
        // Visual toast
        const minutes = Math.round(this.maxRecordingSeconds / 60);
        app.showToast('Info', `Recording auto-stopped: ${minutes} min limit reached. Transcribing...`, 'info');
    }

    playStopBeep() {
        try {
            const ctx = new (window.AudioContext || window.webkitAudioContext)();
            // Two short beeps
            [0, 0.2].forEach(offset => {
                const osc = ctx.createOscillator();
                const gain = ctx.createGain();
                osc.connect(gain);
                gain.connect(ctx.destination);
                osc.frequency.value = 880;
                osc.type = 'sine';
                gain.gain.value = 0.3;
                osc.start(ctx.currentTime + offset);
                osc.stop(ctx.currentTime + offset + 0.15);
            });
            // Close context after beeps finish
            setTimeout(() => ctx.close(), 1000);
        } catch (e) {
            // Ignore audio errors (e.g. no audio context support)
        }
    }

    cancelRecording() {
        if (this.mediaRecorder && this.isRecording) {
            this.clearTimers();
            this.cancelled = true;
            this.targetCallback = null;
            this.mediaRecorder.stop();
            this.isRecording = false;
            this.hideIndicator();
            app.showToast('Info', 'Recording cancelled', 'info');
        }
    }

    startTimers() {
        this.recordingSeconds = 0;
        this.updateTimerDisplay();
        this.recordingTimer = setInterval(() => {
            this.recordingSeconds++;
            this.updateTimerDisplay();
        }, 1000);
        this.timeoutTimer = setTimeout(() => this.autoStopRecording(), this.maxRecordingSeconds * 1000);
    }

    clearTimers() {
        if (this.recordingTimer) {
            clearInterval(this.recordingTimer);
            this.recordingTimer = null;
        }
        if (this.timeoutTimer) {
            clearTimeout(this.timeoutTimer);
            this.timeoutTimer = null;
        }
        this.recordingSeconds = 0;
    }

    updateTimerDisplay() {
        if (!this.timerDisplay) return;
        const mins = Math.floor(this.recordingSeconds / 60);
        const secs = this.recordingSeconds % 60;
        this.timerDisplay.textContent = `${mins}:${secs.toString().padStart(2, '0')}`;
        this.timerDisplay.classList.toggle('warning', this.recordingSeconds >= this.maxRecordingSeconds - 30);
    }

    async processRecording(recordingId) {
        if (this.audioChunks.length === 0) {
            VoiceStore.remove(recordingId);
            this.hideUploading();
            app.showToast('Error', 'No audio recorded', 'error');
            return;
        }

        // Keep the blob around so we can retry without losing the recording.
        this.pendingAudioBlob = new Blob(this.audioChunks, { type: this.recordingMimeType || this.getSupportedMimeType() });
        this.pendingRecordingId = recordingId;
        this.pendingMimeType = this.pendingAudioBlob.type;
        this.pendingSubmit = this.submitAfterTranscribe;
        this.uploadedSlices = new Set();
        this.uploadAttemptCount = 0;
        // Bind the destination to THIS recording. targetCallback is read at
        // delivery time, seconds later; a recording started meanwhile would
        // otherwise steal this transcript — or, if none is registered by then,
        // it would fall through to insertText() and type into the terminal.
        this.pendingTargetCallback = this.targetCallback;
        VoiceStore.updateRecording(recordingId, { status: 'stopped', submit: this.pendingSubmit });

        await this.uploadWithRetry();
    }

    // Offer any recording persisted by an earlier page load (or an earlier
    // failed upload) that never produced a transcript.
    async recoverPendingRecordings() {
        if (this.isRecording || this.pendingAudioBlob || this.uploading) return;
        const now = Date.now();
        const records = (await VoiceStore.listRecordings())
            .filter(r => r.id !== this.recordingId)
            // Skip recordings another tab is actively writing or uploading.
            .filter(r => r.status === 'stopped' || now - (r.updatedAt || 0) > 15000)
            .filter(r => r.status !== 'uploading' || now - (r.updatedAt || 0) > 180000)
            .sort((a, b) => (a.createdAt || 0) - (b.createdAt || 0));
        for (const record of records) {
            if (this.isRecording || this.pendingAudioBlob || this.uploading) return;
            const blob = await VoiceStore.loadBlob(record);
            if (!blob || blob.size === 0) {
                VoiceStore.remove(record.id);
                continue;
            }
            this.pendingAudioBlob = blob;
            this.pendingRecordingId = record.id;
            this.pendingMimeType = record.mimeType || blob.type;
            this.pendingSubmit = false;
            this.pendingTargetCallback = null;
            this.uploadedSlices = new Set();
            const when = new Date(record.createdAt || now).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
            const err = new Error(`Unsent recording from ${when} (${Math.round(blob.size / 1024)} KB) recovered`);
            this.showRetryAvailable(err);
            return;
        }
    }

    async uploadWithRetry() {
        if (!this.pendingAudioBlob || this.uploading) return;

        this.uploading = true;
        this.showUploading();
        const recordingId = this.pendingRecordingId;
        VoiceStore.updateRecording(recordingId, { status: 'uploading' });

        try {
            const result = await this.transcribePending();
            // Success: clear pending state, deliver text.
            this.hideUploading();
            this.pendingAudioBlob = null;
            this.pendingRecordingId = null;
            this.uploadedSlices = new Set();
            await VoiceStore.remove(recordingId);
            const deliver = this.pendingTargetCallback;
            this.pendingTargetCallback = null;
            if (this.targetCallback === deliver) this.targetCallback = null;
            if (result && result.text) {
                if (deliver) {
                    deliver(result.text);
                } else {
                    this.insertText(result.text, this.pendingSubmit);
                }
            }
        } catch (err) {
            // All retries exhausted: keep the blob (in memory and IndexedDB)
            // and offer manual retry.
            VoiceStore.updateRecording(recordingId, { status: 'stopped' });
            this.showRetryAvailable(err);
        } finally {
            this.uploading = false;
        }
        if (!this.pendingAudioBlob) this.recoverPendingRecordings();
    }

    recordingFilename(mimeType) {
        const type = String(mimeType || '').toLowerCase();
        if (type.includes('mp4') || type.includes('aac') || type.includes('m4a')) return 'recording.mp4';
        if (type.includes('ogg')) return 'recording.ogg';
        if (type.includes('wav')) return 'recording.wav';
        if (type.includes('mpeg') || type.includes('mp3')) return 'recording.mp3';
        return 'recording.webm';
    }

    // Upload the pending recording as byte slices, then ask the server to
    // reassemble and transcribe it. Slices the server already holds are not
    // re-sent; slices it reports missing (e.g. lost across a restart) are.
    async transcribePending() {
        const blob = this.pendingAudioBlob;
        const uploadId = this.pendingRecordingId;
        const totalSlices = Math.max(1, Math.ceil(blob.size / this.sliceBytes));
        const filename = this.recordingFilename(this.pendingMimeType || blob.type);
        const base = `/api/voice/uploads/${encodeURIComponent(uploadId)}`;

        for (let round = 0; round < 3; round++) {
            for (let i = 0; i < totalSlices; i++) {
                if (this.uploadedSlices.has(i)) continue;
                const slice = blob.slice(i * this.sliceBytes, Math.min(blob.size, (i + 1) * this.sliceBytes));
                const data = await this.blobToBase64(slice);
                await this.requestWithRetry(
                    () => this.postJSON(`${base}/chunks/${i}`, { data }, 90000),
                    totalSlices > 1 ? `Sending ${i + 1}/${totalSlices}` : `Sending ${Math.round(blob.size / 1024)} KB`
                );
                this.uploadedSlices.add(i);
            }

            try {
                return await this.requestWithRetry(
                    () => this.postJSON(`${base}/complete`, { chunks: totalSlices, filename }, 180000),
                    'Transcribing'
                );
            } catch (err) {
                if (err.status === 409 && Array.isArray(err.body?.missing)) {
                    err.body.missing.forEach(i => this.uploadedSlices.delete(i));
                    continue;
                }
                throw err;
            }
        }
        throw new Error('Upload could not be completed');
    }

    async requestWithRetry(send, label) {
        // Backoff: immediate, 1.5s, 4s, 8s — ~13.5s before handing the
        // decision to the user (the recording is kept either way).
        const backoffsMs = [0, 1500, 4000, 8000];
        let lastError = null;
        for (let attempt = 0; attempt < backoffsMs.length; attempt++) {
            this.uploadAttemptCount++;
            if (backoffsMs[attempt] > 0) {
                this.setStatusLabel(`${label} — retrying (${attempt + 1}/${backoffsMs.length})…`);
                await new Promise(resolve => setTimeout(resolve, backoffsMs[attempt]));
            } else {
                this.setStatusLabel(`${label}…`);
            }
            try {
                return await send();
            } catch (err) {
                lastError = err;
                console.warn(`[voice] ${label} attempt ${attempt + 1} failed:`, err);
                if (!this.isRetryableError(err)) break;
            }
        }
        throw lastError || new Error('Upload failed');
    }

    blobToBase64(blob) {
        return new Promise((resolve, reject) => {
            const reader = new FileReader();
            reader.onload = () => {
                const url = String(reader.result || '');
                resolve(url.slice(url.indexOf(',') + 1));
            };
            reader.onerror = () => reject(reader.error || new Error('Could not read recording'));
            reader.readAsDataURL(blob);
        });
    }

    // JSON is used for every request: binary bodies get corrupted by the
    // relay tunnel's JSON serialization, base64 survives it.
    async postJSON(url, payload, timeoutMs) {
        const controller = new AbortController();
        const abortTimer = setTimeout(() => controller.abort(), timeoutMs);
        let response;
        try {
            response = await fetch(url, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(payload),
                signal: controller.signal
            });
        } finally {
            clearTimeout(abortTimer);
        }

        if (!response.ok) {
            const { message, body } = await this.parseErrorBody(response);
            const err = new Error(message);
            err.status = response.status;
            err.body = body;
            throw err;
        }

        // Defensive: server should always return JSON on 200, but guard anyway.
        try {
            return await response.json();
        } catch (parseErr) {
            const err = new Error('Server returned an invalid response');
            err.status = response.status;
            err.parseError = true;
            throw err;
        }
    }

    async parseErrorBody(response) {
        let bodyText = '';
        try {
            bodyText = await response.text();
        } catch (_) {
            // ignore
        }
        if (bodyText) {
            try {
                const json = JSON.parse(bodyText);
                if (json && json.error) return { message: json.error, body: json };
            } catch (_) {
                // Not JSON (likely HTML 502/504 from a tunnel) — fall through.
            }
        }
        const status = response.status;
        let message = `Request failed (${status})`;
        if (status === 0) message = 'Network error';
        else if (status === 413) message = 'Recording too large for the server';
        else if (status === 429) message = 'Rate limited — please retry';
        else if (status >= 500) message = `Server error (${status})`;
        return { message, body: null };
    }

    isRetryableError(err) {
        // AbortError, network failure (TypeError from fetch), 5xx, 429.
        if (!err) return false;
        if (err.nonRetryable) return false;
        if (err.name === 'AbortError') return true;
        if (err.parseError) return true;
        if (err.status === 429) return true;
        if (typeof err.status === 'number' && err.status >= 500) return true;
        // Fetch network failures throw TypeError with no status set.
        if (err.status === undefined) return true;
        return false;
    }

    retryUpload() {
        if (!this.pendingAudioBlob) return;
        this.uploadWithRetry();
    }

    discardPendingAudio() {
        const recordingId = this.pendingRecordingId;
        this.pendingAudioBlob = null;
        this.pendingRecordingId = null;
        this.pendingSubmit = false;
        this.targetCallback = null;
        this.pendingTargetCallback = null;
        this.uploadedSlices = new Set();
        this.hideUploading();
        if (recordingId) {
            VoiceStore.remove(recordingId).then(() => this.recoverPendingRecordings());
            fetch(`/api/voice/uploads/${encodeURIComponent(recordingId)}`, { method: 'DELETE' }).catch(() => {});
        }
    }

    // Insert transcribed text into the appropriate input (mobile input bar or terminal)
    insertText(text, submit) {
        const isMobile = window.innerWidth <= 768;
        const mobileInput = document.getElementById('mobile-terminal-input');
        const tm = window.terminalManager;
        const sessionId = tm?.activeSessionId;
        if (sessionId && tm?.isCodexAppServerSession?.(sessionId)) {
            const svView = window.structuredView?.views?.get(sessionId);
            const codexInput = isMobile ? mobileInput : svView?.textarea;
            if (codexInput) {
                codexInput.value = codexInput.value ? `${codexInput.value} ${text}` : text;
                codexInput.dispatchEvent(new Event('input', { bubbles: true }));
                if (submit) {
                    if (isMobile) {
                        tm.submitTerminalLine?.(sessionId, codexInput.value);
                        codexInput.value = '';
                    } else {
                        svView?.sendToTerminal?.();
                    }
                } else {
                    codexInput.focus();
                }
                return;
            }
        }

        if (isMobile && mobileInput) {
            // Mobile: use the mobile input bar
            const current = mobileInput.value;
            if (current.length > 0) {
                mobileInput.value = current + ' ' + text;
            } else {
                mobileInput.value = text;
            }

            if (submit) {
                // Send text directly to terminal (bypassing real-time sync
                // which doesn't fire for programmatic value changes)
                if (sessionId) {
                    const fullText = mobileInput.value;
                    window.terminalManager.submitTerminalLine(sessionId, fullText);
                }
                mobileInput.value = '';
                mobileInput._lastSyncedValue = '';
                mobileInput.style.height = '44px';
                mobileInput.style.overflow = 'hidden';
            } else {
                // Open full-screen editor with the transcribed text
                if (window.app && window.app.openMobileEditor) {
                    window.app.openMobileEditor();
                } else {
                    mobileInput.focus();
                }
            }
        } else if (window.terminalManager) {
            // Desktop: send directly to terminal
            // Capture target session ID NOW to prevent input going to a different
            // session if the active session changes during async delays.
            const targetSessionId = window.terminalManager.activeSessionId;
            if (!targetSessionId) return;

            if (submit) {
                const delays = window.app
                    ? window.app.getInputDelays(targetSessionId, 'voice')
                    : { textToEnter: 50 };
                window.terminalManager.sendInputToSession(targetSessionId, text);
                setTimeout(() => {
                    window.terminalManager.sendInputToSession(targetSessionId, '\r');
                }, delays.textToEnter);
            } else {
                // Move to end of line, add space if line has text, paste
                window.terminalManager.sendInputToSession(targetSessionId, '\x05'); // Ctrl+E
                const lineContent = window.terminalManager.getActiveLineContent();
                if (lineContent.trim().length > 0) {
                    window.terminalManager.sendInputToSession(targetSessionId, ' ');
                }
                window.terminalManager.sendInputToSession(targetSessionId, text);
            }
        }
    }

    getSupportedMimeType() {
        const types = [
            'audio/webm',
            'audio/webm;codecs=opus',
            'audio/ogg;codecs=opus',
            'audio/mp4'
        ];

        for (const type of types) {
            if (MediaRecorder.isTypeSupported(type)) {
                return type;
            }
        }

        return 'audio/webm';
    }

    showIndicator() {
        this.indicator?.classList.remove('hidden');
        this.indicator?.classList.remove('uploading', 'retry-available');
        this.setStatusLabel('Recording…');
        this.toggleRecordingButtons(true);
        this.startBtn?.classList.add('recording');
        this.mobileBtn?.classList.add('recording');
        // Hide "Send" button when in callback mode (image paste modal)
        this.sendBtn?.classList.toggle('hidden', !!this.targetCallback);
    }

    hideIndicator() {
        // Don't hide if an upload/retry is in progress — only hide after upload
        // resolves or user discards. The recording chrome (cancel/stop/send)
        // still goes away though.
        this.toggleRecordingButtons(false);
        this.startBtn?.classList.remove('recording');
        this.mobileBtn?.classList.remove('recording');
        if (!this.pendingAudioBlob) {
            this.indicator?.classList.add('hidden');
            this.indicator?.classList.remove('uploading', 'retry-available');
        }
    }

    showUploading() {
        this.indicator?.classList.remove('hidden', 'retry-available');
        this.indicator?.classList.add('uploading');
        this.toggleRecordingButtons(false);
        this.toggleRetryButtons(false);
        this.startBtn?.classList.remove('recording');
        this.mobileBtn?.classList.remove('recording');
        if (this.timerDisplay) this.timerDisplay.classList.add('hidden');
    }

    hideUploading() {
        this.indicator?.classList.remove('uploading', 'retry-available');
        this.indicator?.classList.add('hidden');
        this.toggleRetryButtons(false);
        if (this.timerDisplay) {
            this.timerDisplay.classList.remove('hidden');
            this.timerDisplay.classList.remove('warning');
        }
    }

    showRetryAvailable(err) {
        this.indicator?.classList.remove('hidden', 'uploading');
        if (this.timerDisplay) this.timerDisplay.classList.add('hidden');
        this.indicator?.classList.add('retry-available');
        const msg = err && err.message ? err.message : 'Upload failed';
        const canRetry = !err?.nonRetryable;
        this.setStatusLabel(canRetry ? `${msg} — tap Retry` : msg);
        this.toggleRecordingButtons(false);
        this.retryBtn?.classList.toggle('hidden', !canRetry);
        this.discardBtn?.classList.remove('hidden');
    }

    setStatusLabel(text) {
        if (this.statusLabel) this.statusLabel.textContent = text;
    }

    toggleRecordingButtons(show) {
        // Cancel/Stop/Send are recording-only controls.
        this.cancelBtn?.classList.toggle('hidden', !show);
        this.stopBtn?.classList.toggle('hidden', !show);
        this.sendBtn?.classList.toggle('hidden', !show || !!this.targetCallback);
    }

    toggleRetryButtons(show) {
        this.retryBtn?.classList.toggle('hidden', !show);
        this.discardBtn?.classList.toggle('hidden', !show);
    }

    // Start recording with a callback for the transcribed text (used by image paste modal)
    startRecordingWithCallback(callback) {
        if (!this.isSupported()) {
            const isHTTP = window.location.protocol === 'http:' && window.location.hostname !== 'localhost';
            if (isHTTP) {
                app.showToast('Error', 'Microphone requires HTTPS. Access via https:// or localhost.', 'error');
            } else {
                app.showToast('Error', 'Microphone not supported in this browser.', 'error');
            }
            return;
        }
        if (this.isRecording) {
            this.stopRecording(false);
            return;
        }
        this.targetCallback = callback;
        this.startRecording();
    }

    isSupported() {
        return !!(navigator.mediaDevices && navigator.mediaDevices.getUserMedia && window.MediaRecorder);
    }
}

// Initialize voice input
window.voiceInput = new VoiceInput();
