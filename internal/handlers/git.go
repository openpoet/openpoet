package handlers

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// GitHandler serves git-related endpoints for project repositories.
type GitHandler struct {
	api *API
}

func NewGitHandler(api *API) *GitHandler {
	return &GitHandler{api: api}
}

// safeRefRe validates git ref names to prevent injection.
var safeRefRe = regexp.MustCompile(`^[a-zA-Z0-9_./:~^{}\-@]+$`)

func validateRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("ref is required")
	}
	if !safeRefRe.MatchString(ref) {
		return fmt.Errorf("invalid ref: %q", ref)
	}
	return nil
}

const (
	gitTimeout   = 30 * time.Second
	maxOutputLen = 5 * 1024 * 1024 // 5MB
)

// runGit executes a git command in the project directory (local only for now).
func (h *GitHandler) runGit(ctx context.Context, projectPath string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = projectPath

	var stdout, stderr bytes.Buffer
	stdout.Grow(64 * 1024)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s (stderr: %s)", args[0], err, strings.TrimSpace(stderr.String()))
	}

	if stdout.Len() > maxOutputLen {
		return stdout.String()[:maxOutputLen], nil
	}
	return stdout.String(), nil
}

// getProjectPath resolves project ID from URL and returns its path (local only).
func (h *GitHandler) getProjectPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid project ID")
		return "", false
	}

	project, err := h.api.db.GetProject(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "Project not found")
		return "", false
	}

	if project.Type != "local" {
		respondError(w, http.StatusBadRequest, "Git operations are only available for local projects")
		return "", false
	}

	// Verify it's a git repo
	out, err := h.runGit(r.Context(), project.Path, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(out) != "true" {
		respondError(w, http.StatusBadRequest, "Project directory is not a git repository")
		return "", false
	}

	return project.Path, true
}

// --- Branches endpoint ---

type BranchInfo struct {
	Name     string `json:"name"`
	Hash     string `json:"hash"`
	IsHead   bool   `json:"is_head,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Date     string `json:"date"`
}

type BranchesResponse struct {
	Local   []BranchInfo `json:"local"`
	Remote  []BranchInfo `json:"remote"`
	Current string       `json:"current"`
}

func (h *GitHandler) GetBranches(w http.ResponseWriter, r *http.Request) {
	path, ok := h.getProjectPath(w, r)
	if !ok {
		return
	}

	out, err := h.runGit(r.Context(), path,
		"branch", "-a",
		"--format=%(refname:short)%00%(objectname:short)%00%(HEAD)%00%(upstream:short)%00%(committerdate:iso-strict)",
	)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := BranchesResponse{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 5)
		if len(parts) < 5 {
			continue
		}
		b := BranchInfo{
			Name:     parts[0],
			Hash:     parts[1],
			IsHead:   parts[2] == "*",
			Upstream: parts[3],
			Date:     parts[4],
		}
		if b.IsHead {
			resp.Current = b.Name
		}
		if strings.HasPrefix(b.Name, "origin/") || strings.Contains(b.Name, "/") && !strings.Contains(b.Name, "HEAD") {
			// Skip remote HEAD pointers
			if strings.HasSuffix(b.Name, "/HEAD") {
				continue
			}
			resp.Remote = append(resp.Remote, b)
		} else {
			resp.Local = append(resp.Local, b)
		}
	}

	if resp.Local == nil {
		resp.Local = []BranchInfo{}
	}
	if resp.Remote == nil {
		resp.Remote = []BranchInfo{}
	}

	respondJSON(w, http.StatusOK, resp)
}

// --- Log endpoint ---

type CommitInfo struct {
	Hash        string    `json:"hash"`
	ShortHash   string    `json:"short_hash"`
	Parents     []string  `json:"parents"`
	AuthorName  string    `json:"author_name"`
	AuthorEmail string    `json:"author_email"`
	Date        string    `json:"date"`
	Message     string    `json:"message"`
	Refs        []string  `json:"refs,omitempty"`
	Graph       GraphNode `json:"graph"`
}

type LogResponse struct {
	Commits []CommitInfo `json:"commits"`
	Total   int          `json:"total"`
	Page    int          `json:"page"`
	HasMore bool         `json:"has_more"`
}

func (h *GitHandler) GetLog(w http.ResponseWriter, r *http.Request) {
	path, ok := h.getProjectPath(w, r)
	if !ok {
		return
	}

	branches := r.URL.Query()["branch"]
	pageStr := r.URL.Query().Get("page")
	limitStr := r.URL.Query().Get("limit")
	search := r.URL.Query().Get("search")
	author := r.URL.Query().Get("author")

	page := 0
	if pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil && p >= 0 {
			page = p
		}
	}
	limit := 50
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 200 {
			limit = l
		}
	}

	// Build args
	args := []string{"log"}
	// Filter out empty strings and "all"
	var validBranches []string
	for _, b := range branches {
		if b != "" && b != "all" {
			validBranches = append(validBranches, b)
		}
	}
	if len(validBranches) > 0 {
		for _, b := range validBranches {
			if err := validateRef(b); err != nil {
				respondError(w, http.StatusBadRequest, err.Error())
				return
			}
			args = append(args, b)
		}
	} else {
		args = append(args, "--all")
	}

	args = append(args,
		"--topo-order",
		"--format=%H%x00%P%x00%an%x00%ae%x00%aI%x00%s%x00%D",
		fmt.Sprintf("--skip=%d", page*limit),
		fmt.Sprintf("--max-count=%d", limit+1), // +1 to detect has_more
	)

	if search != "" {
		args = append(args, "--grep="+search)
	}
	if author != "" {
		args = append(args, "--author="+author)
	}

	out, err := h.runGit(r.Context(), path, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	hasMore := len(lines) > limit
	if hasMore {
		lines = lines[:limit]
	}

	commits := make([]CommitInfo, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 7)
		if len(parts) < 7 {
			continue
		}

		var parents []string
		if parts[1] != "" {
			parents = strings.Split(parts[1], " ")
		}

		var refs []string
		if parts[6] != "" {
			for _, ref := range strings.Split(parts[6], ", ") {
				refs = append(refs, strings.TrimSpace(ref))
			}
		}

		shortHash := parts[0]
		if len(shortHash) > 7 {
			shortHash = shortHash[:7]
		}

		commits = append(commits, CommitInfo{
			Hash:        parts[0],
			ShortHash:   shortHash,
			Parents:     parents,
			AuthorName:  parts[2],
			AuthorEmail: parts[3],
			Date:        parts[4],
			Message:     parts[5],
			Refs:        refs,
		})
	}

	// Compute graph
	computeGraph(commits)

	// Get total count (approximate, cached for UX)
	total := page*limit + len(commits)
	if hasMore {
		// Estimate with rev-list count
		countArgs := []string{"rev-list", "--count"}
		if len(validBranches) > 0 {
			countArgs = append(countArgs, validBranches...)
		} else {
			countArgs = append(countArgs, "--all")
		}
		countOut, err := h.runGit(r.Context(), path, countArgs...)
		if err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(countOut)); err == nil {
				total = n
			}
		}
	}

	respondJSON(w, http.StatusOK, LogResponse{
		Commits: commits,
		Total:   total,
		Page:    page,
		HasMore: hasMore,
	})
}

// --- Graph algorithm ---

type GraphNode struct {
	Column int         `json:"column"`
	Lines  []GraphLine `json:"lines"`
}

type GraphLine struct {
	From  int    `json:"from"`
	To    int    `json:"to"`
	Color int    `json:"color"`
	Half  string `json:"half,omitempty"` // "top" = y:0→mid (ends at dot), "" = full height
}

func computeGraph(commits []CommitInfo) {
	// rails tracks active branch heads by commit hash.
	// Index = column position. Lines map from pre-state columns (top of row)
	// to post-state columns (bottom of row) so adjacent rows connect properly.
	type rail struct {
		hash  string
		color int
	}

	rails := []rail{}
	nextColor := 0
	// Track processed commits so we can detect already-rendered parents
	processed := map[string]int{} // hash → column where commit was rendered

	for i := range commits {
		c := &commits[i]
		hash := c.Hash

		// Find which rail(s) this commit matches
		matchIdx := -1
		for j, r := range rails {
			if r.hash == hash {
				matchIdx = j
				break
			}
		}

		// Track whether this is a brand-new branch appearing for the first time
		isNewBranch := matchIdx == -1
		forkFromRail := -1

		if isNewBranch {
			// New branch — add a rail
			matchIdx = len(rails)
			rails = append(rails, rail{hash: hash, color: nextColor})
			nextColor++

			// Determine which existing rail this branch forked from.
			// Check if the first parent is tracked by an existing rail (exact match).
			// Otherwise use the leftmost rail as a heuristic.
			if len(rails) > 1 && len(c.Parents) > 0 {
				parentHash := c.Parents[0]
				for j, r := range rails {
					if j != matchIdx && r.hash == parentHash {
						forkFromRail = j
						break
					}
				}
				if forkFromRail == -1 {
					forkFromRail = 0
				}
			}
		}

		c.Graph.Column = matchIdx
		commitColor := rails[matchIdx].color
		processed[hash] = matchIdx

		// Collect merge rails (same hash, different columns) — these are
		// fork points going back in time: multiple branches converge here.
		mergeIndices := []int{}
		for j := range rails {
			if j != matchIdx && rails[j].hash == hash {
				mergeIndices = append(mergeIndices, j)
			}
		}

		// Save pre-state for line computation
		preRails := make([]rail, len(rails))
		copy(preRails, rails)

		// Build the set of rails to remove
		removed := map[int]bool{}
		for _, j := range mergeIndices {
			removed[j] = true
		}
		isRoot := len(c.Parents) == 0
		if isRoot {
			removed[matchIdx] = true
		}

		// Determine which extra parents are already processed (rendered above)
		// vs which need new rails. Already-processed parents get a line to their
		// column instead of a new rail (avoids ghost rails from topo-sort ordering).
		type extraParentInfo struct {
			alreadyProcessed bool
			targetCol        int // pre-state column to draw line to
			postIdx          int // post-state rail index (only if not already processed)
			color            int
		}
		var extraParents []extraParentInfo
		if len(c.Parents) > 1 {
			for p := 1; p < len(c.Parents); p++ {
				parentHash := c.Parents[p]
				if _, ok := processed[parentHash]; ok {
					// Parent already rendered — find the rail currently at its column
					// and draw a line to that column (no new rail needed)
					targetCol := -1
					for j, r := range preRails {
						if r.hash == parentHash {
							targetCol = j
							break
						}
					}
					if targetCol == -1 {
						// Parent was processed but no rail tracks it; use its rendered column
						targetCol = processed[parentHash]
					}
					extraParents = append(extraParents, extraParentInfo{
						alreadyProcessed: true,
						targetCol:        targetCol,
						color:            nextColor,
					})
					nextColor++
				} else {
					extraParents = append(extraParents, extraParentInfo{
						alreadyProcessed: false,
						color:            nextColor,
					})
					nextColor++
				}
			}
		}

		// Build post-state rails and pre→post column mapping
		postRails := []rail{}
		preToPost := make(map[int]int)

		for preIdx, r := range preRails {
			if removed[preIdx] {
				continue
			}
			if preIdx == matchIdx && !isRoot {
				// Matched rail → update to first parent
				preToPost[preIdx] = len(postRails)
				postRails = append(postRails, rail{hash: c.Parents[0], color: commitColor})

				// Insert new rails only for extra parents NOT already processed
				for ep := range extraParents {
					if !extraParents[ep].alreadyProcessed {
						extraParents[ep].postIdx = len(postRails)
						postRails = append(postRails, rail{hash: c.Parents[ep+1], color: extraParents[ep].color})
					}
				}
			} else {
				preToPost[preIdx] = len(postRails)
				postRails = append(postRails, r)
			}
		}
		rails = postRails

		// Ghost rail cleanup: detect post-state rails pointing to already-processed
		// commits. These will never match a future row, so remove them now.
		// This can happen when date ordering causes a parent to be rendered before
		// all its children (topo-order prevents most cases, but this is a safety net).
		ghostRails := map[int]int{} // post-index → column where the ghost target was rendered
		for postIdx, r := range rails {
			if col, ok := processed[r.hash]; ok {
				ghostRails[postIdx] = col
			}
		}

		// Build lines
		var lines []GraphLine

		if !isRoot {
			matchPost := preToPost[matchIdx]

			// First parent line: from commit pre-column to its post-column.
			// New branches only draw bottom half (fork indicator provides the top connection).
			firstParentHalf := ""
			if isNewBranch {
				firstParentHalf = "bottom"
			}
			lines = append(lines, GraphLine{
				From:  matchIdx,
				To:    matchPost,
				Color: commitColor,
				Half:  firstParentHalf,
			})

			// Merge convergence lines: merging rails curve into commit column
			// Half:"top" — these lines terminate at the dot, not below it
			for _, j := range mergeIndices {
				lines = append(lines, GraphLine{
					From:  j,
					To:    matchPost,
					Color: preRails[j].color,
					Half:  "top",
				})
			}

			// Extra parent lines
			for _, ep := range extraParents {
				if ep.alreadyProcessed {
					lines = append(lines, GraphLine{
						From:  matchIdx,
						To:    ep.targetCol,
						Color: ep.color,
					})
				} else {
					lines = append(lines, GraphLine{
						From:  matchIdx,
						To:    ep.postIdx,
						Color: rails[ep.postIdx].color,
					})
				}
			}

			// Fork indicator: new branch shows derivation from existing rail
			// Half:"top" — the fork curve terminates at the dot
			if isNewBranch && forkFromRail >= 0 {
				if forkFromRail != matchPost {
					lines = append(lines, GraphLine{
						From:  forkFromRail,
						To:    matchPost,
						Color: commitColor,
						Half:  "top",
					})
				}
			}
		}

		// Pass-through lines for rails unrelated to this commit
		// These maintain visual continuity for branches passing through this row.
		// Skip ghost rails — they point to already-processed commits and should end here.
		occupiedTo := map[int]bool{}
		for _, line := range lines {
			occupiedTo[line.To] = true
		}
		for preIdx := range preRails {
			if preIdx == matchIdx || removed[preIdx] {
				continue
			}
			postIdx := preToPost[preIdx]
			if _, isGhost := ghostRails[postIdx]; isGhost {
				continue
			}
			if !occupiedTo[postIdx] {
				lines = append(lines, GraphLine{
					From:  preIdx,
					To:    postIdx,
					Color: preRails[preIdx].color,
				})
				occupiedTo[postIdx] = true
			}
		}

		// Remove ghost rails from post-state so they don't persist
		if len(ghostRails) > 0 {
			cleanRails := []rail{}
			for postIdx, r := range rails {
				if _, isGhost := ghostRails[postIdx]; !isGhost {
					cleanRails = append(cleanRails, r)
				}
			}
			rails = cleanRails
		}

		c.Graph.Lines = lines
	}
}

// --- Diff endpoint ---

type DiffFile struct {
	Path      string     `json:"path"`
	OldPath   string     `json:"old_path,omitempty"`
	Status    string     `json:"status"` // "added", "deleted", "modified", "renamed"
	Binary    bool       `json:"binary,omitempty"`
	Additions int        `json:"additions"`
	Deletions int        `json:"deletions"`
	Hunks     []DiffHunk `json:"hunks,omitempty"`
}

type DiffHunk struct {
	Header   string     `json:"header"`
	OldStart int        `json:"old_start"`
	OldLines int        `json:"old_lines"`
	NewStart int        `json:"new_start"`
	NewLines int        `json:"new_lines"`
	Lines    []DiffLine `json:"lines"`
}

type DiffLine struct {
	Type    string `json:"type"` // "context", "addition", "deletion"
	Content string `json:"content"`
	OldNum  int    `json:"old_num,omitempty"`
	NewNum  int    `json:"new_num,omitempty"`
}

type DiffResponse struct {
	Ref   string     `json:"ref"`
	Files []DiffFile `json:"files"`
	Stats struct {
		FilesChanged int `json:"files_changed"`
		Additions    int `json:"additions"`
		Deletions    int `json:"deletions"`
	} `json:"stats"`
}

func (h *GitHandler) GetDiff(w http.ResponseWriter, r *http.Request) {
	path, ok := h.getProjectPath(w, r)
	if !ok {
		return
	}

	ref := r.URL.Query().Get("ref")
	if err := validateRef(ref); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	fileFilter := r.URL.Query().Get("file")
	statOnly := r.URL.Query().Get("stat_only") == "true"

	// Determine diff range
	var diffRef string
	if strings.Contains(ref, "..") {
		diffRef = ref
	} else {
		// Single commit — diff against parent
		diffRef = ref + "^.." + ref
	}

	if statOnly {
		args := []string{"diff", "--stat", "--numstat", diffRef}
		out, err := h.runGit(r.Context(), path, args...)
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		files := parseNumstat(out)
		resp := DiffResponse{Ref: ref, Files: files}
		for _, f := range files {
			resp.Stats.FilesChanged++
			resp.Stats.Additions += f.Additions
			resp.Stats.Deletions += f.Deletions
		}
		respondJSON(w, http.StatusOK, resp)
		return
	}

	// Full diff
	args := []string{"diff", "--unified=3", "-M", diffRef}
	if fileFilter != "" {
		args = append(args, "--", fileFilter)
	}
	out, err := h.runGit(r.Context(), path, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	files := parseUnifiedDiff(out)
	resp := DiffResponse{Ref: ref, Files: files}
	for _, f := range files {
		resp.Stats.FilesChanged++
		resp.Stats.Additions += f.Additions
		resp.Stats.Deletions += f.Deletions
	}
	respondJSON(w, http.StatusOK, resp)
}

// parseNumstat parses `git diff --numstat` output.
func parseNumstat(out string) []DiffFile {
	var files []DiffFile
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		add, _ := strconv.Atoi(parts[0])
		del, _ := strconv.Atoi(parts[1])
		binary := parts[0] == "-" && parts[1] == "-"
		files = append(files, DiffFile{
			Path:      parts[2],
			Status:    "modified",
			Binary:    binary,
			Additions: add,
			Deletions: del,
		})
	}
	return files
}

// parseUnifiedDiff parses a full unified diff into structured DiffFile objects.
func parseUnifiedDiff(out string) []DiffFile {
	var files []DiffFile
	var current *DiffFile
	var currentHunk *DiffHunk
	var oldNum, newNum int

	lines := strings.Split(out, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]

		// New file diff
		if strings.HasPrefix(line, "diff --git ") {
			if current != nil {
				if currentHunk != nil {
					current.Hunks = append(current.Hunks, *currentHunk)
				}
				files = append(files, *current)
			}
			current = &DiffFile{Status: "modified"}
			currentHunk = nil

			// Parse file names from diff --git a/foo b/bar
			parts := strings.SplitN(line, " b/", 2)
			if len(parts) == 2 {
				current.Path = parts[1]
			}
			continue
		}

		if current == nil {
			continue
		}

		// New/deleted file detection
		if strings.HasPrefix(line, "new file mode") {
			current.Status = "added"
			continue
		}
		if strings.HasPrefix(line, "deleted file mode") {
			current.Status = "deleted"
			continue
		}
		if strings.HasPrefix(line, "rename from ") {
			current.OldPath = strings.TrimPrefix(line, "rename from ")
			current.Status = "renamed"
			continue
		}
		if strings.HasPrefix(line, "rename to ") {
			current.Path = strings.TrimPrefix(line, "rename to ")
			continue
		}
		if strings.HasPrefix(line, "Binary files") {
			current.Binary = true
			continue
		}

		// Skip --- and +++ header lines
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			continue
		}
		// Index line
		if strings.HasPrefix(line, "index ") {
			continue
		}

		// Hunk header
		if strings.HasPrefix(line, "@@") {
			if currentHunk != nil {
				current.Hunks = append(current.Hunks, *currentHunk)
			}
			currentHunk = &DiffHunk{Header: line}
			parseHunkHeader(line, currentHunk)
			oldNum = currentHunk.OldStart
			newNum = currentHunk.NewStart
			continue
		}

		if currentHunk == nil {
			continue
		}

		// Diff content lines
		if strings.HasPrefix(line, "+") {
			currentHunk.Lines = append(currentHunk.Lines, DiffLine{
				Type:    "addition",
				Content: line[1:],
				NewNum:  newNum,
			})
			newNum++
			current.Additions++
		} else if strings.HasPrefix(line, "-") {
			currentHunk.Lines = append(currentHunk.Lines, DiffLine{
				Type:    "deletion",
				Content: line[1:],
				OldNum:  oldNum,
			})
			oldNum++
			current.Deletions++
		} else if strings.HasPrefix(line, " ") {
			currentHunk.Lines = append(currentHunk.Lines, DiffLine{
				Type:    "context",
				Content: line[1:],
				OldNum:  oldNum,
				NewNum:  newNum,
			})
			oldNum++
			newNum++
		} else if line == `\ No newline at end of file` {
			// Skip the no-newline marker
			continue
		}
	}

	// Flush last file
	if current != nil {
		if currentHunk != nil {
			current.Hunks = append(current.Hunks, *currentHunk)
		}
		files = append(files, *current)
	}

	if files == nil {
		files = []DiffFile{}
	}
	return files
}

// parseHunkHeader extracts line numbers from "@@ -10,7 +10,9 @@" format.
func parseHunkHeader(header string, hunk *DiffHunk) {
	// Find the @@ ... @@ portion
	parts := strings.SplitN(header, "@@", 3)
	if len(parts) < 2 {
		return
	}
	rangeStr := strings.TrimSpace(parts[1])
	// Parse -old,count +new,count
	ranges := strings.Fields(rangeStr)
	for _, r := range ranges {
		if strings.HasPrefix(r, "-") {
			nums := strings.SplitN(r[1:], ",", 2)
			hunk.OldStart, _ = strconv.Atoi(nums[0])
			if len(nums) > 1 {
				hunk.OldLines, _ = strconv.Atoi(nums[1])
			} else {
				hunk.OldLines = 1
			}
		} else if strings.HasPrefix(r, "+") {
			nums := strings.SplitN(r[1:], ",", 2)
			hunk.NewStart, _ = strconv.Atoi(nums[0])
			if len(nums) > 1 {
				hunk.NewLines, _ = strconv.Atoi(nums[1])
			} else {
				hunk.NewLines = 1
			}
		}
	}
}

// --- Show endpoint ---

func (h *GitHandler) GetShow(w http.ResponseWriter, r *http.Request) {
	path, ok := h.getProjectPath(w, r)
	if !ok {
		return
	}

	ref := r.URL.Query().Get("ref")
	if err := validateRef(ref); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		respondError(w, http.StatusBadRequest, "path parameter is required")
		return
	}

	out, err := h.runGit(r.Context(), path, "show", ref+":"+filePath)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ref":     ref,
		"path":    filePath,
		"content": out,
		"size":    len(out),
	})
}
