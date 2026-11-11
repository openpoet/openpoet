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

        this._artifact = null;      // metadata from GET /api/artifacts/:id
        this._version = 0;          // version shown in the frame
        this._openToken = 0;        // guards against out-of-order async opens

        if (!this.overlay) return;

        this.closeBtn?.addEventListener('click', () => this.close());
        this.downloadBtn?.addEventListener('click', () => this._download());
        this.fullscreenBtn?.addEventListener('click', () => this._toggleFullscreen());
        this.bannerBtn?.addEventListener('click', () => this._showVersion(this._artifact?.current_version));
        this.versionSelect?.addEventListener('change', () => this._showVersion(Number(this.versionSelect.value)));
        this.overlay.addEventListener('click', (e) => {
            if (e.target === this.overlay) this.close();
        });
        document.addEventListener('keydown', (e) => {
            if (e.key === 'Escape' && this.isOpen()) {
                e.stopPropagation();
                this.close();
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
            this._showVersion(opts.version || artifact.current_version);
            if (opts.updateURL !== false) this._notifyURLStateChanged({ replace: !!opts.replaceURL });
        } catch (e) {
            if (token !== this._openToken) return;
            console.error('ArtifactViewer: failed to open artifact', e);
            this._artifact = null;
            this._setStatus('This artifact could not be loaded.');
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
        if (options.updateURL !== false) this._notifyURLStateChanged();
    }

    // Same contract as DocViewer.dismiss, so URL restoration can treat both alike.
    dismiss(options = {}) {
        if (this.isOpen()) this.close(options);
    }

    getURLState() {
        if (!this.isOpen() || !this._artifact) return null;
        return { kind: 'artifact', id: this._artifact.id, projectId: this._artifact.project_id || null };
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
        this.frame.onload = () => {
            // about:blank also fires load (on close); only clear for a real page.
            if (this.frame.getAttribute('src') !== 'about:blank') this._setStatus('');
        };
        this.frame.src = `/api/artifacts/${encodeURIComponent(this._artifact.id)}/render?v=${version}`;
        this._updateBanner(false);
        this._onVersionShown?.();
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
            const blob = new Blob([version.content || ''], { type: 'text/html;charset=utf-8' });
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
}

/** Ids of this form in document listings, cards and URLs are artifacts. */
ArtifactViewer.ID_PREFIX = 'artifact:';

document.addEventListener('DOMContentLoaded', () => {
    window.artifactViewer = new ArtifactViewer();
});
