// Voice Input using MediaRecorder and OpenAI Whisper
class VoiceInput {
    constructor() {
        this.mediaRecorder = null;
        this.audioChunks = [];
        this.isRecording = false;
        this.autoSubmit = false;

        this.indicator = document.getElementById('voice-indicator');
        this.startBtn = document.getElementById('btn-voice-input');
        this.mobileBtn = document.getElementById('btn-mobile-voice-input');
        this.stopBtn = document.getElementById('voice-stop');
        this.cancelBtn = document.getElementById('voice-cancel');
        this.timerDisplay = document.getElementById('voice-timer');
        this.cancelled = false;
        this.recordingTimer = null;
        this.timeoutTimer = null;
        this.recordingSeconds = 0;

        this.setupEventListeners();
        this.loadSettings();
    }

    async loadSettings() {
        try {
            const response = await fetch('/api/config/settings');
            if (response.ok) {
                const settings = await response.json();
                this.autoSubmit = settings.voice_auto_submit === 'true';
            }
        } catch (error) {
            console.error('Failed to load voice settings:', error);
        }
    }

    updateSettings() {
        this.loadSettings();
    }

    setupEventListeners() {
        this.startBtn?.addEventListener('click', () => this.toggleRecording());
        this.mobileBtn?.addEventListener('click', () => this.toggleRecording());
        this.stopBtn?.addEventListener('click', () => this.stopRecording());
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
            this.stopRecording();
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

    stopRecording() {
        if (this.mediaRecorder && this.isRecording) {
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

            // Clear input line if auto-submit is enabled
            if (this.autoSubmit && window.terminalManager) {
                // Send Ctrl+U to clear the current line in terminal
                window.terminalManager.sendInput('\x15');
            }

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
                // Insert transcribed text into terminal
                if (window.terminalManager) {
                    // Send the transcribed text first
                    window.terminalManager.sendInput(result.text);

                    // If auto-submit is enabled, send Enter key separately
                    if (this.autoSubmit) {
                        // Small delay to ensure text is processed first
                        setTimeout(() => {
                            window.terminalManager.sendInput('\r');
                        }, 50);
                    }
                }
                app.showToast('Success', 'Transcription complete', 'success');
            }

        } catch (error) {
            console.error('Transcription error:', error);
            app.showToast('Error', error.message, 'error');
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
    }

    hideIndicator() {
        this.indicator?.classList.add('hidden');
        this.startBtn?.classList.remove('recording');
        this.mobileBtn?.classList.remove('recording');
    }

    isSupported() {
        return !!(navigator.mediaDevices && navigator.mediaDevices.getUserMedia && window.MediaRecorder);
    }
}

// Initialize voice input
window.voiceInput = new VoiceInput();
