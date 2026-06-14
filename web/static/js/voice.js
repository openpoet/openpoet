// Voice Input using MediaRecorder and OpenAI Whisper
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
        // Held between attempts so retries don't lose the recording.
        this.pendingAudioBlob = null;
        this.pendingSubmit = false;
        this.uploadAttemptCount = 0;

        this.setupEventListeners();
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

    async startRecording() {
        try {
            const stream = await navigator.mediaDevices.getUserMedia({ audio: true });

            this.audioChunks = [];
            this.mediaRecorder = new MediaRecorder(stream, {
                mimeType: this.getSupportedMimeType()
            });

            this.mediaRecorder.ondataavailable = (event) => {
                if (event.data.size > 0) {
                    this.audioChunks.push(event.data);
                }
            };

            this.mediaRecorder.onstop = async () => {
                stream.getTracks().forEach(track => track.stop());
                if (this.cancelled) {
                    this.cancelled = false;
                    this.audioChunks = [];
                    return;
                }
                await this.processRecording();
            };

            this.mediaRecorder.start();
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
        app.showToast('Info', 'Recording auto-stopped: 60s limit reached. Transcribing...', 'info');
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

    cancelRecording(isTimeout = false) {
        if (this.mediaRecorder && this.isRecording) {
            this.clearTimers();
            this.cancelled = true;
            this.targetCallback = null;
            this.mediaRecorder.stop();
            this.isRecording = false;
            this.hideIndicator();
            if (isTimeout) {
                app.showToast('Warning', 'Recording cancelled: 60s limit reached', 'warning');
            } else {
                app.showToast('Info', 'Recording cancelled', 'info');
            }
        }
    }

    startTimers() {
        this.recordingSeconds = 0;
        this.updateTimerDisplay();
        this.recordingTimer = setInterval(() => {
            this.recordingSeconds++;
            this.updateTimerDisplay();
        }, 1000);
        this.timeoutTimer = setTimeout(() => this.autoStopRecording(), 60000);
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
        this.timerDisplay.classList.toggle('warning', this.recordingSeconds >= 55);
    }

    async processRecording() {
        if (this.audioChunks.length === 0) {
            app.showToast('Error', 'No audio recorded', 'error');
            return;
        }

        // Keep the blob around so we can retry without losing the recording.
        this.pendingAudioBlob = new Blob(this.audioChunks, { type: this.getSupportedMimeType() });
        this.pendingSubmit = this.submitAfterTranscribe;
        this.uploadAttemptCount = 0;

        await this.uploadWithRetry();
    }

    async uploadWithRetry() {
        if (!this.pendingAudioBlob) return;

        // Backoff: immediate, 1.5s, 4s — total ~5.5s before giving up.
        const backoffsMs = [0, 1500, 4000];
        const sizeKB = Math.round(this.pendingAudioBlob.size / 1024);

        this.showUploading();

        let lastError = null;
        for (let attempt = 0; attempt < backoffsMs.length; attempt++) {
            this.uploadAttemptCount++;
            if (backoffsMs[attempt] > 0) {
                this.setStatusLabel(`Retrying (${attempt + 1}/${backoffsMs.length})…`);
                await new Promise(resolve => setTimeout(resolve, backoffsMs[attempt]));
            } else {
                this.setStatusLabel(`Sending ${sizeKB} KB…`);
            }

            try {
                const result = await this.uploadOnce(this.pendingAudioBlob);
                // Success: clear pending state, deliver text.
                this.hideUploading();
                this.pendingAudioBlob = null;
                if (result && result.text) {
                    if (this.targetCallback) {
                        this.targetCallback(result.text);
                        this.targetCallback = null;
                    } else {
                        this.insertText(result.text, this.pendingSubmit);
                    }
                }
                return;
            } catch (err) {
                lastError = err;
                console.warn(`[voice] attempt ${attempt + 1} failed:`, err);
                if (!this.isRetryableError(err)) break;
            }
        }

        // All retries exhausted: keep the blob and offer manual retry.
        this.showRetryAvailable(lastError);
    }

    async uploadOnce(audioBlob) {
        // Detect relay: binary multipart data gets corrupted through the
        // tunnel JSON serialization, so send base64-encoded JSON instead.
        const isTunnel = window.location.hostname !== 'localhost'
            && window.location.hostname !== '127.0.0.1'
            && !window.location.hostname.match(/^(10|192\.168|172\.(1[6-9]|2\d|3[01]))\./);

        // Per-attempt 90s ceiling — generous enough for slow uplinks on large
        // recordings, tight enough to retry sooner than browser defaults.
        const controller = new AbortController();
        const abortTimer = setTimeout(() => controller.abort(), 90000);

        let response;
        try {
            if (isTunnel) {
                const arrayBuf = await audioBlob.arrayBuffer();
                const bytes = new Uint8Array(arrayBuf);
                let binary = '';
                for (let i = 0; i < bytes.length; i += 8192) {
                    binary += String.fromCharCode(...bytes.subarray(i, i + 8192));
                }
                const base64 = btoa(binary);
                response = await fetch('/api/voice/transcribe', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ audio: base64, filename: 'recording.webm' }),
                    signal: controller.signal
                });
            } else {
                const formData = new FormData();
                formData.append('audio', audioBlob, 'recording.webm');
                response = await fetch('/api/voice/transcribe', {
                    method: 'POST',
                    body: formData,
                    signal: controller.signal
                });
            }
        } finally {
            clearTimeout(abortTimer);
        }

        if (!response.ok) {
            const message = await this.parseErrorBody(response);
            const err = new Error(message);
            err.status = response.status;
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
                if (json && json.error) return json.error;
            } catch (_) {
                // Not JSON (likely HTML 502/504 from a tunnel) — fall through.
            }
        }
        const status = response.status;
        if (status === 0) return 'Network error';
        if (status === 413) return 'Recording too large for the server';
        if (status === 429) return 'Rate limited — please retry';
        if (status >= 500) return `Server error (${status})`;
        return `Request failed (${status})`;
    }

    isRetryableError(err) {
        // AbortError, network failure (TypeError from fetch), 5xx, 429.
        if (!err) return false;
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
        this.pendingAudioBlob = null;
        this.pendingSubmit = false;
        this.targetCallback = null;
        this.hideUploading();
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
        this.indicator?.classList.remove('uploading');
        this.indicator?.classList.add('retry-available');
        const msg = err && err.message ? err.message : 'Upload failed';
        this.setStatusLabel(`${msg} — tap Retry`);
        this.toggleRecordingButtons(false);
        this.toggleRetryButtons(true);
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
