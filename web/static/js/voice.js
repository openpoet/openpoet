// Voice Input using MediaRecorder and Whisper-style transcription
//
// A recording must never be lost or truncated:
//   1. Audio is cut into segments of roughly 12–25 s, always at a pause
//      (a live voice-activity detector watches the microphone). Whisper
//      transcribes one 30 s window reliably; longer audio goes through its
//      long-form mode, which can silently drop whole windows it judges to be
//      "no speech" — that is how the tail of long dictations went missing.
//   2. Before upload, long pauses inside a segment are shortened (voice and
//      400 ms around it are always kept). After a long silence Whisper tends
//      to stop transcribing and skip the speech that follows — the thinking
//      pauses of a real dictation are exactly where text went missing.
//   3. Each segment is transcribed in the background while the user keeps
//      talking, and the final transcript is the segments joined in order.
//      The language detected on the first segment is pinned on the rest.
//      (No previous-text prompt: over pauses Whisper copies the prompt back
//      as invented speech.)
//   4. Everything is persisted to IndexedDB every second (audio and finished
//      segment texts), so a reload, crash or failed upload resumes without
//      re-recording or re-transcribing (see VoiceStore).
//   5. Uploads go in small idempotent byte slices (the public proxy rejects
//      bodies over 1 MiB) that the server reassembles (/api/voice/uploads/…).

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

    // mutate receives the stored record and changes it in place.
    async updateRecording(id, mutate) {
        try {
            await this.tx(['recordings'], 'readwrite', tx => {
                const store = tx.objectStore('recordings');
                const get = store.get(id);
                get.onsuccess = () => {
                    if (!get.result) return;
                    const record = get.result;
                    mutate(record);
                    record.updatedAt = Date.now();
                    store.put(record);
                };
            });
        } catch (err) { console.warn('[voice] could not update recording metadata:', err); }
    },

    async appendChunk(id, seq, segment, blob) {
        try {
            await this.tx(['chunks', 'recordings'], 'readwrite', tx => {
                tx.objectStore('chunks').put({ id, seq, segment, blob });
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

    // Returns the recording's segments as [{index, blob}] in order.
    async loadSegments(record) {
        try {
            const range = IDBKeyRange.bound([record.id, 0], [record.id, Number.MAX_SAFE_INTEGER]);
            const chunks = await this.tx(['chunks'], 'readonly', tx => new Promise(resolve => {
                const req = tx.objectStore('chunks').getAll(range);
                req.onsuccess = () => resolve(req.result || []);
                req.onerror = () => resolve([]);
            }));
            const bySegment = new Map();
            chunks.sort((a, b) => a.seq - b.seq);
            for (const chunk of chunks) {
                const index = chunk.segment || 0;
                if (!bySegment.has(index)) bySegment.set(index, []);
                bySegment.get(index).push(chunk.blob);
            }
            return [...bySegment.keys()].sort((a, b) => a - b).map(index => ({
                index,
                blob: new Blob(bySegment.get(index), { type: record.mimeType || 'audio/webm' })
            }));
        } catch (_) { return []; }
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
        this.recordingTimer = null;
        this.timeoutTimer = null;
        this.recordingSeconds = 0;

        // Live recording state.
        this.stream = null;
        this.mediaRecorder = null; // recorder of the segment being captured
        this.currentSegment = null;
        this.chunkSeq = 0;
        this.vad = null;

        // The recording being transcribed ("job"): segments with their audio
        // and, once transcribed, their text. It survives failures and retries.
        this.job = null;
        this.uploadAttemptCount = 0;

        this.maxRecordingSeconds = 20 * 60;
        this.audioBitsPerSecond = 32000;
        this.sliceBytes = 256 * 1024; // ~350 KB as base64 JSON, far below the proxy's 1 MiB
        // Segment cut policy: [minimum segment age in s, silence needed in ms].
        // The longer a segment runs, the shorter the pause that ends it, and
        // none outgrows one 30 s Whisper window.
        this.segmentCutRules = [[12, 700], [20, 350], [26, 150]];
        this.segmentHardSeconds = 29;

        this.setupEventListeners();
        this.recoverPendingRecordings();
    }

    // Back-compat for callers that test "is there audio awaiting a transcript".
    get pendingAudioBlob() {
        return this.job && this.job.closed ? this.job : null;
    }

    get uploading() {
        return !!this.job?.pumping;
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
        let stream;
        try {
            stream = await navigator.mediaDevices.getUserMedia({ audio: true });
        } catch (error) {
            console.error('Failed to start recording:', error);
            app.showToast('Error', 'Could not access microphone', 'error');
            return;
        }

        // A previous recording still awaiting its transcript stays in
        // IndexedDB and is offered again once this one is delivered.
        const id = this.newRecordingId();
        this.stream = stream;
        this.chunkSeq = 0;
        this.job = {
            id,
            mimeType: null,
            segments: [],
            closed: false,
            submit: false,
            target: this.targetCallback,
            error: null,
            recovered: false
        };
        try {
            this.startSegment();
        } catch (error) {
            console.error('Failed to start recording:', error);
            stream.getTracks().forEach(track => track.stop());
            this.stream = null;
            this.job = null;
            app.showToast('Error', 'Could not start recording', 'error');
            return;
        }
        VoiceStore.putRecording({
            id,
            mimeType: this.job.mimeType,
            status: 'recording',
            texts: {},
            createdAt: Date.now(),
            updatedAt: Date.now()
        });
        this.startVAD(stream);
        this.isRecording = true;
        this.showIndicator();
        this.startTimers();
    }

    // Starts capturing a new segment on the shared stream. Called at start and
    // on every rotation; the new recorder starts before the old one stops so
    // no audio falls between them (the overlap sits inside a pause).
    startSegment() {
        const job = this.job;
        const recorder = this.createMediaRecorder(this.stream);
        const segment = {
            index: job.segments.length,
            chunks: [],
            blob: null,
            text: null,
            seconds: 0,
            startedAt: performance.now(),
            uploadedSlices: new Set()
        };
        job.segments.push(segment);
        job.mimeType = job.mimeType || recorder.mimeType || this.getSupportedMimeType();

        recorder.ondataavailable = (event) => {
            if (event.data.size > 0) {
                segment.chunks.push(event.data);
                VoiceStore.appendChunk(job.id, this.chunkSeq++, segment.index, event.data);
            }
        };
        recorder.onstop = () => this.finishSegment(job, segment);

        // A timeslice flushes audio every second so it reaches IndexedDB
        // while the user is still talking, not only at stop().
        recorder.start(1000);
        this.mediaRecorder = recorder;
        this.currentSegment = segment;
    }

    rotateSegment() {
        if (!this.isRecording || !this.mediaRecorder) return;
        const previous = this.mediaRecorder;
        this.startSegment();
        previous.stop();
    }

    finishSegment(job, segment) {
        segment.seconds = (performance.now() - segment.startedAt) / 1000;
        segment.blob = new Blob(segment.chunks, { type: job.mimeType || 'audio/webm' });
        segment.chunks = [];
        if (job.cancelled) return;
        // Close only once every segment is finalized: after a rotation the
        // previous recorder's stop can land after the final one's.
        if (job.stopRequested && !job.closed && job.segments.every(s => s.blob)) {
            this.onRecordingClosed(job);
        } else {
            this.pump(job);
        }
    }

    // Voice activity detection on the live stream: decides when to rotate to
    // a new segment. Adapts to the room's noise floor so it works with AGC /
    // noise suppression on phones and laptops alike.
    startVAD(stream) {
        const AudioContextCtor = window.AudioContext || window.webkitAudioContext;
        let ctx = null;
        let analyser = null;
        try {
            ctx = new AudioContextCtor();
            if (ctx.state === 'suspended' && ctx.resume) ctx.resume().catch(() => {});
            const source = ctx.createMediaStreamSource(stream);
            analyser = ctx.createAnalyser();
            analyser.fftSize = 2048;
            source.connect(analyser);
        } catch (err) {
            console.warn('[voice] no voice activity detection, using fixed-length segments:', err);
            if (ctx?.close) ctx.close().catch(() => {});
            ctx = null;
            analyser = null;
        }

        const samples = analyser ? new Float32Array(analyser.fftSize) : null;
        let floor = null;
        let silentMs = 0;
        let last = performance.now();
        const timer = setInterval(() => {
            const now = performance.now();
            const elapsed = now - last;
            last = now;
            const segment = this.currentSegment;
            if (!segment || !this.isRecording) return;
            const age = (now - segment.startedAt) / 1000;

            if (analyser) {
                analyser.getFloatTimeDomainData(samples);
                let sum = 0;
                for (let i = 0; i < samples.length; i++) sum += samples[i] * samples[i];
                const rms = Math.sqrt(sum / samples.length);
                // Floor follows drops immediately and rises slowly, so speech
                // does not drag it up but a noisier room eventually does.
                floor = floor === null || rms < floor ? rms : floor + (rms - floor) * 0.005;
                const silent = rms < Math.max(floor * 2.5, 0.004);
                silentMs = silent ? silentMs + elapsed : 0;
            }

            const cut = this.segmentCutRules.some(([minAge, minSilence]) => age >= minAge && silentMs >= minSilence)
                || age >= this.segmentHardSeconds;
            if (cut) {
                silentMs = 0;
                this.rotateSegment();
            }
        }, 50);
        this.vad = { ctx, timer };
    }

    stopVAD() {
        if (!this.vad) return;
        clearInterval(this.vad.timer);
        if (this.vad.ctx?.close) this.vad.ctx.close().catch(() => {});
        this.vad = null;
    }

    releaseStream() {
        this.stopVAD();
        this.stream?.getTracks().forEach(track => track.stop());
        this.stream = null;
        this.mediaRecorder = null;
        this.currentSegment = null;
    }

    stopRecording(autoSubmit = false) {
        if (!this.isRecording || !this.mediaRecorder) return;
        const job = this.job;
        job.submit = autoSubmit;
        job.stopRequested = true;
        this.clearTimers();
        this.isRecording = false;
        this.stopVAD();
        this.mediaRecorder.stop();
        // Transition straight into the uploading state so there's no flash
        // where the indicator disappears between stop and upload-start.
        this.showUploading();
        this.setStatusLabel('Preparing…');
    }

    onRecordingClosed(job) {
        if (this.job === job) this.releaseStream();
        job.segments = job.segments.filter(s => s.blob && s.blob.size > 0);
        if (job.segments.length === 0) {
            VoiceStore.remove(job.id);
            if (this.job === job) {
                this.job = null;
                this.hideUploading();
            }
            app.showToast('Error', 'No audio recorded', 'error');
            return;
        }
        job.closed = true;
        VoiceStore.updateRecording(job.id, r => { r.status = 'stopped'; r.submit = job.submit; });
        this.pump(job);
    }

    autoStopRecording() {
        if (!this.isRecording) return;
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
        if (!this.isRecording) return;
        const job = this.job;
        job.cancelled = true;
        this.clearTimers();
        this.targetCallback = null;
        this.isRecording = false;
        this.mediaRecorder?.stop();
        this.releaseStream();
        this.job = null;
        VoiceStore.remove(job.id);
        this.discardServerUploads(job);
        this.hideIndicator();
        app.showToast('Info', 'Recording cancelled', 'info');
        this.recoverPendingRecordings();
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

    // Offer any recording persisted by an earlier page load (or an earlier
    // failed upload) that never produced a transcript.
    async recoverPendingRecordings() {
        if (this.isRecording || this.job) return;
        const now = Date.now();
        const records = (await VoiceStore.listRecordings())
            // Skip recordings another tab is actively writing or uploading.
            .filter(r => r.status === 'stopped' || now - (r.updatedAt || 0) > 15000)
            .filter(r => r.status !== 'uploading' || now - (r.updatedAt || 0) > 180000)
            .sort((a, b) => (a.createdAt || 0) - (b.createdAt || 0));
        for (const record of records) {
            if (this.isRecording || this.job) return;
            const stored = await VoiceStore.loadSegments(record);
            const segments = stored.filter(s => s.blob.size > 0).map(s => ({
                index: s.index,
                blob: s.blob,
                text: record.texts?.[s.index] ?? null,
                seconds: 0,
                uploadedSlices: new Set()
            }));
            if (segments.length === 0) {
                VoiceStore.remove(record.id);
                continue;
            }
            this.job = {
                id: record.id,
                mimeType: record.mimeType,
                segments,
                closed: true,
                submit: false,
                target: null,
                language: record.language || null,
                error: null,
                recovered: true
            };
            const size = segments.reduce((n, s) => n + s.blob.size, 0);
            const when = new Date(record.createdAt || now).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
            this.showRetryAvailable(new Error(`Unsent recording from ${when} (${Math.round(size / 1024)} KB) recovered`));
            return;
        }
    }

    // Transcribes the job's segments in order, one at a time. Runs in the
    // background while recording (a failure there is retried at the next
    // rotation or at stop) and in the foreground after stop, where exhausting
    // the retries hands the decision to the user with the audio kept. Each
    // job is independent: starting a new recording never strands the
    // transcript of the previous one.
    async pump(job) {
        if (job.pumping || job.cancelled) return;
        job.pumping = true;
        let failure = null;
        try {
            if (job.closed) {
                if (job === this.job) this.showUploading();
                VoiceStore.updateRecording(job.id, r => { r.status = 'uploading'; });
            }
            for (;;) {
                // Strictly in order: the next untranscribed segment, and only
                // once its audio is finalized (the first one sets the language).
                const position = job.segments.findIndex(s => s.text === null);
                if (position < 0 || !job.segments[position].blob) break;
                const segment = job.segments[position];
                const result = await this.transcribeSegment(job, segment);
                if (job.cancelled) return;
                segment.text = (result?.text || '').trim();
                if (!job.language && result?.language && segment.text.length >= 20) job.language = result.language;
                const text = segment.text;
                const language = job.language || null;
                VoiceStore.updateRecording(job.id, r => {
                    r.texts = { ...(r.texts || {}), [segment.index]: text };
                    r.language = language;
                });
            }
        } catch (err) {
            failure = err;
        } finally {
            job.pumping = false;
        }

        if (job.cancelled) return;
        if (failure) {
            console.warn('[voice] segment transcription failed:', failure);
            if (job.closed) {
                VoiceStore.updateRecording(job.id, r => { r.status = 'stopped'; });
                // A job the user moved on from stays in IndexedDB and is
                // offered again (recoverPendingRecordings) once they are free.
                if (job === this.job) this.showRetryAvailable(failure);
            }
            return;
        }
        // A segment may have been finalized while the last request was in flight.
        if (job.segments.some(s => s.blob && s.text === null)) {
            this.pump(job);
            return;
        }
        if (job.closed) this.deliver(job);
    }

    deliver(job) {
        if (this.job === job) {
            this.hideUploading();
            this.job = null;
        }
        VoiceStore.remove(job.id);
        const text = job.segments.map(s => s.text).filter(Boolean).join(' ').replace(/\s+/g, ' ').trim();
        if (this.targetCallback === job.target) this.targetCallback = null;
        if (text) {
            if (job.target) {
                job.target(text);
            } else {
                this.insertText(text, job.submit);
            }
        }
        this.recoverPendingRecordings();
    }

    recordingFilename(mimeType) {
        const type = String(mimeType || '').toLowerCase();
        if (type.includes('mp4') || type.includes('aac') || type.includes('m4a')) return 'recording.mp4';
        if (type.includes('ogg')) return 'recording.ogg';
        if (type.includes('wav')) return 'recording.wav';
        if (type.includes('mpeg') || type.includes('mp3')) return 'recording.mp3';
        return 'recording.webm';
    }

    // Upload one segment as byte slices, then ask the server to reassemble
    // and transcribe it. Slices the server already holds are not re-sent;
    // slices it reports missing (e.g. lost across a restart) are.
    async transcribeSegment(job, segment) {
        segment.prepared = segment.prepared || await this.prepareSegmentAudio(job, segment);
        const { blob, filename } = segment.prepared;
        const uploadId = `${job.id}-s${segment.index}`;
        const totalSlices = Math.max(1, Math.ceil(blob.size / this.sliceBytes));
        const base = `/api/voice/uploads/${encodeURIComponent(uploadId)}`;
        const total = job.segments.length;
        const label = job.closed && total > 1 ? `Transcribing part ${segment.index + 1}/${total}` : 'Transcribing';

        for (let round = 0; round < 3; round++) {
            for (let i = 0; i < totalSlices; i++) {
                if (segment.uploadedSlices.has(i)) continue;
                const slice = blob.slice(i * this.sliceBytes, Math.min(blob.size, (i + 1) * this.sliceBytes));
                const data = await this.blobToBase64(slice);
                await this.requestWithRetry(
                    () => this.postJSON(`${base}/chunks/${i}`, { data }, 90000),
                    totalSlices > 1 ? `${label} — sending ${i + 1}/${totalSlices}` : label,
                    job
                );
                segment.uploadedSlices.add(i);
            }

            try {
                return await this.requestWithRetry(
                    () => this.postJSON(`${base}/complete`, {
                        chunks: totalSlices,
                        filename,
                        language: job.language || undefined,
                        seconds: Math.round(segment.seconds * 10) / 10,
                        kept_seconds: segment.keptSeconds ? Math.round(segment.keptSeconds * 10) / 10 : undefined
                    }, 180000),
                    label,
                    job
                );
            } catch (err) {
                if (err.status === 409 && Array.isArray(err.body?.missing)) {
                    err.body.missing.forEach(i => segment.uploadedSlices.delete(i));
                    continue;
                }
                throw err;
            }
        }
        throw new Error('Upload could not be completed');
    }

    // Shortens long pauses: decodes the segment to 16 kHz mono, marks 20 ms
    // frames as voice when their energy clears a threshold derived from the
    // segment's own noise floor, keeps every voice frame plus 400 ms on each
    // side, and drops the rest of each pause. Falls back to the original
    // recording whenever decoding fails or there is little to remove.
    async prepareSegmentAudio(job, segment) {
        const original = { blob: segment.blob, filename: this.recordingFilename(job.mimeType || segment.blob.type) };
        const OfflineCtor = window.OfflineAudioContext || window.webkitOfflineAudioContext;
        if (!OfflineCtor) return original;
        let audio;
        try {
            const rate = 16000;
            const ctx = new OfflineCtor(1, rate, rate);
            const bytes = await segment.blob.arrayBuffer();
            audio = await new Promise((resolve, reject) => {
                const result = ctx.decodeAudioData(bytes, resolve, reject);
                if (result?.then) result.then(resolve, reject);
            });
        } catch (err) {
            console.warn('[voice] could not decode segment, sending it unchanged:', err);
            return original;
        }

        const rate = audio.sampleRate;
        const mono = new Float32Array(audio.length);
        for (let c = 0; c < audio.numberOfChannels; c++) {
            const data = audio.getChannelData(c);
            for (let i = 0; i < data.length; i++) mono[i] += data[i] / audio.numberOfChannels;
        }
        const frame = Math.round(rate * 0.02);
        const frames = Math.floor(mono.length / frame);
        if (frames < 50) return original;
        const energy = new Float32Array(frames);
        for (let f = 0; f < frames; f++) {
            let sum = 0;
            for (let i = f * frame; i < (f + 1) * frame; i++) sum += mono[i] * mono[i];
            energy[f] = Math.sqrt(sum / frame);
        }
        const sorted = Float32Array.from(energy).sort();
        const floor = sorted[Math.floor(frames * 0.1)];
        const loud = sorted[Math.floor(frames * 0.98)];
        // Voice is ~10 dB over the noise floor. In a noisy room that could
        // exceed quiet speech, so it is capped relative to the loudest speech —
        // but only when the segment clearly has speech; a segment that is mostly
        // pause would otherwise pull the cap down to the noise itself.
        let threshold = Math.max(floor * 3, 0.002);
        if (loud > floor * 6) threshold = Math.min(threshold, loud * 0.15);
        const pad = 20; // frames = 400 ms
        const keep = new Uint8Array(frames);
        let voiced = 0;
        for (let f = 0; f < frames; f++) {
            if (energy[f] <= threshold) continue;
            voiced++;
            for (let k = Math.max(0, f - pad); k <= Math.min(frames - 1, f + pad); k++) keep[k] = 1;
        }
        const keptFrames = keep.reduce((n, v) => n + v, 0);
        // No voice found means the detector is not to be trusted here; less
        // than a second to remove is not worth a larger upload.
        if (voiced === 0 || frames - keptFrames < 50) return original;

        const out = new Float32Array(keptFrames * frame);
        let o = 0;
        for (let f = 0; f < frames; f++) {
            if (!keep[f]) continue;
            out.set(mono.subarray(f * frame, (f + 1) * frame), o);
            o += frame;
        }
        segment.keptSeconds = out.length / rate;
        return { blob: new Blob([this.encodeWav(out, rate)], { type: 'audio/wav' }), filename: 'recording.wav' };
    }

    encodeWav(samples, rate) {
        const buffer = new ArrayBuffer(44 + samples.length * 2);
        const view = new DataView(buffer);
        const ascii = (offset, text) => { for (let i = 0; i < text.length; i++) view.setUint8(offset + i, text.charCodeAt(i)); };
        ascii(0, 'RIFF');
        view.setUint32(4, 36 + samples.length * 2, true);
        ascii(8, 'WAVE');
        ascii(12, 'fmt ');
        view.setUint32(16, 16, true);
        view.setUint16(20, 1, true);   // PCM
        view.setUint16(22, 1, true);   // mono
        view.setUint32(24, rate, true);
        view.setUint32(28, rate * 2, true);
        view.setUint16(32, 2, true);
        view.setUint16(34, 16, true);
        ascii(36, 'data');
        view.setUint32(40, samples.length * 2, true);
        for (let i = 0, offset = 44; i < samples.length; i++, offset += 2) {
            const x = Math.max(-1, Math.min(1, samples[i]));
            view.setInt16(offset, x < 0 ? x * 0x8000 : x * 0x7fff, true);
        }
        return buffer;
    }

    async requestWithRetry(send, label, job) {
        // Backoff: immediate, 1.5s, 4s, 8s — ~13.5s before handing the
        // decision to the user (the recording is kept either way).
        const backoffsMs = [0, 1500, 4000, 8000];
        let lastError = null;
        for (let attempt = 0; attempt < backoffsMs.length; attempt++) {
            this.uploadAttemptCount++;
            if (backoffsMs[attempt] > 0) {
                this.setProgressLabel(job, `${label} — retrying (${attempt + 1}/${backoffsMs.length})…`);
                await new Promise(resolve => setTimeout(resolve, backoffsMs[attempt]));
            } else {
                this.setProgressLabel(job, `${label}…`);
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

    // Background work while recording must not overwrite "Recording…".
    setProgressLabel(job, text) {
        if (job && job.closed && job === this.job) this.setStatusLabel(text);
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
        if (!this.job || !this.job.closed) return;
        this.pump(this.job);
    }

    discardServerUploads(job) {
        for (const segment of job.segments) {
            if (segment.text !== null) continue; // completed uploads are already gone
            fetch(`/api/voice/uploads/${encodeURIComponent(`${job.id}-s${segment.index}`)}`, { method: 'DELETE' }).catch(() => {});
        }
    }

    discardPendingAudio() {
        const job = this.job;
        this.job = null;
        this.targetCallback = null;
        this.hideUploading();
        if (job) {
            job.cancelled = true;
            this.discardServerUploads(job);
            VoiceStore.remove(job.id).then(() => this.recoverPendingRecordings());
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
