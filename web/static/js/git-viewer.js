// Git Viewer - displays git log, graph, diffs, and branches in a full-screen overlay
class GitViewer {
    static GRAPH_COLORS = [
        '#7aa2f7', '#9ece6a', '#f7768e', '#e0af68',
        '#bb9af7', '#7dcfff', '#ff9e64', '#c0caf5',
    ];
    static COL_WIDTH = 20;
    static ROW_HEIGHT = 36;

    constructor() {
        this.overlay = document.getElementById('git-viewer-overlay');
        this.projectNameEl = document.getElementById('git-viewer-project-name');
        this.tabs = this.overlay?.querySelectorAll('.git-tab');
        this.panels = {
            log: document.getElementById('git-panel-log'),
            diff: document.getElementById('git-panel-diff'),
            status: document.getElementById('git-panel-status'),
            branches: document.getElementById('git-panel-branches'),
        };
        this.branchToggle = document.getElementById('git-branch-toggle');
        this.branchLabel = document.getElementById('git-branch-label');
        this.branchMenu = document.getElementById('git-branch-menu');
        this.searchInput = document.getElementById('git-search');
        this.logList = document.getElementById('git-log-list');
        this.diffFiles = document.getElementById('git-diff-files');
        this.diffTitle = document.getElementById('git-diff-title');
        this.diffStats = document.getElementById('git-diff-stats-summary');
        this.branchesContent = document.getElementById('git-branches-content');
        this.statusContent = document.getElementById('git-status-content');
        this.statusBranch = document.getElementById('git-status-branch');
        this.commitBox = document.getElementById('git-commit-box');
        this.commitMessage = document.getElementById('git-commit-message');
        this.commitBtn = document.getElementById('git-commit-btn');
        this.commitAiBtn = document.getElementById('git-commit-ai');
        this.commitVoiceBtn = document.getElementById('git-commit-voice');
        this.commitExpandBtn = document.getElementById('git-commit-expand');
        this.compareBar = document.getElementById('git-compare-bar');
        this.compareFrom = document.getElementById('git-compare-from');
        this.compareTo = document.getElementById('git-compare-to');

        this.currentProjectId = null;
        this.currentTab = 'log';
        this.commits = [];
        this.branches = { local: [], remote: [], current: '' };
        this.selectedBranches = new Set(); // empty = all branches
        this.page = 0;
        this.hasMore = false;
        this.loading = false;
        this.selectedCommitHash = null;
        this.maxGraphCols = 0;
        this._searchTimeout = null;

        this.setupEventListeners();
    }

    setupEventListeners() {
        // Tab switching
        this.tabs?.forEach(tab => {
            tab.addEventListener('click', () => this.switchTab(tab.dataset.tab));
        });

        // Close button
        document.getElementById('git-viewer-close')?.addEventListener('click', () => this.hide());

        // Escape key
        document.addEventListener('keydown', (e) => {
            if (e.key === 'Escape' && this.overlay && !this.overlay.classList.contains('hidden')) {
                // Close branch menu first if open
                if (this.branchMenu && !this.branchMenu.classList.contains('hidden')) {
                    this.branchMenu.classList.add('hidden');
                    return;
                }
                this.hide();
            }
        });

        // Branch dropdown toggle
        this.branchToggle?.addEventListener('click', (e) => {
            e.stopPropagation();
            this.branchMenu?.classList.toggle('hidden');
        });

        // Close dropdown when clicking outside
        document.addEventListener('click', (e) => {
            if (this.branchMenu && !this.branchMenu.classList.contains('hidden')) {
                if (!e.target.closest('.git-branch-dropdown')) {
                    this.branchMenu.classList.add('hidden');
                }
            }
        });

        // Status tab buttons
        document.getElementById('git-status-refresh')?.addEventListener('click', () => this.loadStatus());
        this.commitBtn?.addEventListener('click', () => this.doCommit());
        this.commitAiBtn?.addEventListener('click', () => this.generateCommitMessage());

        // Expand/collapse commit message textarea
        this.commitExpandBtn?.addEventListener('click', () => this.toggleCommitExpand());

        // Expanded header buttons — wire to same actions
        document.getElementById('git-commit-collapse')?.addEventListener('click', () => this.toggleCommitExpand());
        this.commitBox?.querySelector('.git-commit-btn-exp')?.addEventListener('click', () => this.doCommit());
        this.commitBox?.querySelector('.git-commit-ai-exp')?.addEventListener('click', () => this.generateCommitMessage());

        // Voice input handler (shared between collapsed and expanded)
        const handleVoice = (btn) => {
            if (!window.voiceInput) return;
            if (window.voiceInput.isRecording) {
                window.voiceInput.stopRecording();
                btn.classList.remove('recording');
                return;
            }
            btn.classList.add('recording');
            window.voiceInput.startRecordingWithCallback((text) => {
                btn.classList.remove('recording');
                const ta = this.commitMessage;
                if (ta) {
                    ta.value = (ta.value ? ta.value + ' ' : '') + text;
                    ta.focus();
                }
            });
        };
        this.commitVoiceBtn?.addEventListener('click', (e) => { e.preventDefault(); e.stopPropagation(); handleVoice(e.currentTarget); });
        this.commitBox?.querySelector('.git-commit-voice-exp')?.addEventListener('click', (e) => { e.preventDefault(); e.stopPropagation(); handleVoice(e.currentTarget); });

        // Search with debounce
        this.searchInput?.addEventListener('input', () => {
            clearTimeout(this._searchTimeout);
            this._searchTimeout = setTimeout(() => {
                this.page = 0;
                this.commits = [];
                this.logList.innerHTML = '';
                this.loadLog();
            }, 300);
        });

        // Infinite scroll
        this.logList?.addEventListener('scroll', () => {
            if (this.loading || !this.hasMore) return;
            const el = this.logList;
            if (el.scrollTop + el.clientHeight >= el.scrollHeight - 100) {
                this.page++;
                this.loadLog(true);
            }
        });
    }

    // --- Open / Close ---

    open(projectId) {
        this.currentProjectId = projectId;
        this.page = 0;
        this.commits = [];
        this.hasMore = false;
        this.graphState = '';
        this.selectedCommitHash = null;
        this.selectedBranches = new Set(); // default: all

        // Set project name
        const project = window.app?.projects?.find(p => p.id === projectId);
        if (this.projectNameEl) {
            this.projectNameEl.textContent = project?.name || 'Git';
        }

        this.overlay?.classList.remove('hidden');
        this.switchTab('log');
        this.logList.innerHTML = '<div class="git-loading"><div class="spinner"></div><span>Loading...</span></div>';
        this.loadBranches().then(() => this.loadLog());
    }

    hide() {
        this.overlay?.classList.add('hidden');
        this.branchMenu?.classList.add('hidden');
        // Restore body scroll if commit editor was expanded
        if (this.commitBox?.classList.contains('expanded')) {
            this.commitBox.classList.remove('expanded');
            this.panels.status?.classList.remove('commit-expanded');
            document.body.style.overflow = '';
            document.documentElement.style.overflow = '';
        }
    }

    switchTab(tab) {
        this.currentTab = tab;
        this.tabs?.forEach(t => t.classList.toggle('active', t.dataset.tab === tab));
        Object.entries(this.panels).forEach(([key, panel]) => {
            panel?.classList.toggle('hidden', key !== tab);
        });
        if (tab === 'status') this.loadStatus();
        // Hide compare bar when not explicitly comparing
        if (tab !== 'diff') {
            this.compareBar?.classList.add('hidden');
        }
    }

    // --- API calls ---

    async loadBranches() {
        try {
            const resp = await fetch(`/api/projects/${this.currentProjectId}/git/branches`);
            if (!resp.ok) {
                const err = await resp.json();
                throw new Error(err.error || 'Failed to load branches');
            }
            this.branches = await resp.json();
            this.renderBranchFilter();
            this.renderBranchesPanel();
        } catch (e) {
            console.error('git branches error:', e);
        }
    }

    async loadLog(append = false) {
        if (this.loading) return;
        this.loading = true;

        if (!append) {
            this.logList.innerHTML = '<div class="git-loading"><div class="spinner"></div><span>Loading...</span></div>';
        }

        try {
            const params = new URLSearchParams({
                page: this.page,
                limit: 50,
            });

            // Pass graph state continuation for paginated graph
            if (append && this.graphState) {
                params.set('graph_state', this.graphState);
            }

            // Multi-branch support: if none selected, use "all"
            if (this.selectedBranches.size === 0) {
                params.set('branch', 'all');
            } else {
                for (const b of this.selectedBranches) {
                    params.append('branch', b);
                }
            }

            const search = this.searchInput?.value || '';
            if (search) params.set('search', search);

            const resp = await fetch(`/api/projects/${this.currentProjectId}/git/log?${params}`);
            if (!resp.ok) {
                const err = await resp.json();
                throw new Error(err.error || 'Failed to load log');
            }
            const data = await resp.json();

            if (!append) {
                this.commits = data.commits || [];
                this.logList.innerHTML = '';
            } else {
                this.commits = this.commits.concat(data.commits || []);
            }

            this.hasMore = data.has_more;
            this.graphState = data.graph_state || '';

            // Calculate max graph columns
            if (!append) this.maxGraphCols = 0;
            for (const c of (data.commits || [])) {
                for (const line of (c.graph?.lines || [])) {
                    this.maxGraphCols = Math.max(this.maxGraphCols, line.from + 1, line.to + 1);
                }
                this.maxGraphCols = Math.max(this.maxGraphCols, (c.graph?.column || 0) + 1);
            }

            this.renderLogRows(data.commits || [], append);

            if (this.commits.length === 0) {
                this.logList.innerHTML = '<div class="git-empty">No commits found</div>';
            }
        } catch (e) {
            if (!append) {
                this.logList.innerHTML = `<div class="git-error">${this.escapeHtml(e.message)}</div>`;
            }
        } finally {
            this.loading = false;
        }
    }

    async loadDiff(ref) {
        this.selectedCommitHash = ref;
        this.compareBar?.classList.add('hidden');
        this.switchTab('diff');

        const commit = this.commits.find(c => c.hash === ref);
        if (this.diffTitle) {
            this.diffTitle.textContent = commit
                ? `${commit.short_hash} — ${commit.message}`
                : ref.substring(0, 7);
        }
        this.diffFiles.innerHTML = '<div class="git-loading"><div class="spinner"></div><span>Loading diff...</span></div>';

        try {
            const resp = await fetch(`/api/projects/${this.currentProjectId}/git/diff?ref=${encodeURIComponent(ref)}`);
            if (!resp.ok) {
                const err = await resp.json();
                throw new Error(err.error || 'Failed to load diff');
            }
            const data = await resp.json();

            if (this.diffStats) {
                this.diffStats.textContent = `${data.stats.files_changed} files, +${data.stats.additions} −${data.stats.deletions}`;
            }

            this.renderDiff(data.files);
        } catch (e) {
            this.diffFiles.innerHTML = `<div class="git-error">${this.escapeHtml(e.message)}</div>`;
        }
    }

    // --- Branch selection ---

    _onBranchToggle(branchName) {
        if (this.selectedBranches.has(branchName)) {
            this.selectedBranches.delete(branchName);
        } else {
            this.selectedBranches.add(branchName);
        }
        this._updateBranchLabel();
        this._updateBranchCheckboxes();
        this._reloadLog();
    }

    _onSelectAll() {
        // If all are selected (or selectedBranches is empty = "all"), toggle to none
        const allBranches = this._allBranchNames();
        if (this.selectedBranches.size === 0) {
            // Currently "all" — can't deselect all, so no-op
            return;
        }
        // Clear selection = show all
        this.selectedBranches.clear();
        this._updateBranchLabel();
        this._updateBranchCheckboxes();
        this._reloadLog();
    }

    _allBranchNames() {
        return [
            ...this.branches.local.map(b => b.name),
            ...this.branches.remote.map(b => b.name),
        ];
    }

    _updateBranchLabel() {
        if (!this.branchLabel) return;
        if (this.selectedBranches.size === 0) {
            this.branchLabel.textContent = 'All branches';
        } else if (this.selectedBranches.size === 1) {
            this.branchLabel.textContent = [...this.selectedBranches][0];
        } else {
            this.branchLabel.textContent = `${this.selectedBranches.size} branches`;
        }
    }

    _updateBranchCheckboxes() {
        if (!this.branchMenu) return;
        const allChecked = this.selectedBranches.size === 0;
        this.branchMenu.querySelectorAll('.git-branch-menu-item input[type="checkbox"]').forEach(cb => {
            if (cb.dataset.branch === '__all__') {
                cb.checked = allChecked;
            } else {
                cb.checked = allChecked || this.selectedBranches.has(cb.dataset.branch);
            }
        });
    }

    _reloadLog() {
        this.page = 0;
        this.commits = [];
        this.graphState = '';
        this.logList.innerHTML = '';
        this.loadLog();
    }

    // --- Rendering: Log ---

    renderLogRows(commits, append) {
        const fragment = document.createDocumentFragment();
        const graphWidth = Math.max(this.maxGraphCols * GitViewer.COL_WIDTH + 10, 30);

        for (const commit of commits) {
            const row = document.createElement('div');
            row.className = 'git-commit-row';
            if (commit.hash === this.selectedCommitHash) row.classList.add('selected');
            row.addEventListener('click', () => this.loadDiff(commit.hash));

            // Graph SVG
            const svg = this.renderGraphCell(commit, graphWidth);
            row.appendChild(svg);

            // Info
            const info = document.createElement('div');
            info.className = 'git-commit-info';

            // Hash
            const hash = document.createElement('span');
            hash.className = 'git-commit-hash';
            hash.textContent = commit.short_hash;
            hash.title = commit.hash;
            info.appendChild(hash);

            // Refs
            if (commit.refs && commit.refs.length > 0) {
                const refsEl = document.createElement('span');
                refsEl.className = 'git-commit-refs';
                for (const ref of commit.refs) {
                    const badge = document.createElement('span');
                    badge.className = 'git-ref-badge ' + this.getRefClass(ref);
                    badge.textContent = ref.replace('HEAD -> ', '');
                    refsEl.appendChild(badge);
                }
                info.appendChild(refsEl);
            }

            // Message
            const msg = document.createElement('span');
            msg.className = 'git-commit-message';
            msg.textContent = commit.message;
            msg.title = commit.message;
            info.appendChild(msg);

            // Author
            const author = document.createElement('span');
            author.className = 'git-commit-author';
            author.textContent = commit.author_name;
            info.appendChild(author);

            // Date
            const date = document.createElement('span');
            date.className = 'git-commit-date';
            date.textContent = this.relativeDate(commit.date);
            date.title = commit.date;
            info.appendChild(date);

            row.appendChild(info);
            fragment.appendChild(row);
        }
        this.logList.appendChild(fragment);
    }

    getRefClass(ref) {
        if (ref.startsWith('HEAD')) return 'head';
        if (ref.startsWith('tag:')) return 'tag';
        if (ref.includes('/')) return 'remote';
        return 'branch';
    }

    // --- Rendering: Graph ---

    renderGraphCell(commit, width) {
        const ns = 'http://www.w3.org/2000/svg';
        const svg = document.createElementNS(ns, 'svg');
        svg.setAttribute('class', 'git-graph-cell');
        svg.setAttribute('width', width);
        svg.setAttribute('height', GitViewer.ROW_HEIGHT);

        const CW = GitViewer.COL_WIDTH;
        const midY = GitViewer.ROW_HEIGHT / 2;
        const colors = GitViewer.GRAPH_COLORS;
        const lines = commit.graph?.lines || [];

        // Draw lines
        for (const line of lines) {
            const x1 = line.from * CW + CW / 2;
            const x2 = line.to * CW + CW / 2;
            const color = colors[line.color % colors.length];
            const startY = line.half === 'bottom' ? midY : 0;
            const endY = line.half === 'top' ? midY : GitViewer.ROW_HEIGHT;

            if (line.from === line.to) {
                // Straight vertical line
                const el = document.createElementNS(ns, 'line');
                el.setAttribute('x1', x1);
                el.setAttribute('y1', startY);
                el.setAttribute('x2', x2);
                el.setAttribute('y2', endY);
                el.setAttribute('stroke', color);
                el.setAttribute('stroke-width', '2');
                svg.appendChild(el);
            } else {
                // Curved line (branch/merge/fork)
                const spanY = endY - startY;
                const cpY = startY + spanY * 0.5;
                const path = document.createElementNS(ns, 'path');
                path.setAttribute('d', `M ${x1},${startY} C ${x1},${cpY} ${x2},${cpY} ${x2},${endY}`);
                path.setAttribute('stroke', color);
                path.setAttribute('stroke-width', '2');
                path.setAttribute('fill', 'none');
                svg.appendChild(path);
            }
        }

        // Draw commit dot
        const col = commit.graph?.column || 0;
        const cx = col * CW + CW / 2;
        const commitColorIdx = lines.find(l => l.from === col || l.to === col)?.color || 0;
        const dotColor = colors[commitColorIdx % colors.length];

        const circle = document.createElementNS(ns, 'circle');
        circle.setAttribute('cx', cx);
        circle.setAttribute('cy', midY);
        circle.setAttribute('r', '4');
        circle.setAttribute('fill', dotColor);
        circle.setAttribute('stroke', 'var(--color-bg, #1a1b26)');
        circle.setAttribute('stroke-width', '1.5');
        svg.appendChild(circle);

        return svg;
    }

    // --- Rendering: Diff ---

    renderDiff(files) {
        this.diffFiles.innerHTML = '';

        if (files.length === 0) {
            this.diffFiles.innerHTML = '<div class="git-empty">No changes in this commit</div>';
            return;
        }

        for (const file of files) {
            const section = document.createElement('div');
            section.className = 'diff-file-section';

            // Header
            const header = document.createElement('div');
            header.className = 'diff-file-header';

            const statusChar = { added: 'A', deleted: 'D', modified: 'M', renamed: 'R' }[file.status] || 'M';
            header.innerHTML = `
                <span class="diff-file-status ${file.status}">${statusChar}</span>
                <span class="diff-file-path">${this.escapeHtml(file.old_path ? file.old_path + ' → ' + file.path : file.path)}</span>
                <span class="diff-file-stats-inline">
                    <span class="additions">+${file.additions}</span>
                    <span class="deletions">−${file.deletions}</span>
                </span>
                <span class="diff-file-toggle">▼</span>
            `;

            const body = document.createElement('div');
            body.className = 'diff-table-wrapper';

            if (file.binary) {
                body.innerHTML = '<div class="diff-binary-notice">Binary file changed</div>';
            } else {
                body.appendChild(this.renderDiffTable(file));
            }

            // Toggle collapse
            header.addEventListener('click', () => {
                const toggle = header.querySelector('.diff-file-toggle');
                const isHidden = body.style.display === 'none';
                body.style.display = isHidden ? '' : 'none';
                toggle?.classList.toggle('collapsed', !isHidden);
            });

            section.appendChild(header);
            section.appendChild(body);
            this.diffFiles.appendChild(section);
        }
    }

    renderDiffTable(file) {
        const table = document.createElement('table');
        table.className = 'diff-table';

        for (const hunk of (file.hunks || [])) {
            // Hunk header row
            const hunkRow = document.createElement('tr');
            hunkRow.className = 'diff-line-hunk';
            hunkRow.innerHTML = `<td colspan="3">${this.escapeHtml(hunk.header)}</td>`;
            table.appendChild(hunkRow);

            for (const line of (hunk.lines || [])) {
                const tr = document.createElement('tr');
                tr.className = `diff-line-${line.type}`;

                const oldTd = document.createElement('td');
                oldTd.className = 'line-num';
                oldTd.textContent = line.old_num || '';

                const newTd = document.createElement('td');
                newTd.className = 'line-num';
                newTd.textContent = line.new_num || '';

                const contentTd = document.createElement('td');
                contentTd.className = 'line-content';
                const prefix = line.type === 'addition' ? '+' : line.type === 'deletion' ? '-' : ' ';
                contentTd.textContent = prefix + line.content;

                tr.appendChild(oldTd);
                tr.appendChild(newTd);
                tr.appendChild(contentTd);
                table.appendChild(tr);
            }
        }

        return table;
    }

    // --- Rendering: Branches ---

    renderBranchFilter() {
        if (!this.branchMenu) return;

        let html = '';

        // "All branches" toggle
        html += `<label class="git-branch-menu-item">
            <input type="checkbox" data-branch="__all__" checked>
            <span>All branches</span>
        </label>`;
        html += '<div class="git-branch-menu-divider"></div>';

        if (this.branches.local.length > 0) {
            html += '<div class="git-branch-menu-section">Local</div>';
            for (const b of this.branches.local) {
                const cls = b.is_head ? ' current' : '';
                html += `<label class="git-branch-menu-item${cls}">
                    <input type="checkbox" data-branch="${this.escapeHtml(b.name)}" checked>
                    <span>${b.is_head ? '● ' : ''}${this.escapeHtml(b.name)}</span>
                </label>`;
            }
        }

        if (this.branches.remote.length > 0) {
            html += '<div class="git-branch-menu-divider"></div>';
            html += '<div class="git-branch-menu-section">Remote</div>';
            for (const b of this.branches.remote) {
                html += `<label class="git-branch-menu-item">
                    <input type="checkbox" data-branch="${this.escapeHtml(b.name)}" checked>
                    <span>${this.escapeHtml(b.name)}</span>
                </label>`;
            }
        }

        this.branchMenu.innerHTML = html;

        // Wire up checkbox handlers
        this.branchMenu.querySelectorAll('input[type="checkbox"]').forEach(cb => {
            cb.addEventListener('change', (e) => {
                e.stopPropagation();
                const branch = cb.dataset.branch;
                if (branch === '__all__') {
                    // Toggle all: if checked, clear selection (= all)
                    if (cb.checked) {
                        this.selectedBranches.clear();
                    }
                    // Can't uncheck "all" directly — user should uncheck individual branches
                    this._updateBranchLabel();
                    this._updateBranchCheckboxes();
                    this._reloadLog();
                } else {
                    if (!cb.checked) {
                        // Unchecking a branch: if we were in "all" mode, switch to explicit selection
                        if (this.selectedBranches.size === 0) {
                            // Populate with all branches except the unchecked one
                            for (const name of this._allBranchNames()) {
                                if (name !== branch) {
                                    this.selectedBranches.add(name);
                                }
                            }
                        } else {
                            this.selectedBranches.delete(branch);
                        }
                    } else {
                        // Checking a branch
                        if (this.selectedBranches.size === 0) {
                            // Already all — no-op
                            return;
                        }
                        this.selectedBranches.add(branch);
                        // If all branches now selected, switch back to "all" mode
                        if (this.selectedBranches.size >= this._allBranchNames().length) {
                            this.selectedBranches.clear();
                        }
                    }
                    this._updateBranchLabel();
                    this._updateBranchCheckboxes();
                    this._reloadLog();
                }
            });
        });

        this._updateBranchLabel();
    }

    renderBranchesPanel() {
        if (!this.branchesContent) return;
        let html = '';

        if (this.branches.local.length > 0) {
            html += '<div class="git-branches-section-title">Local</div>';
            for (const b of this.branches.local) {
                const cls = b.is_head ? ' current' : '';
                html += `<div class="git-branch-item${cls}" data-branch="${this.escapeHtml(b.name)}">
                    <span>${b.is_head ? '● ' : ''}${this.escapeHtml(b.name)}</span>
                    <span class="branch-hash">${this.escapeHtml(b.hash)}</span>
                </div>`;
            }
        }

        if (this.branches.remote.length > 0) {
            html += '<div class="git-branches-section-title">Remote</div>';
            for (const b of this.branches.remote) {
                html += `<div class="git-branch-item" data-branch="${this.escapeHtml(b.name)}">
                    <span>${this.escapeHtml(b.name)}</span>
                    <span class="branch-hash">${this.escapeHtml(b.hash)}</span>
                </div>`;
            }
        }

        if (this.branches.tags && this.branches.tags.length > 0) {
            html += '<div class="git-branches-section-title">Tags</div>';
            for (const t of this.branches.tags) {
                html += `<div class="git-branch-item" data-branch="${this.escapeHtml(t.name)}">
                    <span>${this.escapeHtml(t.name)}</span>
                    <span class="branch-hash">${this.escapeHtml(t.hash)}</span>
                </div>`;
            }
        }

        this.branchesContent.innerHTML = html;

        // Click handler — toggle branch in the multi-select and switch to log
        this.branchesContent.querySelectorAll('.git-branch-item').forEach(el => {
            el.addEventListener('click', () => {
                const branch = el.dataset.branch;
                // Set as the only selected branch
                this.selectedBranches.clear();
                this.selectedBranches.add(branch);
                this._updateBranchLabel();
                this._updateBranchCheckboxes();
                this._reloadLog();
                this.switchTab('log');
            });
        });
    }

    // --- Utilities ---

    relativeDate(dateStr) {
        if (!dateStr) return '';
        const date = new Date(dateStr);
        const now = new Date();
        const diffMs = now - date;
        const diffMins = Math.floor(diffMs / 60000);
        const diffHours = Math.floor(diffMs / 3600000);
        const diffDays = Math.floor(diffMs / 86400000);

        if (diffMins < 1) return 'just now';
        if (diffMins < 60) return `${diffMins}m ago`;
        if (diffHours < 24) return `${diffHours}h ago`;
        if (diffDays < 30) return `${diffDays}d ago`;
        if (diffDays < 365) return `${Math.floor(diffDays / 30)}mo ago`;
        return `${Math.floor(diffDays / 365)}y ago`;
    }

    escapeHtml(str) {
        if (!str) return '';
        return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }

    // --- Status tab ---

    async loadStatus() {
        if (!this.statusContent) return;
        this.statusContent.innerHTML = '<div class="git-loading"><div class="spinner"></div><span>Loading...</span></div>';

        try {
            const resp = await fetch(`/api/projects/${this.currentProjectId}/git/status`);
            if (!resp.ok) throw new Error((await resp.json()).error || 'Failed to load status');
            const data = await resp.json();
            this.statusData = data;

            if (this.statusBranch) {
                this.statusBranch.textContent = `On branch ${data.branch}`;
            }

            const hasStaged = data.staged.length > 0;
            this.commitBox?.classList.toggle('hidden', !hasStaged);

            if (!data.staged.length && !data.unstaged.length && !data.untracked.length) {
                this.statusContent.innerHTML = '<div class="git-empty">Working tree clean — nothing to commit</div>';
                return;
            }

            let html = '';

            if (data.staged.length) {
                html += this._renderStatusSection('Staged Changes', 'staged', data.staged, true);
            }
            if (data.unstaged.length) {
                html += this._renderStatusSection('Changes Not Staged', 'unstaged', data.unstaged, false);
            }
            if (data.untracked.length) {
                html += this._renderStatusSection('Untracked Files', 'untracked', data.untracked, false);
            }

            this.statusContent.innerHTML = html;

            // Wire up stage/unstage buttons
            this.statusContent.querySelectorAll('[data-action]').forEach(btn => {
                btn.addEventListener('click', (e) => {
                    e.stopPropagation();
                    const action = btn.dataset.action;
                    const file = btn.dataset.file;
                    if (action === 'unstage') this.unstageFile(file);
                    else if (action === 'stage') this.stageFile(file);
                });
            });

            // Wire up file click to show diff
            this.statusContent.querySelectorAll('.git-status-file').forEach(row => {
                row.addEventListener('click', (e) => {
                    if (e.target.closest('[data-action]')) return; // don't trigger on button clicks
                    const file = row.dataset.file;
                    const section = row.dataset.section;
                    if (section === 'staged') {
                        this.loadStagedDiff(file);
                    } else {
                        this.loadWorkingDiff(file);
                    }
                });
            });
        } catch (e) {
            this.statusContent.innerHTML = `<div class="git-error">${this.escapeHtml(e.message)}</div>`;
        }
    }

    _renderStatusSection(title, section, files, isStaged) {
        const actionLabel = isStaged ? 'Unstage' : 'Stage';
        const actionType = isStaged ? 'unstage' : 'stage';

        let html = `<div class="git-status-section">`;
        html += `<h4>${this.escapeHtml(title)} <span class="count">(${files.length})</span></h4>`;

        for (const f of files) {
            const status = f.index || f.work || '?';
            const badgeClass = status === '?' ? 'Q' : status;
            html += `<div class="git-status-file" data-file="${this.escapeHtml(f.path)}" data-section="${section}" title="Click to view diff">`;
            html += `<span class="git-status-badge ${badgeClass}">${status}</span>`;
            html += `<span class="git-status-path">${this.escapeHtml(f.path)}</span>`;
            if (section !== 'untracked') {
                html += `<div class="git-status-actions"><button data-action="${actionType}" data-file="${this.escapeHtml(f.path)}" title="${actionLabel} this file">${actionLabel}</button></div>`;
            } else {
                html += `<div class="git-status-actions"><button data-action="stage" data-file="${this.escapeHtml(f.path)}" title="Stage this file">Stage</button></div>`;
            }
            html += `</div>`;
        }

        html += `</div>`;
        return html;
    }

    async stageFile(file) {
        try {
            const resp = await fetch(`/api/projects/${this.currentProjectId}/git/stage`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ files: [file] }),
            });
            if (!resp.ok) throw new Error((await resp.json()).error);
            this.loadStatus();
        } catch (e) {
            alert('Stage failed: ' + e.message);
        }
    }

    async unstageFile(file) {
        try {
            const resp = await fetch(`/api/projects/${this.currentProjectId}/git/unstage`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ files: [file] }),
            });
            if (!resp.ok) throw new Error((await resp.json()).error);
            this.loadStatus();
        } catch (e) {
            alert('Unstage failed: ' + e.message);
        }
    }

    async loadWorkingDiff(file) {
        // Show unstaged diff for a working tree file
        this.selectedCommitHash = null;
        this.switchTab('diff');
        if (this.diffTitle) this.diffTitle.textContent = `Working changes — ${file}`;
        if (this.diffStats) this.diffStats.textContent = '';
        if (this.diffFiles) this.diffFiles.innerHTML = '<div class="git-loading"><div class="spinner"></div><span>Loading diff...</span></div>';

        try {
            const resp = await fetch(`/api/projects/${this.currentProjectId}/git/diff?ref=HEAD&file=${encodeURIComponent(file)}`);
            if (!resp.ok) throw new Error((await resp.json()).error);
            const data = await resp.json();
            if (this.diffStats) {
                this.diffStats.textContent = `${data.stats.files_changed} file(s), +${data.stats.additions} −${data.stats.deletions}`;
            }
            this.renderDiff(data.files);
        } catch (e) {
            if (this.diffFiles) this.diffFiles.innerHTML = `<div class="git-error">${this.escapeHtml(e.message)}</div>`;
        }
    }

    async loadStagedDiff(file) {
        // Show staged diff for a file
        this.selectedCommitHash = null;
        this.switchTab('diff');
        if (this.diffTitle) this.diffTitle.textContent = `Staged changes — ${file}`;
        if (this.diffStats) this.diffStats.textContent = '';
        if (this.diffFiles) this.diffFiles.innerHTML = '<div class="git-loading"><div class="spinner"></div><span>Loading diff...</span></div>';

        try {
            const params = new URLSearchParams({ staged: 'true' });
            if (file) params.set('file', file);
            const resp = await fetch(`/api/projects/${this.currentProjectId}/git/diff?${params}`);
            if (!resp.ok) throw new Error((await resp.json()).error);
            const data = await resp.json();
            if (this.diffStats) {
                this.diffStats.textContent = `${data.stats.files_changed} file(s), +${data.stats.additions} −${data.stats.deletions}`;
            }
            this.renderDiff(data.files);
        } catch (e) {
            if (this.diffFiles) this.diffFiles.innerHTML = `<div class="git-error">${this.escapeHtml(e.message)}</div>`;
        }
    }

    async doCommit() {
        const msg = this.commitMessage?.value?.trim();
        if (!msg) {
            this.commitMessage?.focus();
            return;
        }

        this.commitBtn.disabled = true;
        this.commitBtn.textContent = 'Committing...';

        try {
            const resp = await fetch(`/api/projects/${this.currentProjectId}/git/commit`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ message: msg }),
            });
            if (!resp.ok) throw new Error((await resp.json()).error);
            this.commitMessage.value = '';
            this.loadStatus();
        } catch (e) {
            alert('Commit failed: ' + e.message);
        } finally {
            this.commitBtn.disabled = false;
            this.commitBtn.textContent = 'Commit';
        }
    }

    toggleCommitExpand() {
        if (!this.commitBox || !this.commitMessage) return;
        const isExpanded = this.commitBox.classList.toggle('expanded');
        this.panels.status?.classList.toggle('commit-expanded', isExpanded);
        // Lock body scroll on mobile to prevent keyboard from pushing the overlay
        document.body.style.overflow = isExpanded ? 'hidden' : '';
        document.documentElement.style.overflow = isExpanded ? 'hidden' : '';
        if (isExpanded) this.commitMessage.focus();
    }

    async generateCommitMessage() {
        if (!this.commitMessage) return;
        const aiBtns = [this.commitAiBtn, this.commitBox?.querySelector('.git-commit-ai-exp')].filter(Boolean);
        aiBtns.forEach(b => { b.disabled = true; b.classList.add('loading'); });
        this.commitMessage.value = '';
        this.commitMessage.placeholder = 'Generating commit message...';

        try {
            // Get the staged diff for context
            const diffResp = await fetch(`/api/projects/${this.currentProjectId}/git/diff?staged=true`);
            if (!diffResp.ok) throw new Error('Failed to get staged diff');
            const diffData = await diffResp.json();

            if (!diffData.files || diffData.files.length === 0) {
                this.commitMessage.placeholder = 'No staged changes to describe';
                return;
            }

            // Build a summary of changes including actual diff content
            const fileSummaries = diffData.files.map(f => {
                let summary = `File: ${f.path} (${f.status}) +${f.additions} -${f.deletions}`;
                for (const hunk of (f.hunks || [])) {
                    for (const line of (hunk.lines || [])) {
                        if (line.type === 'addition') summary += `\n+${line.content}`;
                        else if (line.type === 'deletion') summary += `\n-${line.content}`;
                    }
                }
                return summary;
            }).join('\n\n');

            // Call AI assistant with SSE streaming
            const aiResp = await fetch('/api/ai/chat', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    message: `Generate a concise git commit message (conventional commits format like "feat:", "fix:", "refactor:" etc.) for the following staged changes. Return ONLY the commit message, no explanation, no markdown, no quotes.\n\nStaged diff:\n${fileSummaries}\n\nFiles changed: ${diffData.stats?.files_changed || 0}, +${diffData.stats?.additions || 0} -${diffData.stats?.deletions || 0}`,
                    slot: 'ai_background',
                }),
            });
            if (!aiResp.ok) throw new Error('AI request failed');

            // Consume SSE stream
            const reader = aiResp.body.getReader();
            const decoder = new TextDecoder();
            let buffer = '';
            let fullText = '';

            while (true) {
                const { done, value } = await reader.read();
                if (done) break;
                buffer += decoder.decode(value, { stream: true });
                const lines = buffer.split('\n');
                buffer = lines.pop();
                for (const line of lines) {
                    if (!line.startsWith('data: ')) continue;
                    try {
                        const event = JSON.parse(line.slice(6));
                        if (event.type === 'text') {
                            fullText += event.data.text;
                            this.commitMessage.value = fullText.trim();
                        }
                    } catch { /* skip non-JSON SSE lines */ }
                }
            }

            this.commitMessage.focus();
        } catch (e) {
            console.error('AI commit message error:', e);
            this.commitMessage.placeholder = 'Failed to generate — try manually';
        } finally {
            aiBtns.forEach(b => { b.disabled = false; b.classList.remove('loading'); });
            if (!this.commitMessage.value) {
                this.commitMessage.placeholder = 'Commit message...';
            }
        }
    }

    // --- Compare Refs ---

    openCompare() {
        this.switchTab('diff');
        if (this.compareBar) {
            this.compareBar.classList.remove('hidden');
        }
        if (this.diffTitle) this.diffTitle.textContent = 'Compare refs';
        if (this.diffStats) this.diffStats.textContent = '';
        if (this.diffFiles) this.diffFiles.innerHTML = '<div class="git-empty">Select two refs and click Compare</div>';

        // Populate dropdowns with branches + tags
        this._populateCompareSelects();
    }

    _populateCompareSelects() {
        if (!this.compareFrom || !this.compareTo) return;

        let options = '';

        // Tags (most useful for version comparisons)
        if (this.branches.tags && this.branches.tags.length > 0) {
            options += '<optgroup label="Tags">';
            for (const t of this.branches.tags) {
                options += `<option value="${this.escapeHtml(t.name)}">${this.escapeHtml(t.name)}</option>`;
            }
            options += '</optgroup>';
        }

        // Local branches
        if (this.branches.local.length > 0) {
            options += '<optgroup label="Local">';
            for (const b of this.branches.local) {
                options += `<option value="${this.escapeHtml(b.name)}"${b.is_head ? ' selected' : ''}>${this.escapeHtml(b.name)}</option>`;
            }
            options += '</optgroup>';
        }

        // Remote branches
        if (this.branches.remote.length > 0) {
            options += '<optgroup label="Remote">';
            for (const b of this.branches.remote) {
                options += `<option value="${this.escapeHtml(b.name)}">${this.escapeHtml(b.name)}</option>`;
            }
            options += '</optgroup>';
        }

        this.compareFrom.innerHTML = options;
        this.compareTo.innerHTML = options;

        // Default: from = first tag (if any), to = HEAD branch
        if (this.branches.tags && this.branches.tags.length > 0) {
            this.compareFrom.value = this.branches.tags[0].name;
        }
        const headBranch = this.branches.local.find(b => b.is_head);
        if (headBranch) {
            this.compareTo.value = headBranch.name;
        }
    }

    async runCompare() {
        const from = this.compareFrom?.value;
        const to = this.compareTo?.value;
        if (!from || !to) return;

        const ref = `${from}..${to}`;
        if (this.diffTitle) this.diffTitle.textContent = `${from} → ${to}`;
        if (this.diffStats) this.diffStats.textContent = '';
        if (this.diffFiles) this.diffFiles.innerHTML = '<div class="git-loading"><div class="spinner"></div><span>Loading diff...</span></div>';

        try {
            const resp = await fetch(`/api/projects/${this.currentProjectId}/git/diff?ref=${encodeURIComponent(ref)}`);
            if (!resp.ok) {
                const err = await resp.json();
                throw new Error(err.error || 'Failed to load diff');
            }
            const data = await resp.json();
            if (this.diffStats) {
                this.diffStats.textContent = `${data.stats.files_changed} files, +${data.stats.additions} −${data.stats.deletions}`;
            }
            this.renderDiff(data.files);
        } catch (e) {
            if (this.diffFiles) this.diffFiles.innerHTML = `<div class="git-error">${this.escapeHtml(e.message)}</div>`;
        }
    }
}

// Initialize
window.gitViewer = new GitViewer();
