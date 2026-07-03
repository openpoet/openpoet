package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"time"
)

type openCodeSessionMeta struct {
	ID        string
	CWD       string
	Timestamp time.Time
}

func discoverOpenCodeProviderSessionID(ctx context.Context, binaryPath, workDir string, since time.Time) (string, error) {
	binaryPath = strings.TrimSpace(binaryPath)
	if binaryPath == "" {
		binaryPath = "opencode"
	}
	cmd := exec.CommandContext(ctx, binaryPath, "session", "list", "--format", "json", "--max-count", "25")
	if strings.TrimSpace(workDir) != "" {
		cmd.Dir = workDir
	}
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", errors.New(strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	return selectOpenCodeProviderSessionID(out, workDir, since), nil
}

func selectOpenCodeProviderSessionID(data []byte, workDir string, since time.Time) string {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return ""
	}
	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}
	metas := collectOpenCodeSessionMetas(raw)
	if len(metas) == 0 {
		return ""
	}

	wantCWD := canonicalPath(workDir)
	var best openCodeSessionMeta
	for _, meta := range metas {
		if meta.ID == "" {
			continue
		}
		if !since.IsZero() && !meta.Timestamp.IsZero() && meta.Timestamp.Before(since) {
			continue
		}
		if wantCWD != "" && meta.CWD != "" && !samePath(wantCWD, canonicalPath(meta.CWD)) {
			continue
		}
		if best.ID == "" || betterOpenCodeSessionMeta(meta, best, wantCWD) {
			best = meta
		}
	}
	return best.ID
}

func collectOpenCodeSessionMetas(raw interface{}) []openCodeSessionMeta {
	var metas []openCodeSessionMeta
	switch v := raw.(type) {
	case []interface{}:
		for _, item := range v {
			metas = append(metas, collectOpenCodeSessionMetas(item)...)
		}
	case map[string]interface{}:
		if meta := openCodeSessionMetaFromMap(v); meta.ID != "" {
			metas = append(metas, meta)
		}
		for _, key := range []string{"sessions", "data", "items", "results"} {
			if nested, ok := v[key]; ok {
				metas = append(metas, collectOpenCodeSessionMetas(nested)...)
			}
		}
	}
	return metas
}

func openCodeSessionMetaFromMap(m map[string]interface{}) openCodeSessionMeta {
	return openCodeSessionMeta{
		ID:        firstString(m, "id", "sessionID", "sessionId", "session_id"),
		CWD:       firstString(m, "cwd", "directory", "path", "project", "projectPath", "project_path"),
		Timestamp: firstTime(m, "updatedAt", "updated_at", "updated", "createdAt", "created_at", "time", "timestamp"),
	}
}

func firstString(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstTime(m map[string]interface{}, keys ...string) time.Time {
	for _, key := range keys {
		switch v := m[key].(type) {
		case string:
			if ts := parseOpenCodeTime(v); !ts.IsZero() {
				return ts
			}
		case float64:
			if v > 0 {
				if v > 1_000_000_000_000 {
					return time.UnixMilli(int64(v))
				}
				return time.Unix(int64(v), 0)
			}
		}
	}
	return time.Time{}
}

func parseOpenCodeTime(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if ts, err := time.Parse(layout, v); err == nil {
			return ts
		}
	}
	return time.Time{}
}

func betterOpenCodeSessionMeta(candidate, current openCodeSessionMeta, wantCWD string) bool {
	candidateMatches := wantCWD != "" && candidate.CWD != "" && samePath(wantCWD, canonicalPath(candidate.CWD))
	currentMatches := wantCWD != "" && current.CWD != "" && samePath(wantCWD, canonicalPath(current.CWD))
	if candidateMatches != currentMatches {
		return candidateMatches
	}
	if !candidate.Timestamp.IsZero() && !current.Timestamp.IsZero() {
		return candidate.Timestamp.After(current.Timestamp)
	}
	if !candidate.Timestamp.IsZero() != !current.Timestamp.IsZero() {
		return !candidate.Timestamp.IsZero()
	}
	return candidate.ID > current.ID
}
