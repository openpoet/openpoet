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
    }

    switchTab(tab) {
        this.currentTab = tab;
        this.tabs?.forEach(t => t.classList.toggle('active', t.dataset.tab === tab));
        Object.entries(this.panels).forEach(([key, panel]) => {
            panel?.classList.toggle('hidden', key !== tab);
        });
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

            if (line.from === line.to) {
                // Straight vertical pass-through
                const el = document.createElementNS(ns, 'line');
                el.setAttribute('x1', x1);
                el.setAttribute('y1', 0);
                el.setAttribute('x2', x2);
                el.setAttribute('y2', GitViewer.ROW_HEIGHT);
                el.setAttribute('stroke', color);
                el.setAttribute('stroke-width', '2');
                svg.appendChild(el);
            } else {
                // Curved line (branch/merge)
                const path = document.createElementNS(ns, 'path');
                path.setAttribute('d', `M ${x1},${midY} C ${x1},${GitViewer.ROW_HEIGHT} ${x2},0 ${x2},${midY}`);
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
}

// Initialize
window.gitViewer = new GitViewer();
