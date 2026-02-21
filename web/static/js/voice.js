// Voice Input using MediaRecorder and OpenAI Whisper
class VoiceInput {
    constructor() {
        this.mediaRecorder = null;
        this.audioChunks = [];
        this.isRecording = false;
        this.submitAfterTranscribe = false;
        this.targetCallback = null; // When set, transcribed text goes here instead of terminal

        this.indicator = document.getElementById('voice-indicator');
        this.startBtn = document.getElementById('btn-voice-input');
        this.mobileBtn = document.getElementById('btn-mobile-voice-input');
        this.stopBtn = document.getElementById('voice-stop-btn');
        this.sendBtn = document.getElementById('voice-send');
        this.cancelBtn = document.getElementById('voice-cancel');
        this.timerDisplay = document.getElementById('voice-timer');
        this.cancelled = false;
        this.recordingTimer = null;
        this.timeoutTimer = null;
        this.recordingSeconds = 0;

        this.setupEventListeners();
    }

    setupEventListeners() {
        this.startBtn?.addEventListener('click', () => this.toggleRecording());
        this.mobileBtn?.addEventListener('click', () => this.toggleRecording());
        this.stopBtn?.addEventListener('click', () => this.stopRecording(false));
        this.sendBtn?.addEventListener('click', () => this.stopRecording(true));
        this.cancelBtn?.addEventListener('click', () => this.cancelRecording());
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
            this.hideIndicator();
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

        const audioBlob = new Blob(this.audioChunks, { type: this.getSupportedMimeType() });

        try {
            app.showToast('Info', 'Transcribing...', 'info');

            // Detect relay: binary multipart data gets corrupted through the
            // tunnel JSON serialization, so send base64-encoded JSON instead.
            const isTunnel = window.location.hostname !== 'localhost'
                && window.location.hostname !== '127.0.0.1'
                && !window.location.hostname.match(/^(10|192\.168|172\.(1[6-9]|2\d|3[01]))\./);

            let response;
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
                    body: JSON.stringify({ audio: base64, filename: 'recording.webm' })
                });
            } else {
                const formData = new FormData();
                formData.append('audio', audioBlob, 'recording.webm');
                response = await fetch('/api/voice/transcribe', {
                    method: 'POST',
                    body: formData
                });
            }

            if (!response.ok) {
                const error = await response.json();
                throw new Error(error.error || 'Transcription failed');
            }

            const result = await response.json();

            if (result.text) {
                // If a target callback is set, send text there instead of terminal
                if (this.targetCallback) {
                    this.targetCallback(result.text);
                    this.targetCallback = null;
                } else {
                    this.insertText(result.text, this.submitAfterTranscribe);
                }
                app.showToast('Success', 'Transcription complete', 'success');
            }

        } catch (error) {
            console.error('Transcription error:', error);
            app.showToast('Error', error.message, 'error');
        }
    }

    // Insert transcribed text into the appropriate input (mobile input bar or terminal)
    insertText(text, submit) {
        const isMobile = window.innerWidth <= 768;
        const mobileInput = document.getElementById('mobile-terminal-input');

        if (isMobile && mobileInput) {
            // Mobile: use the mobile input bar
            const current = mobileInput.value;
            if (current.length > 0) {
                mobileInput.value = current + ' ' + text;
            } else {
                mobileInput.value = text;
            }

            if (submit) {
                // Trigger the mobile send flow
                if (window.app) {
                    window.app.sendMobileTerminalInput();
                }
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
        this.startBtn?.classList.add('recording');
        this.mobileBtn?.classList.add('recording');
        // Hide "Send" button when in callback mode (image paste modal)
        this.sendBtn?.classList.toggle('hidden', !!this.targetCallback);
    }

    hideIndicator() {
        this.indicator?.classList.add('hidden');
        this.startBtn?.classList.remove('recording');
        this.mobileBtn?.classList.remove('recording');
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
