/**
 * ArtifactViewer — overlay that shows an HTML artifact stored in OpenPoet.
 *
 * The artifact is HTML/JS written by an agent, so it is never inserted into
 * this page. It is loaded from /api/artifacts/:id/render into an iframe whose
 * sandbox allows scripts and nothing else: the frame runs in an opaque origin
 * (no cookies, no storage, no access to this page) and the response carries a
 * CSP that blocks all network access. This page only ever talks to the frame
 * through postMessage, and treats what comes back as untrusted.
 *
 * Annotations: the server appends a small bridge script to the artifact it
 * serves. The bridge reports where the user selected text or clicked an
 * element and draws the highlights of saved notes. The note itself is typed
 * here, in the OpenPoet page, and its text is never sent into the frame.
 *
 * Questions: a version may declare questions (the server reads them from the
 * artifact when it is published and this page fetches that list). A click on a
 * choice inside the frame only proposes an answer: it is checked against the
 * declared list, staged here, and saved when the user confirms in this page.
 * Comments and free-text answers are typed here and never enter the frame.
 *
 * Usage:
 *   window.artifactViewer.open(artifactId)             // current version
 *   window.artifactViewer.open(artifactId, {version})  // a specific version
 */
class ArtifactViewer {
    constructor() {
        this.overlay = document.getElementById('artifact-viewer-overlay');
        this.nameEl = document.getElementById('artifact-viewer-name');
        this.versionSelect = document.getElementById('artifact-viewer-version');
        this.frame = document.getElementById('artifact-viewer-frame');
        this.statusEl = document.getElementById('artifact-viewer-status');
        this.bannerEl = document.getElementById('artifact-viewer-banner');
        this.bannerTextEl = document.getElementById('artifact-viewer-banner-text');
        this.bannerBtn = document.getElementById('artifact-viewer-banner-open');
        this.downloadBtn = document.getElementById('artifact-viewer-download');
        this.fullscreenBtn = document.getElementById('artifact-viewer-fullscreen');
        this.closeBtn = document.getElementById('artifact-viewer-close');

        // Annotations
        this.annotateBtn = document.getElementById('artifact-viewer-annotate');
        this.panelToggleBtn = document.getElementById('artifact-viewer-panel-toggle');
        this.openCountEl = document.getElementById('artifact-viewer-open-count');
        this.panelEl = document.getElementById('artifact-viewer-panel');
        this.tabOpenBtn = document.getElementById('artifact-panel-tab-open');
        this.tabResolvedBtn = document.getElementById('artifact-panel-tab-resolved');
        this.countOpenEl = document.getElementById('artifact-panel-count-open');
        this.countResolvedEl = document.getElementById('artifact-panel-count-resolved');
        this.composerEl = document.getElementById('artifact-panel-composer');
        this.composerQuoteEl = document.getElementById('artifact-composer-quote');
        this.composerTextEl = document.getElementById('artifact-composer-text');
        this.composerSaveBtn = document.getElementById('artifact-composer-save');
        this.composerCancelBtn = document.getElementById('artifact-composer-cancel');
        this.hintEl = document.getElementById('artifact-panel-hint');
        this.listEl = document.getElementById('artifact-panel-list');
        this.sendBtn = document.getElementById('artifact-panel-send');

        // Questions and answers
        this.tabAnswersBtn = document.getElementById('artifact-panel-tab-answers');
        this.countAnswersEl = document.getElementById('artifact-panel-count-answers');
        this.answersEl = document.getElementById('artifact-panel-answers');
        this.confirmBtn = document.getElementById('artifact-panel-confirm');
        this.pendingBarEl = document.getElementById('artifact-viewer-pending');
        this.pendingTextEl = document.getElementById('artifact-viewer-pending-text');
        this.pendingReviewBtn = document.getElementById('artifact-viewer-pending-review');

        this._artifact = null;      // metadata from GET /api/artifacts/:id
        this._version = 0;          // version shown in the frame
        this._openToken = 0;        // guards against out-of-order async opens

        this._annotations = [];     // every annotation of the artifact, oldest first
        this._tab = 'open';         // which annotations the panel and the frame show
        this._annotate = false;     // annotate mode: clicks in the frame pick anchors
        this._bridgeReady = false;  // the frame's bridge announced itself
        this._bridgeMissing = false; // the frame loaded but no bridge announced itself
        this._bridgeTimer = null;
        this._orphaned = new Set(); // annotation ids the bridge could not place
        this._pendingAnchor = null; // anchor of the note being written
        this._activeAnnotation = 0;
        this._notice = null;        // {text, error} shown in the panel hint

        this._questions = [];       // questions the shown version declares (from the server, never from the frame)
        this._pending = new Map();  // question id -> {options, text, comment}: chosen but not confirmed yet
        this._activeQuestion = '';
        this._intentWindow = { at: 0, count: 0 }; // rate limit for proposals coming from the frame

        if (!this.overlay) return;

        this.tabAnswersBtn?.addEventListener('click', () => this._setTab('answers'));
        this.confirmBtn?.addEventListener('click', () => this._confirmAnswers());
        this.pendingReviewBtn?.addEventListener('click', () => this._reviewAnswers());

        this.annotateBtn?.addEventListener('click', () => this._setAnnotate(!this._annotate));
        this.panelToggleBtn?.addEventListener('click', () => this._setPanelOpen(this.panelEl.classList.contains('hidden')));
        this.tabOpenBtn?.addEventListener('click', () => this._setTab('open'));
        this.tabResolvedBtn?.addEventListener('click', () => this._setTab('resolved'));
        this.composerSaveBtn?.addEventListener('click', () => this._saveAnnotation());
        this.composerCancelBtn?.addEventListener('click', () => this._cancelComposer());
        this.sendBtn?.addEventListener('click', () => this._sendToAgent());
        this.composerTextEl?.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                e.preventDefault();
                this._saveAnnotation();
            }
        });
        window.addEventListener('message', (e) => this._onFrameMessage(e));

        this.closeBtn?.addEventListener('click', () => this.close());
        this.downloadBtn?.addEventListener('click', () => this._download());
        this.fullscreenBtn?.addEventListener('click', () => this._toggleFullscreen());
        this.bannerBtn?.addEventListener('click', () => {
            this._showVersion(this._artifact?.current_version);
            this._notifyURLStateChanged({ replace: true });
        });
        this.versionSelect?.addEventListener('change', () => {
            this._showVersion(Number(this.versionSelect.value));
            this._notifyURLStateChanged({ replace: true });
        });
        this.overlay.addEventListener('click', (e) => {
            if (e.target === this.overlay) this.close();
        });
        document.addEventListener('keydown', (e) => {
            if (e.key === 'Escape' && this.isOpen()) {
                e.stopPropagation();
                this._onEscape();
            }
        }, true);
    }

    isOpen() {
        return !!this.overlay && !this.overlay.classList.contains('hidden');
    }

    async open(artifactId, opts = {}) {
        if (!this.overlay || !artifactId) return;
        const token = ++this._openToken;
        this.overlay.classList.remove('hidden');
        this._setStatus('Loading artifact…');
        this.nameEl.textContent = 'Artifact';
        this._hideBanner();
        try {
            const artifact = await this._fetchArtifact(artifactId);
            if (token !== this._openToken) return;
            this._artifact = artifact;
            this.nameEl.textContent = artifact.title || 'Artifact';
            this._renderVersions();
            this._resetAnnotations();
            this._showVersion(opts.version || artifact.current_version);
            this._loadAnnotations();
            if (opts.updateURL !== false) this._notifyURLStateChanged({ replace: !!opts.replaceURL });
        } catch (e) {
            if (token !== this._openToken) return;
            console.error('ArtifactViewer: failed to open artifact', e);
            this._artifact = null;
            this._setStatus(/HTTP 404/.test(e?.message || '')
                ? 'This artifact does not exist in this OpenPoet instance. The link may be from another instance, or the artifact was removed.'
                : 'This artifact could not be loaded.');
        }
    }

    close(options = {}) {
        if (!this.overlay) return;
        this._openToken++;
        this.overlay.classList.add('hidden');
        this.overlay.classList.remove('is-fullscreen');
        // Unload the artifact so its scripts and timers stop.
        if (this.frame) this.frame.src = 'about:blank';
        this._artifact = null;
        this._version = 0;
        this._hideBanner();
        this._resetAnnotations();
        if (options.updateURL !== false) this._notifyURLStateChanged();
    }

    // Same contract as DocViewer.dismiss, so URL restoration can treat both alike.
    dismiss(options = {}) {
        if (this.isOpen()) this.close(options);
    }

    getURLState() {
        if (!this.isOpen() || !this._artifact) return null;
        // The version is part of the address only when it is not the current
        // one, so a plain artifact link always opens the latest.
        const version = this._version && this._version !== this._artifact.current_version ? this._version : null;
        return { kind: 'artifact', id: this._artifact.id, projectId: this._artifact.project_id || null, version };
    }

    /** Called by the events WebSocket when any artifact gets a new version. */
    async handleArtifactUpdated(data) {
        if (!this.isOpen() || !this._artifact || !data || data.artifact_id !== this._artifact.id) return;
        if (!(data.version > this._artifact.current_version)) return;
        const wasOnLatest = this._version === this._artifact.current_version;
        try {
            const artifact = await this._fetchArtifact(this._artifact.id);
            if (!this._artifact || artifact.id !== this._artifact.id) return;
            this._artifact = artifact;
            this.nameEl.textContent = artifact.title || 'Artifact';
            this._renderVersions();
            if (this.versionSelect) this.versionSelect.value = String(this._version);
            // Do not swap the page under the reader: offer the new version instead.
            this._updateBanner(wasOnLatest);
        } catch (e) {
            console.error('ArtifactViewer: failed to refresh artifact', e);
        }
    }

    async _fetchArtifact(artifactId) {
        const resp = await fetch(`/api/artifacts/${encodeURIComponent(artifactId)}`);
        if (!resp.ok) throw new Error(`artifact ${artifactId}: HTTP ${resp.status}`);
        return resp.json();
    }

    _renderVersions() {
        if (!this.versionSelect || !this._artifact) return;
        this.versionSelect.innerHTML = '';
        for (const version of this._artifact.versions || []) {
            const option = document.createElement('option');
            option.value = String(version.version);
            const when = version.created_at ? new Date(version.created_at).toLocaleString() : '';
            const note = version.change_note ? ` — ${version.change_note}` : '';
            option.textContent = `v${version.version}${when ? ' · ' + when : ''}${note}`;
            this.versionSelect.appendChild(option);
        }
    }

    _showVersion(version) {
        if (!this._artifact || !this.frame) return;
        const known = (this._artifact.versions || []).some(v => v.version === version);
        if (!known) version = this._artifact.current_version;
        this._version = version;
        if (this.versionSelect) this.versionSelect.value = String(version);
        this._setStatus('Loading artifact…');
        // A new document loads: its bridge has to announce itself again.
        this._bridgeReady = false;
        this._bridgeMissing = false;
        this._orphaned = new Set();
        this._cancelComposer();
        clearTimeout(this._bridgeTimer);
        // Questions belong to a version: forget the previous one's.
        this._questions = [];
        this._pending = new Map();
        this._activeQuestion = '';
        if (this._tab === 'answers') this._tab = 'open';
        this._loadQuestions();
        this.frame.onload = () => {
            // about:blank also fires load (on close); only act for a real page.
            if (this.frame.getAttribute('src') === 'about:blank') return;
            this._setStatus('');
            // The bridge announces itself as the document finishes parsing. If
            // it has not by now, the artifact's markup swallowed it.
            clearTimeout(this._bridgeTimer);
            this._bridgeTimer = setTimeout(() => {
                this._bridgeMissing = !this._bridgeReady;
                this._renderPanel();
            }, 1500);
        };
        this.frame.src = `/api/artifacts/${encodeURIComponent(this._artifact.id)}/render?v=${version}`;
        this._updateBanner(false);
        this._renderPanel();
    }

    _updateBanner(newVersionArrived) {
        if (!this._artifact) return this._hideBanner();
        const latest = this._artifact.current_version;
        if (this._version === latest) return this._hideBanner();
        if (!this.bannerEl) return;
        this.bannerTextEl.textContent = newVersionArrived
            ? `Version ${latest} was just published. You are viewing version ${this._version}.`
            : `You are viewing version ${this._version}. The latest is version ${latest}.`;
        this.bannerBtn.textContent = `Open version ${latest}`;
        this.bannerEl.classList.remove('hidden');
    }

    _hideBanner() {
        this.bannerEl?.classList.add('hidden');
    }

    _setStatus(message) {
        if (!this.statusEl) return;
        this.statusEl.textContent = message || '';
        this.statusEl.classList.toggle('hidden', !message);
    }

    _toggleFullscreen() {
        this.overlay.classList.toggle('is-fullscreen');
    }

    async _download() {
        if (!this._artifact || !this._version) return;
        try {
            const resp = await fetch(`/api/artifacts/${encodeURIComponent(this._artifact.id)}/versions/${this._version}`);
            if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
            const version = await resp.json();
            // Not text/html: this blob URL belongs to the OpenPoet origin, and a
            // browser that opens it instead of saving it (some in-app WebViews
            // ignore the download attribute) must not run the artifact there.
            const blob = new Blob([version.content || ''], { type: 'application/octet-stream' });
            const url = URL.createObjectURL(blob);
            const a = document.createElement('a');
            a.href = url;
            a.download = `${this._fileStem()}-v${this._version}.html`;
            document.body.appendChild(a);
            a.click();
            document.body.removeChild(a);
            setTimeout(() => URL.revokeObjectURL(url), 1000);
        } catch (e) {
            console.error('ArtifactViewer: download failed', e);
            window.app?.showToast?.('Error', 'Failed to download the artifact', 'error');
        }
    }

    _fileStem() {
        return (this._artifact?.title || 'artifact')
            .replace(/[^a-zA-Z0-9\s-]/g, '')
            .trim()
            .replace(/\s+/g, '-')
            .toLowerCase()
            .substring(0, 60) || 'artifact';
    }

    _notifyURLStateChanged(options = {}) {
        window.app?._onDocumentURLStateChanged?.(options);
    }

    // ---- Annotations ----

    /** Called by the events WebSocket when annotations of any artifact change. */
    handleAnnotationsChanged(data) {
        if (!this.isOpen() || !this._artifact || !data || data.artifact_id !== this._artifact.id) return;
        this._loadAnnotations();
    }

    _resetAnnotations() {
        this._annotations = [];
        this._tab = 'open';
        this._bridgeReady = false;
        this._bridgeMissing = false;
        this._orphaned = new Set();
        this._pendingAnchor = null;
        this._activeAnnotation = 0;
        this._notice = null;
        this._questions = [];
        this._pending = new Map();
        this._activeQuestion = '';
        clearTimeout(this._bridgeTimer);
        this._setAnnotate(false);
        this._setPanelOpen(false);
        this.composerEl?.classList.add('hidden');
        this._renderPanel();
    }

    async _api(method, path, body) {
        const resp = await fetch(path, {
            method,
            headers: body ? { 'Content-Type': 'application/json' } : undefined,
            body: body ? JSON.stringify(body) : undefined,
        });
        let data = null;
        try { data = await resp.json(); } catch {}
        if (!resp.ok) {
            const error = new Error(data?.error || `HTTP ${resp.status}`);
            error.status = resp.status;
            throw error;
        }
        return data;
    }

    _annotationsPath(suffix = '') {
        return `/api/artifacts/${encodeURIComponent(this._artifact.id)}/annotations${suffix}`;
    }

    async _loadAnnotations() {
        if (!this._artifact) return;
        const artifactId = this._artifact.id;
        try {
            const annotations = await this._api('GET', this._annotationsPath());
            if (!this._artifact || this._artifact.id !== artifactId) return;
            this._annotations = Array.isArray(annotations) ? annotations : [];
        } catch (e) {
            console.error('ArtifactViewer: failed to load annotations', e);
            return;
        }
        this._dropRedundantPending();
        this._renderPanel();
        this._pushToFrame();
    }

    _setAnnotate(enabled) {
        this._annotate = !!enabled;
        this.annotateBtn?.setAttribute('aria-pressed', String(this._annotate));
        if (this._annotate) {
            this._setPanelOpen(true);
        } else {
            this._cancelComposer();
        }
        this._renderPanel();
        this._pushToFrame();
    }

    _setPanelOpen(open) {
        if (!this.panelEl) return;
        this.panelEl.classList.toggle('hidden', !open);
        this.panelToggleBtn?.setAttribute('aria-pressed', String(open));
        this._renderPendingChrome(); // the "to confirm" bar gives way to the open panel
    }

    _setTab(tab) {
        this._tab = tab === 'resolved' || tab === 'answers' ? tab : 'open';
        this._notice = null;
        this._renderPanel();
        this._pushToFrame();
    }

    // The Answers tab keeps the open notes highlighted in the artifact.
    _visibleAnnotations() {
        return this._annotations.filter(a => (a.status === 'resolved') === (this._tab === 'resolved'));
    }

    _isAnswer(annotation) {
        return annotation.kind === 'answer';
    }

    // Annotations are numbered in creation order; the number is the same in the
    // panel and on the highlight inside the frame.
    _annotationNumber(annotation) {
        return this._annotations.findIndex(a => a.id === annotation.id) + 1;
    }

    _postToFrame(type, payload = {}) {
        const target = this.frame?.contentWindow;
        if (!target || !this._bridgeReady) return;
        // The frame has an opaque origin, so no target origin can be named.
        // Only anchors, ids and statuses are sent — never the text of a note.
        target.postMessage({ source: 'openpoet-artifact-host', type, ...payload }, '*');
    }

    _pushToFrame() {
        this._postToFrame('set-mode', { annotate: this._annotate });
        this._postToFrame('show-annotations', {
            // An answer has no place in the page to highlight.
            annotations: this._visibleAnnotations().filter(a => !this._isAnswer(a)).map(a => ({
                id: a.id, n: this._annotationNumber(a), status: a.status, anchor: a.anchor,
            })),
        });
        this._pushAnswersToFrame();
    }

    // Tells the frame which options are chosen, so the artifact can show them.
    // Only question and option ids go in: they are the artifact's own. What the
    // user typed (a comment, a free-text answer) stays in this page.
    _pushAnswersToFrame() {
        const answers = [];
        for (const question of this._questions) {
            const state = this._answerState(question);
            if (state.kind === 'none') continue;
            answers.push({ question: question.id, options: state.options.slice(), state: state.kind === 'pending' ? 'pending' : 'saved' });
        }
        this._postToFrame('answers', { answers });
    }

    // Messages from the frame come from agent-generated content (the bridge
    // shares the frame with the artifact's own scripts), so every field is
    // checked and nothing here is trusted to be what the bridge would send.
    _onFrameMessage(event) {
        if (!this.isOpen() || !this.frame || event.source !== this.frame.contentWindow) return;
        const data = event.data;
        if (!data || typeof data !== 'object' || data.source !== 'openpoet-artifact-bridge') return;
        switch (data.type) {
            case 'ready':
                this._bridgeReady = true;
                this._bridgeMissing = false;
                clearTimeout(this._bridgeTimer);
                this._renderPanel();
                this._pushToFrame();
                break;
            case 'selection': {
                // Once the user has typed something, the note stays attached to
                // the text they were looking at: a later "selection" (which the
                // artifact's own script can forge) must not re-point it.
                if (this._noteInProgress()) break;
                const anchor = ArtifactViewer.sanitizeAnchor(data.anchor);
                if (anchor && this._annotate) this._openComposer(anchor);
                break;
            }
            case 'annotation-click':
                if (Number.isInteger(data.id)) this._activateAnnotation(data.id, { fromFrame: true });
                break;
            case 'anchors':
                if (Array.isArray(data.orphaned)) {
                    this._orphaned = new Set(data.orphaned.filter(Number.isInteger).slice(0, 500));
                    this._renderPanel();
                }
                break;
            case 'answer-intent':
                this._onAnswerIntent(data);
                break;
            case 'key':
                // Escape pressed inside the frame. It may cancel an empty
                // composer or close the viewer, but never throws away text the
                // user typed: the frame cannot be trusted with that.
                if (data.key === 'Escape' && !this._noteInProgress()) this._onEscape();
                break;
        }
    }

    _noteInProgress() {
        return !!this.composerEl && !this.composerEl.classList.contains('hidden') &&
            (this.composerTextEl?.value || '').trim() !== '';
    }

    _onEscape() {
        if (this.composerEl && !this.composerEl.classList.contains('hidden')) {
            this._cancelComposer();
        } else if (this._annotate) {
            this._setAnnotate(false);
        } else {
            this.close();
        }
    }

    _openComposer(anchor) {
        this._pendingAnchor = anchor;
        this._notice = null;
        this._setPanelOpen(true);
        this.composerQuoteEl.textContent = anchor.type === 'text'
            ? anchor.exact
            : (anchor.label ? `<${anchor.tag || 'element'}> ${anchor.label}` : `<${anchor.tag || 'element'}>`);
        this.composerEl.classList.remove('hidden');
        this._renderPanel();
        this.composerTextEl.focus();
    }

    _cancelComposer() {
        if (!this.composerEl) return;
        this._pendingAnchor = null;
        this.composerEl.classList.add('hidden');
        if (this.composerTextEl) this.composerTextEl.value = '';
        this._postToFrame('clear-selection');
    }

    async _saveAnnotation() {
        const body = (this.composerTextEl?.value || '').trim();
        if (!this._artifact || !this._pendingAnchor) return;
        if (!body) {
            this.composerTextEl?.focus();
            return;
        }
        this.composerSaveBtn.disabled = true;
        try {
            await this._api('POST', this._annotationsPath(), {
                version: this._version, anchor: this._pendingAnchor, body,
            });
            this._cancelComposer();
            this._tab = 'open';
            await this._loadAnnotations();
        } catch (e) {
            console.error('ArtifactViewer: failed to save annotation', e);
            this._notice = { text: `The note could not be saved: ${e.message}`, error: true };
            this._renderPanel();
        } finally {
            this.composerSaveBtn.disabled = false;
        }
    }

    async _changeAnnotation(annotation, action) {
        try {
            await this._api('POST', this._annotationsPath(`/${annotation.id}/${action}`), {});
            await this._loadAnnotations();
        } catch (e) {
            console.error(`ArtifactViewer: failed to ${action} annotation`, e);
            this._notice = { text: `The annotation could not be updated: ${e.message}`, error: true };
            this._renderPanel();
        }
    }

    _activateAnnotation(id, { fromFrame = false } = {}) {
        const annotation = this._annotations.find(a => a.id === id);
        if (!annotation) return;
        this._activeAnnotation = id;
        const tab = annotation.status === 'resolved' ? 'resolved' : 'open';
        if (tab !== this._tab) {
            this._tab = tab;
            this._pushToFrame();
        }
        this._setPanelOpen(true);
        this._renderPanel();
        this.listEl?.querySelector(`[data-annotation-id="${id}"]`)?.scrollIntoView({ block: 'nearest' });
        if (fromFrame) return;
        if (this._isAnswer(annotation)) {
            this._postToFrame('focus-question', { question: annotation.question_id });
        } else {
            this._postToFrame('focus', { id });
        }
    }

    // Tells the session that published the artifact that there are notes to
    // address. The notes stay in the database either way; this is only a nudge.
    async _sendToAgent() {
        const artifact = this._artifact;
        const open = this._annotations.filter(a => a.status !== 'resolved').length;
        if (!artifact?.session_id) return;
        // Answers that were chosen but not confirmed are not saved: the agent
        // would not see them. Confirm first.
        if (this._pending.size > 0) {
            this._reviewAnswers();
            this._notice = { text: `Confirm your ${this._pending.size} pending answer${this._pending.size === 1 ? '' : 's'} first: only confirmed answers reach the agent.`, error: true };
            this._renderPanel();
            return;
        }
        if (open === 0) return;
        // This text is typed into the agent's terminal: keep the title to
        // printable characters on one line.
        const title = String(artifact.title || '').replace(/[\u0000-\u001f\u007f-\u009f]+/g, ' ').trim();
        const text = `The user left ${open} open annotation${open === 1 ? '' : 's'} on the HTML artifact "${title}" ` +
            `(artifact_id ${artifact.id}, current version ${artifact.current_version}). ` +
            `Read them with openpoet_list_artifact_annotations, publish a corrected version with openpoet_update_artifact, ` +
            `then resolve each one with openpoet_resolve_artifact_annotation, saying what you changed.`;
        this.sendBtn.disabled = true;
        try {
            await this._api('POST', `/api/sessions/${encodeURIComponent(artifact.session_id)}/input`, { text });
            this._notice = { text: `Sent to the session that published this artifact (${open} open).`, error: false };
        } catch (e) {
            this._notice = {
                text: 'The session that published this artifact is not accepting input right now. ' +
                    'The annotations stay open: an agent sees them when it reads this artifact.',
                error: true,
            };
        }
        this._renderPanel();
    }

    _renderPanel() {
        if (!this.listEl) return;
        const openCount = this._annotations.filter(a => a.status !== 'resolved').length;
        const resolvedCount = this._annotations.length - openCount;
        this.countOpenEl.textContent = String(openCount);
        this.countResolvedEl.textContent = String(resolvedCount);
        this.openCountEl.textContent = String(openCount);
        this.openCountEl.classList.toggle('hidden', openCount === 0);
        const hasQuestions = this._questions.length > 0;
        if (!hasQuestions && this._tab === 'answers') this._tab = 'open';
        for (const [btn, tab] of [[this.tabOpenBtn, 'open'], [this.tabResolvedBtn, 'resolved'], [this.tabAnswersBtn, 'answers']]) {
            if (!btn) continue;
            btn.classList.toggle('active', this._tab === tab);
            btn.setAttribute('aria-selected', String(this._tab === tab));
        }
        const onAnswers = this._tab === 'answers';
        this.tabAnswersBtn?.classList.toggle('hidden', !hasQuestions);
        if (this.countAnswersEl) {
            const answered = this._questions.filter(q => this._answerState(q).kind !== 'none').length;
            this.countAnswersEl.textContent = `${answered}/${this._questions.length}`;
        }
        this.listEl.classList.toggle('hidden', onAnswers);
        this.answersEl?.classList.toggle('hidden', !onAnswers);
        this._renderPendingChrome();

        // One line of guidance or status above the list.
        let hint = '';
        let hintError = false;
        if (this._notice) {
            hint = this._notice.text;
            hintError = this._notice.error;
        } else if (this._artifact && this._bridgeMissing) {
            hint = 'Annotations cannot be placed on this version: its markup kept the annotation layer from loading.';
            hintError = true;
        } else if (onAnswers && this._pending.size > 0) {
            hint = 'Answers marked "to confirm" are not saved yet. They reach the agent only after you confirm them here.';
        } else if (onAnswers) {
            hint = 'Choose here or tap an option in the artifact. Comments and free-text answers are typed here.';
        } else if (this._annotate && !this._pendingAnchor) {
            hint = 'Select text or click an element in the artifact to add a note.';
        }
        this.hintEl.textContent = hint;
        this.hintEl.classList.toggle('hidden', !hint);
        this.hintEl.classList.toggle('is-error', hintError);

        this.sendBtn.disabled = openCount === 0 || !this._artifact?.session_id;
        this.sendBtn.title = !this._artifact?.session_id
            ? 'This artifact was not published by a session'
            : 'Tell the session that published this artifact to address the open annotations';

        if (onAnswers) {
            this._renderAnswers();
            return;
        }
        this.listEl.innerHTML = '';
        const visible = this._visibleAnnotations();
        if (visible.length === 0) {
            const empty = document.createElement('div');
            empty.className = 'artifact-panel-empty';
            empty.textContent = this._tab === 'resolved'
                ? 'No resolved annotations.'
                : 'No open annotations. Turn on Annotate, then select text or click an element.';
            this.listEl.appendChild(empty);
            return;
        }
        for (const annotation of visible) {
            this.listEl.appendChild(this._renderAnnotation(annotation));
        }
    }

    // Built with DOM nodes and textContent only: quotes come from the artifact
    // and resolution notes from an agent.
    _renderAnnotation(annotation) {
        const resolved = annotation.status === 'resolved';
        const item = document.createElement('div');
        item.className = 'artifact-annotation' + (resolved ? ' is-resolved' : '') +
            (annotation.id === this._activeAnnotation ? ' is-active' : '');
        item.dataset.annotationId = String(annotation.id);
        item.tabIndex = 0;

        const el = (tag, className, text) => {
            const node = document.createElement(tag);
            if (className) node.className = className;
            if (text !== undefined) node.textContent = text;
            return node;
        };

        const head = el('div', 'artifact-annotation-head');
        head.appendChild(el('span', 'artifact-annotation-number', String(this._annotationNumber(annotation))));
        head.appendChild(el('span', 'artifact-annotation-tag', `v${annotation.version}`));
        const isAnswer = this._isAnswer(annotation);
        if (isAnswer) head.appendChild(el('span', 'artifact-annotation-tag', `answer · ${annotation.question_id}`));
        if (isAnswer ? !this._questions.some(q => q.id === annotation.question_id) : (this._bridgeReady && this._orphaned.has(annotation.id))) {
            const orphan = el('span', 'artifact-annotation-tag is-orphan', 'not in this version');
            orphan.title = isAnswer
                ? 'The version being shown does not ask this question'
                : 'The annotated text or element was not found in the version being shown';
            head.appendChild(orphan);
        }
        if (annotation.created_at) {
            head.appendChild(el('span', 'artifact-annotation-time', new Date(annotation.created_at).toLocaleString()));
        }
        item.appendChild(head);

        if (annotation.quote) item.appendChild(el('blockquote', 'artifact-annotation-quote', annotation.quote));
        if (isAnswer) {
            // Option labels were written by the agent: text only, like quotes.
            const chosen = (annotation.answer?.options || []).map(o => o.label || o.id).join(', ');
            if (chosen) item.appendChild(el('div', 'artifact-annotation-answer', `→ ${chosen}`));
            if (annotation.body) item.appendChild(el('div', 'artifact-annotation-body', annotation.body));
        } else {
            item.appendChild(el('div', 'artifact-annotation-body', annotation.body));
        }

        if (resolved && (annotation.resolution_note || annotation.resolved_by)) {
            const resolution = el('div', 'artifact-annotation-resolution');
            resolution.appendChild(el('strong', '', `Resolved${annotation.resolved_by ? ' by ' + annotation.resolved_by : ''}`));
            if (annotation.resolution_note) resolution.appendChild(document.createTextNode(annotation.resolution_note));
            item.appendChild(resolution);
        }

        const actions = el('div', 'artifact-annotation-actions');
        const action = el('button', 'btn btn-secondary btn-sm', resolved ? 'Reopen' : 'Resolve');
        action.type = 'button';
        action.addEventListener('click', (e) => {
            e.stopPropagation();
            this._changeAnnotation(annotation, resolved ? 'reopen' : 'resolve');
        });
        actions.appendChild(action);
        item.appendChild(actions);

        const activate = () => this._activateAnnotation(annotation.id);
        item.addEventListener('click', activate);
        item.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && e.target === item) activate();
        });
        return item;
    }

    // ---- Questions and answers ----

    async _loadQuestions() {
        if (!this._artifact || !this._version) return;
        const artifactId = this._artifact.id;
        const version = this._version;
        let questions = [];
        try {
            const view = await this._api('GET', `/api/artifacts/${encodeURIComponent(artifactId)}/questions?v=${version}`);
            questions = Array.isArray(view?.questions) ? view.questions : [];
        } catch (e) {
            console.error('ArtifactViewer: failed to load questions', e);
        }
        if (!this._artifact || this._artifact.id !== artifactId || this._version !== version) return;
        this._questions = questions;
        this._restorePending();
        this._renderPanel();
        this._pushAnswersToFrame();
    }

    _question(id) {
        return this._questions.find(q => q.id === id) || null;
    }

    _savedAnswer(questionId) {
        return this._annotations.find(a => this._isAnswer(a) && a.status !== 'resolved' && a.question_id === questionId) || null;
    }

    // What a question's answer is right now: a choice waiting for confirmation
    // ('pending'), the saved open answer ('saved'), or nothing ('none').
    _answerState(question) {
        const pending = this._pending.get(question.id);
        if (pending) return { kind: 'pending', options: pending.options, text: pending.text, comment: pending.comment };
        const saved = this._savedAnswer(question.id);
        if (!saved) return { kind: 'none', options: [], text: '', comment: '' };
        const declared = new Set((question.options || []).map(o => o.id));
        const options = (saved.answer?.options || []).map(o => o.id).filter(id => declared.has(id));
        const isText = question.type === 'text';
        return { kind: 'saved', options, text: isText ? saved.body || '' : '', comment: isText ? '' : saved.body || '', version: saved.version };
    }

    // Stages a change to one answer. Nothing is saved until the user confirms.
    _stage(questionId, change, { rerender = true } = {}) {
        const question = this._question(questionId);
        if (!question) return;
        const current = this._answerState(question);
        const next = {
            options: change.options !== undefined ? change.options : current.options.slice(),
            text: change.text !== undefined ? change.text : current.text,
            comment: change.comment !== undefined ? change.comment : current.comment,
        };
        this._pending.set(questionId, next);
        this._dropRedundantPending();
        this._savePending();
        if (rerender) this._renderPanel(); else this._renderPendingChrome();
        this._pushAnswersToFrame();
    }

    // A staged answer equal to the saved one (or empty with nothing saved) is
    // not a change.
    _dropRedundantPending() {
        for (const [questionId, pending] of [...this._pending]) {
            const question = this._question(questionId);
            if (!question) { this._pending.delete(questionId); continue; }
            const saved = this._savedAnswer(questionId);
            const savedOptions = saved ? (saved.answer?.options || []).map(o => o.id) : [];
            const savedBody = saved ? saved.body || '' : '';
            const pendingBody = (question.type === 'text' ? pending.text : pending.comment).trim();
            const sameOptions = pending.options.length === savedOptions.length && pending.options.every(id => savedOptions.includes(id));
            const empty = pending.options.length === 0 && pendingBody === '';
            if ((saved && sameOptions && pendingBody === savedBody.trim()) || (!saved && empty)) this._pending.delete(questionId);
        }
    }

    // Staged answers survive closing the viewer or reloading the page (they are
    // kept in this page's session storage, per artifact version).
    _pendingKey() {
        return this._artifact && this._version ? `openpoet.artifact.pending.${this._artifact.id}.v${this._version}` : '';
    }

    _savePending() {
        const key = this._pendingKey();
        if (!key) return;
        try {
            if (this._pending.size === 0) sessionStorage.removeItem(key);
            else sessionStorage.setItem(key, JSON.stringify([...this._pending]));
        } catch { /* storage unavailable: staged answers live for this view only */ }
    }

    _restorePending() {
        this._pending = new Map();
        const key = this._pendingKey();
        if (!key) return;
        let stored = [];
        try { stored = JSON.parse(sessionStorage.getItem(key) || '[]'); } catch { stored = []; }
        if (!Array.isArray(stored)) return;
        for (const entry of stored) {
            if (!Array.isArray(entry) || typeof entry[0] !== 'string') continue;
            const question = this._question(entry[0]);
            if (!question) continue;
            const declared = new Set((question.options || []).map(o => o.id));
            const value = entry[1] || {};
            this._pending.set(question.id, {
                options: Array.isArray(value.options) ? value.options.filter(id => declared.has(id)) : [],
                text: typeof value.text === 'string' ? value.text.slice(0, 4000) : '',
                comment: typeof value.comment === 'string' ? value.comment.slice(0, 4000) : '',
            });
        }
        this._dropRedundantPending();
    }

    // A choice clicked inside the frame. The message comes from agent-written
    // content, so it only ever selects among what the server says this version
    // declares, and it is staged, not saved: the user confirms in this page.
    _onAnswerIntent(data) {
        const now = Date.now();
        if (now - this._intentWindow.at > 1000) this._intentWindow = { at: now, count: 0 };
        if (++this._intentWindow.count > 40) return; // a script, not a person
        if (typeof data.question !== 'string' || data.question.length > 64) return;
        if (data.option !== undefined && typeof data.option !== 'string') return;
        const question = this._question(data.question);
        if (!question) return;
        const hasOption = typeof data.option === 'string' && data.option !== '';
        if (!hasOption || question.type === 'text') {
            // Something to type: that happens here, in the panel.
            this._reviewAnswers(question.id, { focusInput: true });
            return;
        }
        if (data.option.length > 64 || !(question.options || []).some(o => o.id === data.option)) return;
        const current = this._answerState(question).options;
        let options;
        if (question.type === 'multi') {
            options = current.includes(data.option) ? current.filter(id => id !== data.option) : [...current, data.option];
        } else {
            options = [data.option];
        }
        this._activeQuestion = question.id;
        // Only this question's card is brought up to date: rebuilding the
        // panel would take the cursor out of a comment being typed.
        this._stage(question.id, { options }, { rerender: false });
        this._syncQuestionCard(question.id);
    }

    _syncQuestionCard(questionId) {
        const question = this._question(questionId);
        const card = [...(this.answersEl?.children || [])].find(node => node.dataset?.questionId === questionId);
        if (!question || !card) return;
        const state = this._answerState(question);
        for (const input of card.querySelectorAll('.artifact-question-options input')) {
            input.checked = state.options.includes(input.value);
        }
        this._decorateQuestionCard(card, state);
    }

    // Opens the Answers tab, optionally on one question.
    _reviewAnswers(questionId = '', { focusInput = false } = {}) {
        if (this._questions.length === 0) return;
        if (questionId) this._activeQuestion = questionId;
        this._tab = 'answers';
        this._notice = null;
        this._setPanelOpen(true);
        this._renderPanel();
        this._pushToFrame();
        if (!questionId) return;
        const card = [...(this.answersEl?.children || [])].find(node => node.dataset?.questionId === questionId);
        card?.scrollIntoView({ block: 'nearest' });
        if (focusInput) card?.querySelector('textarea')?.focus();
    }

    // The "N answers to confirm" bar over the artifact and the Confirm button.
    _renderPendingChrome() {
        const pending = this._pending.size;
        if (this.pendingBarEl) {
            // The bar is the way into the panel; with the Answers tab already
            // open the Confirm button is in view.
            const panelOnAnswers = !this.panelEl.classList.contains('hidden') && this._tab === 'answers';
            this.pendingBarEl.classList.toggle('hidden', pending === 0 || panelOnAnswers);
            this.pendingTextEl.textContent = `${pending} answer${pending === 1 ? '' : 's'} to confirm`;
        }
        if (this.confirmBtn) {
            this.confirmBtn.classList.toggle('hidden', pending === 0);
            this.confirmBtn.textContent = `Confirm ${pending} answer${pending === 1 ? '' : 's'}`;
        }
        if (this.countAnswersEl) {
            const answered = this._questions.filter(q => this._answerState(q).kind !== 'none').length;
            this.countAnswersEl.textContent = `${answered}/${this._questions.length}`;
        }
        for (const card of this.answersEl?.children || []) {
            const question = this._question(card.dataset?.questionId || '');
            if (question) this._decorateQuestionCard(card, this._answerState(question));
        }
    }

    _decorateQuestionCard(card, state) {
        card.classList.toggle('is-pending', state.kind === 'pending');
        card.classList.toggle('is-active', card.dataset.questionId === this._activeQuestion);
        const chip = card.querySelector('.artifact-question-state');
        if (chip) {
            chip.textContent = state.kind === 'pending' ? 'to confirm' : (state.kind === 'saved' ? 'answered' : 'not answered');
            chip.classList.toggle('is-pending', state.kind === 'pending');
            chip.classList.toggle('is-answered', state.kind === 'saved');
        }
        const discard = card.querySelector('[data-action="discard"]');
        if (discard) discard.hidden = state.kind !== 'pending';
        const withdraw = card.querySelector('[data-action="withdraw"]');
        if (withdraw) withdraw.hidden = state.kind !== 'saved';
    }

    // The questions of this version as native controls of this page. Prompts
    // and labels were written by the agent: they are set as text.
    _renderAnswers() {
        if (!this.answersEl) return;
        this.answersEl.innerHTML = '';
        const el = (tag, className, text) => {
            const node = document.createElement(tag);
            if (className) node.className = className;
            if (text !== undefined) node.textContent = text;
            return node;
        };
        this._questions.forEach((question, index) => {
            const state = this._answerState(question);
            const card = el('div', 'artifact-question');
            card.dataset.questionId = question.id;

            const head = el('div', 'artifact-question-head');
            head.appendChild(el('span', 'artifact-question-id', question.id));
            head.appendChild(el('span', 'artifact-question-state'));
            card.appendChild(head);
            card.appendChild(el('div', 'artifact-question-prompt', question.prompt));

            if (question.type !== 'text') {
                const options = el('div', 'artifact-question-options');
                options.setAttribute('role', question.type === 'multi' ? 'group' : 'radiogroup');
                options.setAttribute('aria-label', question.prompt);
                for (const option of question.options || []) {
                    const label = el('label', 'artifact-question-option');
                    const input = document.createElement('input');
                    input.type = question.type === 'multi' ? 'checkbox' : 'radio';
                    input.name = `artifact-question-${index}`;
                    input.value = option.id;
                    input.checked = state.options.includes(option.id);
                    input.addEventListener('change', () => {
                        const chosen = [...options.querySelectorAll('input')].filter(i => i.checked).map(i => i.value);
                        this._activeQuestion = question.id;
                        this._stage(question.id, { options: chosen }, { rerender: false });
                    });
                    label.appendChild(input);
                    label.appendChild(el('span', '', option.label));
                    options.appendChild(label);
                }
                card.appendChild(options);
            }
            if (question.type === 'text' || question.comment) {
                const isText = question.type === 'text';
                const input = document.createElement('textarea');
                input.rows = 2;
                input.maxLength = 4000;
                input.placeholder = isText ? 'Your answer' : 'Comment (optional)';
                input.setAttribute('aria-label', isText ? `Answer to: ${question.prompt}` : `Comment on: ${question.prompt}`);
                input.value = isText ? state.text : state.comment;
                // Typing must not rebuild the panel under the cursor.
                input.addEventListener('input', () => {
                    this._activeQuestion = question.id;
                    this._stage(question.id, isText ? { text: input.value } : { comment: input.value }, { rerender: false });
                });
                card.appendChild(input);
            }

            const actions = el('div', 'artifact-question-actions');
            const discard = el('button', 'btn btn-secondary btn-sm', 'Discard change');
            discard.type = 'button';
            discard.dataset.action = 'discard';
            discard.addEventListener('click', () => {
                this._pending.delete(question.id);
                this._savePending();
                this._renderPanel();
                this._pushAnswersToFrame();
            });
            const withdraw = el('button', 'btn btn-secondary btn-sm', 'Withdraw answer');
            withdraw.type = 'button';
            withdraw.dataset.action = 'withdraw';
            withdraw.addEventListener('click', () => this._withdrawAnswer(question.id));
            actions.appendChild(discard);
            actions.appendChild(withdraw);
            card.appendChild(actions);

            this._decorateQuestionCard(card, state);
            this.answersEl.appendChild(card);
        });
    }

    // Saves every staged answer. This click, in this page, is what makes an
    // answer the user's: nothing the frame says is stored before it.
    async _confirmAnswers() {
        if (!this._artifact || this._pending.size === 0) return;
        const answers = [];
        for (const [questionId, pending] of this._pending) {
            const question = this._question(questionId);
            if (!question) continue;
            if (question.type === 'text') {
                if (!pending.text.trim()) return this._answerProblem(question, 'needs an answer');
                answers.push({ question: questionId, text: pending.text.trim() });
            } else {
                if (pending.options.length === 0) return this._answerProblem(question, 'needs an option');
                answers.push({ question: questionId, options: pending.options, comment: pending.comment.trim() });
            }
        }
        if (answers.length === 0) return;
        this.confirmBtn.disabled = true;
        try {
            await this._api('PUT', `/api/artifacts/${encodeURIComponent(this._artifact.id)}/answers`, { version: this._version, answers });
            this._pending = new Map();
            this._savePending();
            this._notice = { text: `${answers.length} answer${answers.length === 1 ? '' : 's'} saved. Use Send to agent to tell the session.`, error: false };
            await this._loadAnnotations();
        } catch (e) {
            console.error('ArtifactViewer: failed to save answers', e);
            this._notice = { text: `The answers could not be saved: ${e.message}`, error: true };
            this._renderPanel();
        } finally {
            this.confirmBtn.disabled = false;
        }
    }

    _answerProblem(question, problem) {
        this._activeQuestion = question.id;
        this._notice = { text: `Question ${question.id} ${problem}. Complete it or discard the change.`, error: true };
        this._renderPanel();
        [...(this.answersEl?.children || [])].find(node => node.dataset?.questionId === question.id)?.scrollIntoView({ block: 'nearest' });
    }

    async _withdrawAnswer(questionId) {
        if (!this._artifact) return;
        try {
            await this._api('DELETE', `/api/artifacts/${encodeURIComponent(this._artifact.id)}/answers/${encodeURIComponent(questionId)}`);
            this._pending.delete(questionId);
            this._savePending();
            await this._loadAnnotations();
        } catch (e) {
            console.error('ArtifactViewer: failed to withdraw answer', e);
            this._notice = { text: `The answer could not be withdrawn: ${e.message}`, error: true };
            this._renderPanel();
        }
    }
}

/**
 * Returns a bounded copy of an anchor reported by the frame, or null when it
 * is not a usable anchor. The server validates again; this keeps junk from
 * reaching the composer.
 */
ArtifactViewer.sanitizeAnchor = function (anchor) {
    if (!anchor || typeof anchor !== 'object') return null;
    const text = (value, limit) => (typeof value === 'string' ? value.slice(0, limit) : '');
    const selector = text(anchor.selector, 1000);
    if (anchor.type === 'text') {
        const exact = text(anchor.exact, 2000);
        if (!exact.trim()) return null;
        const clean = { type: 'text', selector, exact, prefix: text(anchor.prefix, 200), suffix: text(anchor.suffix, 200) };
        if (Number.isInteger(anchor.start) && Number.isInteger(anchor.end) && anchor.start >= 0 && anchor.end >= anchor.start) {
            clean.start = anchor.start;
            clean.end = anchor.end;
        }
        return clean;
    }
    if (anchor.type === 'element') {
        if (!selector) return null;
        return { type: 'element', selector, tag: text(anchor.tag, 40).toLowerCase(), label: text(anchor.label, 300) };
    }
    return null;
};

/** Ids of this form in document listings, cards and URLs are artifacts. */
ArtifactViewer.ID_PREFIX = 'artifact:';

document.addEventListener('DOMContentLoaded', () => {
    window.artifactViewer = new ArtifactViewer();
});
