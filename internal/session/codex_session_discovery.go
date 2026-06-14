package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type codexSessionMeta struct {
	ID         string
	CWD        string
	Originator string
	Timestamp  time.Time
	Path       string
}

func discoverCodexProviderSessionID(codexHome, workDir string, since time.Time) (string, error) {
	metas, err := discoverCodexSessionMetas(codexHome, workDir, since)
	if err != nil {
		return "", err
	}
	if len(metas) == 0 {
		return "", nil
	}
	best := metas[0]
	for _, meta := range metas[1:] {
		if betterCodexSessionMeta(meta, best) {
			best = meta
		}
	}
	return best.ID, nil
}

func codexProviderSessionIDExists(codexHome, providerID string) bool {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return false
	}
	metas, err := discoverCodexSessionMetas(codexHome, "", time.Time{})
	if err != nil {
		return false
	}
	for _, meta := range metas {
		if meta.ID == providerID {
			return true
		}
	}
	return false
}

func discoverCodexSessionMetas(codexHome, workDir string, since time.Time) ([]codexSessionMeta, error) {
	if strings.TrimSpace(codexHome) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		codexHome = filepath.Join(home, ".codex")
	}
	sessionsDir := filepath.Join(expandCodexHome(codexHome), "sessions")
	info, err := os.Stat(sessionsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	wantCWD := canonicalPath(workDir)
	var metas []codexSessionMeta
	err = filepath.WalkDir(sessionsDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err == nil && !since.IsZero() && info.ModTime().Before(since) {
			return nil
		}
		meta, ok := readCodexSessionMeta(path)
		if !ok || meta.ID == "" {
			return nil
		}
		if wantCWD != "" && !samePath(wantCWD, canonicalPath(meta.CWD)) {
			return nil
		}
		if meta.Timestamp.IsZero() && err == nil {
			meta.Timestamp = info.ModTime()
		}
		if !since.IsZero() && meta.Timestamp.Before(since) {
			return nil
		}
		metas = append(metas, meta)
		return nil
	})
	return metas, err
}

func readCodexSessionMeta(path string) (codexSessionMeta, bool) {
	f, err := os.Open(path)
	if err != nil {
		return codexSessionMeta{}, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				ID         string `json:"id"`
				CWD        string `json:"cwd"`
				Originator string `json:"originator"`
				Timestamp  string `json:"timestamp"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil || raw.Type != "session_meta" {
			return codexSessionMeta{}, false
		}
		ts := parseCodexMetaTime(raw.Payload.Timestamp)
		if ts.IsZero() {
			ts = parseCodexMetaTime(raw.Timestamp)
		}
		return codexSessionMeta{
			ID:         strings.TrimSpace(raw.Payload.ID),
			CWD:        strings.TrimSpace(raw.Payload.CWD),
			Originator: strings.TrimSpace(raw.Payload.Originator),
			Timestamp:  ts,
			Path:       path,
		}, true
	}
	return codexSessionMeta{}, false
}

func betterCodexSessionMeta(candidate, current codexSessionMeta) bool {
	candidateOpenPoet := strings.EqualFold(candidate.Originator, "openpoet")
	currentOpenPoet := strings.EqualFold(current.Originator, "openpoet")
	if candidateOpenPoet != currentOpenPoet {
		return candidateOpenPoet
	}
	return candidate.Timestamp.After(current.Timestamp)
}

func parseCodexMetaTime(value string) time.Time {
	if strings.TrimSpace(value) == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return ts
	}
	return time.Time{}
}

func canonicalPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return filepath.Clean(path)
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return a == b
}
