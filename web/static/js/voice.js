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
        this.pendingTargetCallback = null; // destination bound to the in-flight upload
        this.pendingSubmit = false;
        this.uploadAttemptCount = 0;
        this.maxSingleUploadBytes = 20 * 1024 * 1024;
        this.maxChunkUploadBytes = 18 * 1024 * 1024;
        this.maxChunkSeconds = 90;

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
        // Bind the destination to THIS recording. targetCallback is read at
        // delivery time, seconds later; a recording started meanwhile would
        // otherwise steal this transcript — or, if none is registered by then,
        // it would fall through to insertText() and type into the terminal.
        this.pendingTargetCallback = this.targetCallback;

        await this.uploadWithRetry();
    }

    async uploadWithRetry() {
        if (!this.pendingAudioBlob) return;

        this.showUploading();

        try {
            const result = await this.transcribeBlob(this.pendingAudioBlob);
            // Success: clear pending state, deliver text.
            this.hideUploading();
            this.pendingAudioBlob = null;
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
            // All retries exhausted: keep the blob and offer manual retry.
            this.showRetryAvailable(err);
        }
    }

    async transcribeBlob(audioBlob) {
        if (audioBlob.size > this.maxSingleUploadBytes) {
            return this.uploadInChunks(audioBlob);
        }

        try {
            return await this.uploadBlobWithRetry(audioBlob);
        } catch (err) {
            if (this.isAudioTooLargeError(err)) {
                return this.uploadInChunks(audioBlob);
            }
            throw err;
        }
    }

    async uploadBlobWithRetry(audioBlob, options = {}) {
        // Backoff: immediate, 1.5s, 4s — total ~5.5s before giving up.
        const backoffsMs = [0, 1500, 4000];
        const sizeKB = Math.round(audioBlob.size / 1024);
        const filename = options.filename || 'recording.webm';

        let lastError = null;
        for (let attempt = 0; attempt < backoffsMs.length; attempt++) {
            this.uploadAttemptCount++;
            if (backoffsMs[attempt] > 0) {
                const retryLabel = options.totalChunks
                    ? `Retrying part ${options.chunkIndex}/${options.totalChunks}`
                    : 'Retrying';
                this.setStatusLabel(`${retryLabel} (${attempt + 1}/${backoffsMs.length})…`);
                await new Promise(resolve => setTimeout(resolve, backoffsMs[attempt]));
            } else if (options.totalChunks) {
                this.setStatusLabel(`Sending part ${options.chunkIndex}/${options.totalChunks} (${sizeKB} KB)…`);
            } else {
                this.setStatusLabel(`Sending ${sizeKB} KB…`);
            }

            try {
                return await this.uploadOnce(audioBlob, filename);
            } catch (err) {
                lastError = err;
                console.warn(`[voice] attempt ${attempt + 1} failed:`, err);
                if (this.isAudioTooLargeError(err)) throw err;
                if (!this.isRetryableError(err)) break;
            }
        }

        throw lastError || new Error('Upload failed');
    }

    async uploadInChunks(audioBlob) {
        this.setStatusLabel('Splitting audio…');
        const chunks = await this.createTranscriptionChunks(audioBlob);
        const texts = [];

        for (let i = 0; i < chunks.length; i++) {
            const result = await this.uploadBlobWithRetry(chunks[i].blob, {
                filename: chunks[i].filename,
                chunkIndex: i + 1,
                totalChunks: chunks.length
            });
            const text = result?.text?.trim();
            if (text) texts.push(text);
        }

        return { text: texts.join(' ').replace(/\s+/g, ' ').trim() };
    }

    async createTranscriptionChunks(audioBlob) {
        const AudioContextCtor = window.AudioContext || window.webkitAudioContext;
        if (!AudioContextCtor) {
            const err = new Error('Recording too large and this browser cannot split audio');
            err.nonRetryable = true;
            throw err;
        }

        const audioContext = new AudioContextCtor();
        let decoded;
        try {
            const arrayBuffer = await audioBlob.arrayBuffer();
            decoded = await audioContext.decodeAudioData(arrayBuffer.slice(0));
        } catch (err) {
            const splitErr = new Error('Recording too large and could not be split');
            splitErr.cause = err;
            splitErr.nonRetryable = true;
            throw splitErr;
        } finally {
            if (audioContext.close) {
                const closeResult = audioContext.close();
                if (closeResult?.catch) closeResult.catch(() => {});
            }
        }

        const sampleRate = decoded.sampleRate;
        const totalFrames = decoded.length;
        const channelCount = decoded.numberOfChannels || 1;
        const monoSamples = new Float32Array(totalFrames);

        for (let channel = 0; channel < channelCount; channel++) {
            const source = decoded.getChannelData(channel);
            for (let i = 0; i < totalFrames; i++) {
                monoSamples[i] += source[i] / channelCount;
            }
        }

        const bytesPerSecond = sampleRate * 2; // mono 16-bit PCM
        const maxSecondsBySize = Math.floor((this.maxChunkUploadBytes - 44) / bytesPerSecond);
        const chunkSeconds = Math.max(10, Math.min(this.maxChunkSeconds, maxSecondsBySize));
        const framesPerChunk = Math.max(sampleRate * 10, Math.floor(chunkSeconds * sampleRate));
        const chunks = [];

        for (let start = 0; start < totalFrames; start += framesPerChunk) {
            const end = Math.min(start + framesPerChunk, totalFrames);
            const wavBuffer = this.encodeWavMono(monoSamples.subarray(start, end), sampleRate);
            chunks.push({
                blob: new Blob([wavBuffer], { type: 'audio/wav' }),
                filename: `recording-part-${chunks.length + 1}.wav`
            });
        }

        if (chunks.length === 0) {
            throw new Error('No audio recorded');
        }

        return chunks;
    }

    encodeWavMono(samples, sampleRate) {
        const bytesPerSample = 2;
        const blockAlign = bytesPerSample;
        const dataSize = samples.length * bytesPerSample;
        const buffer = new ArrayBuffer(44 + dataSize);
        const view = new DataView(buffer);

        this.writeAscii(view, 0, 'RIFF');
        view.setUint32(4, 36 + dataSize, true);
        this.writeAscii(view, 8, 'WAVE');
        this.writeAscii(view, 12, 'fmt ');
        view.setUint32(16, 16, true);
        view.setUint16(20, 1, true);
        view.setUint16(22, 1, true);
        view.setUint32(24, sampleRate, true);
        view.setUint32(28, sampleRate * blockAlign, true);
        view.setUint16(32, blockAlign, true);
        view.setUint16(34, 16, true);
        this.writeAscii(view, 36, 'data');
        view.setUint32(40, dataSize, true);

        let offset = 44;
        for (let i = 0; i < samples.length; i++, offset += 2) {
            const sample = Math.max(-1, Math.min(1, samples[i]));
            view.setInt16(offset, sample < 0 ? sample * 0x8000 : sample * 0x7fff, true);
        }

        return buffer;
    }

    writeAscii(view, offset, text) {
        for (let i = 0; i < text.length; i++) {
            view.setUint8(offset + i, text.charCodeAt(i));
        }
    }

    async uploadOnce(audioBlob, filename = 'recording.webm') {
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
                    body: JSON.stringify({ audio: base64, filename }),
                    signal: controller.signal
                });
            } else {
                const formData = new FormData();
                formData.append('audio', audioBlob, filename);
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

    isAudioTooLargeError(err) {
        if (!err) return false;
        if (err.status === 413) return true;

        const message = String(err.message || '').toLowerCase();
        return message.includes('too large')
            || message.includes('maximum')
            || message.includes('exceeds')
            || message.includes('file size')
            || message.includes('request entity too large')
            || message.includes('25mb')
            || message.includes('25 mb');
    }

    isRetryableError(err) {
        // AbortError, network failure (TypeError from fetch), 5xx, 429.
        if (!err) return false;
        if (err.nonRetryable) return false;
        if (this.isAudioTooLargeError(err)) return false;
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
        this.pendingTargetCallback = null;
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
