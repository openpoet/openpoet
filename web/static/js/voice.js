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
        this.timeoutTimer = setTimeout(() => this.cancelRecording(true), 60000);
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

        // Create form data
        const formData = new FormData();
        formData.append('audio', audioBlob, 'recording.webm');

        try {
            app.showToast('Info', 'Transcribing...', 'info');

            const response = await fetch('/api/voice/transcribe', {
                method: 'POST',
                body: formData
            });

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
            if (submit) {
                window.terminalManager.sendInput(text);
                setTimeout(() => {
                    window.terminalManager.sendInput('\r');
                }, 50);
            } else {
                // Move to end of line, add space if line has text, paste
                window.terminalManager.sendInput('\x05'); // Ctrl+E
                const lineContent = window.terminalManager.getActiveLineContent();
                if (lineContent.trim().length > 0) {
                    window.terminalManager.sendInput(' ');
                }
                window.terminalManager.sendInput(text);
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
        // Hide "Enviar" button when in callback mode (image paste modal)
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
