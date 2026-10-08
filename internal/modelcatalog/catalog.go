// Package modelcatalog discovers the models each coding harness can start,
// by asking the installed CLI itself instead of hard-coding model IDs:
//
//   - Claude Code: the stream-json "initialize" control request (the same call
//     the Agent SDK uses for supportedModels()).
//   - Codex: the app-server "model/list" JSON-RPC method.
//   - OpenCode: `opencode models`.
//
// Probes are slow (they boot the CLI), so results are cached per harness.
package modelcatalog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Model is one selectable model for a harness.
type Model struct {
	// ID is the value to store in the project config / pass as --model.
	ID string `json:"id"`
	// ResolvedID is the concrete model an alias currently maps to, when it
	// differs from ID (e.g. "opus" -> "claude-opus-5-5").
	ResolvedID  string `json:"resolved_id,omitempty"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	IsDefault   bool   `json:"is_default,omitempty"`
	// Efforts are the reasoning effort levels the harness accepts for this
	// model, as it reports them; empty when the model takes no effort.
	Efforts []string `json:"efforts,omitempty"`
	// DefaultEffort is the effort the harness picks when none is passed
	// (reported by Codex only).
	DefaultEffort string `json:"default_effort,omitempty"`
}

// Find returns the entry whose ID (or, for an alias, resolved ID) is id.
func (c Catalog) Find(id string) (Model, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Model{}, false
	}
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	for _, m := range c.Models {
		if m.ResolvedID == id {
			return m, true
		}
	}
	return Model{}, false
}

// fallbackEfforts are the effort levels each harness CLI documents, used when
// its catalog cannot be read (CLI missing, probe failed).
var fallbackEfforts = map[string][]string{
	"claude_code": {"low", "medium", "high", "xhigh", "max"},
	"codex":       {"minimal", "low", "medium", "high", "xhigh", "max", "ultra"},
}

// KnownEfforts returns every effort level harness accepts for some model: the
// union of its catalog's levels, or the documented set without a catalog.
func KnownEfforts(harness string, catalog Catalog) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range catalog.Models {
		for _, e := range m.Efforts {
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	if len(out) == 0 {
		out = append(out, fallbackEfforts[harness]...)
	}
	return out
}

// Catalog is the discovery result for one harness.
type Catalog struct {
	Harness   string    `json:"harness"`
	Models    []Model   `json:"models"`
	FetchedAt time.Time `json:"fetched_at"`
	Error     string    `json:"error,omitempty"`
}

const (
	successTTL   = time.Hour
	failureTTL   = 2 * time.Minute
	probeTimeout = 30 * time.Second
)

type entry struct {
	catalog Catalog
	expires time.Time
}

// Service caches catalogs and deduplicates concurrent probes.
type Service struct {
	mu      sync.Mutex
	cache   map[string]entry
	pending map[string]chan struct{}
	probes  map[string]func(context.Context) ([]Model, error)
}

// NewService returns a Service wired to the real CLIs.
func NewService() *Service {
	return &Service{
		cache:   map[string]entry{},
		pending: map[string]chan struct{}{},
		probes: map[string]func(context.Context) ([]Model, error){
			"claude_code": probeClaude,
			"codex":       probeCodex,
			"opencode":    probeOpenCode,
		},
	}
}

// Supported reports whether a harness has a model probe.
func (s *Service) Supported(harness string) bool {
	_, ok := s.probes[harness]
	return ok
}

// Get returns the cached catalog for harness, probing the CLI when the cache
// is empty, expired or refresh is set. Probe failures are returned inside the
// Catalog (Error) so the UI can still fall back to free text.
func (s *Service) Get(ctx context.Context, harness string, refresh bool) Catalog {
	probe, ok := s.probes[harness]
	if !ok {
		return Catalog{Harness: harness, Models: []Model{}, Error: "unsupported harness"}
	}
	for {
		s.mu.Lock()
		if e, ok := s.cache[harness]; ok && !refresh && time.Now().Before(e.expires) {
			s.mu.Unlock()
			return e.catalog
		}
		if wait, ok := s.pending[harness]; ok {
			s.mu.Unlock()
			select {
			case <-wait:
				refresh = false
				continue
			case <-ctx.Done():
				return Catalog{Harness: harness, Models: []Model{}, Error: ctx.Err().Error()}
			}
		}
		done := make(chan struct{})
		s.pending[harness] = done
		s.mu.Unlock()

		// Detached from the request context so a closed browser tab does not
		// abort a probe other callers are waiting on.
		probeCtx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		models, err := probe(probeCtx)
		cancel()

		catalog := Catalog{Harness: harness, Models: models, FetchedAt: time.Now()}
		ttl := successTTL
		if catalog.Models == nil {
			catalog.Models = []Model{}
		}
		if err != nil {
			catalog.Error = err.Error()
			ttl = failureTTL
		}
		s.mu.Lock()
		s.cache[harness] = entry{catalog: catalog, expires: time.Now().Add(ttl)}
		delete(s.pending, harness)
		close(done)
		s.mu.Unlock()
		return catalog
	}
}

// findBinary resolves a CLI from PATH, falling back to the user-level install
// locations used on the OpenPoet host (systemd units have a minimal PATH).
func findBinary(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, dir := range []string{".local/bin", ".nvm/current/bin", ".opencode/bin", ".npm-global/bin", "bin"} {
			p := filepath.Join(home, dir, name)
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("%s CLI not found on this host", name)
}

func probeEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		// A nested-session marker makes Claude Code refuse to start.
		if strings.HasPrefix(kv, "CLAUDECODE=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// runJSONLines starts bin with args, writes requests (one JSON per line) and
// scans stdout lines until match returns true. The process is killed after.
func runJSONLines(ctx context.Context, bin string, args []string, requests []string, match func(line []byte) (bool, error)) error {
	dir, err := os.MkdirTemp("", "openpoet-models-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = probeEnv()
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = stdin.Close()
		cancel()
		_ = cmd.Wait()
	}()

	for _, req := range requests {
		if _, err := io.WriteString(stdin, req+"\n"); err != nil {
			return err
		}
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		done, err := match(scanner.Bytes())
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	if ctx.Err() != nil {
		return fmt.Errorf("timed out waiting for model list")
	}
	return errors.New("CLI exited without returning a model list")
}

func probeClaude(ctx context.Context) ([]Model, error) {
	bin, err := findBinary("claude")
	if err != nil {
		return nil, err
	}
	// No setting sources: user/project hooks (SessionStart, OpenPoet's bridge)
	// must not fire for a throwaway probe. No MCP servers, no transcript.
	args := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--setting-sources", "", "--strict-mcp-config", "--no-session-persistence",
	}
	req := `{"type":"control_request","request_id":"openpoet-models","request":{"subtype":"initialize"}}`

	var models []Model
	err = runJSONLines(ctx, bin, args, []string{req}, func(line []byte) (bool, error) {
		var msg struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string `json:"subtype"`
				RequestID string `json:"request_id"`
				Error     string `json:"error"`
				Response  struct {
					Models []claudeModel `json:"models"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(line, &msg) != nil || msg.Type != "control_response" || msg.Response.RequestID != "openpoet-models" {
			return false, nil
		}
		if msg.Response.Subtype != "success" {
			return false, fmt.Errorf("claude initialize failed: %s", msg.Response.Error)
		}
		models = claudeModels(msg.Response.Response.Models)
		return true, nil
	})
	return models, err
}

type claudeModel struct {
	Value                 string   `json:"value"`
	ResolvedModel         string   `json:"resolvedModel"`
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description"`
	SupportsEffort        bool     `json:"supportsEffort"`
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
}

func (m claudeModel) efforts() []string {
	if !m.SupportsEffort {
		return nil
	}
	return normalizeEfforts(m.SupportedEffortLevels)
}

func normalizeEfforts(in []string) []string {
	var out []string
	for _, e := range in {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// claudeModels maps the harness list to catalog entries. The "default" entry
// is dropped (an empty project model already means default) and, for every
// alias, the concrete model it resolves to is also offered so the user can
// pin an exact ID instead of following the alias.
func claudeModels(in []claudeModel) []Model {
	listed := map[string]bool{}
	for _, m := range in {
		listed[strings.TrimSpace(m.Value)] = true
	}
	out := make([]Model, 0, len(in)*2)
	seen := map[string]bool{}
	for _, m := range in {
		id := strings.TrimSpace(m.Value)
		if id == "" || strings.EqualFold(id, "default") || seen[id] {
			continue
		}
		seen[id] = true
		resolved := strings.TrimSpace(m.ResolvedModel)
		label := strings.TrimSpace(m.DisplayName)
		if label == "" {
			label = id
		}
		item := Model{ID: id, Label: label, Description: strings.TrimSpace(m.Description), Efforts: m.efforts()}
		if resolved == "" || resolved == id {
			out = append(out, item)
			continue
		}
		item.ResolvedID = resolved
		item.Label = label + " (latest)"
		out = append(out, item)
		if !listed[resolved] && !seen[resolved] {
			seen[resolved] = true
			out = append(out, Model{ID: resolved, Label: label, Description: item.Description, Efforts: item.Efforts})
		}
	}
	return out
}

func probeCodex(ctx context.Context) ([]Model, error) {
	bin, err := findBinary("codex")
	if err != nil {
		return nil, err
	}
	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"openpoet","title":"OpenPoet","version":"0.1.0"},"capabilities":{"experimentalApi":true}}}`,
		`{"jsonrpc":"2.0","method":"initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"model/list","params":{"limit":100,"includeHidden":false}}`,
	}
	var models []Model
	err = runJSONLines(ctx, bin, []string{"app-server"}, requests, func(line []byte) (bool, error) {
		var done bool
		var parseErr error
		models, done, parseErr = parseCodexModelList(line)
		return done, parseErr
	})
	return models, err
}

// parseCodexModelList reads the app-server reply to model/list (request id 2);
// done is false for any other line.
func parseCodexModelList(line []byte) (models []Model, done bool, err error) {
	var msg struct {
		ID     json.RawMessage           `json:"id"`
		Error  *struct{ Message string } `json:"error"`
		Result struct {
			Data []struct {
				ID          string `json:"id"`
				Model       string `json:"model"`
				DisplayName string `json:"displayName"`
				Description string `json:"description"`
				Hidden      bool   `json:"hidden"`
				IsDefault   bool   `json:"isDefault"`
				Efforts     []struct {
					ReasoningEffort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
				DefaultReasoningEffort string `json:"defaultReasoningEffort"`
			} `json:"data"`
		} `json:"result"`
	}
	if json.Unmarshal(line, &msg) != nil || string(msg.ID) != "2" {
		return nil, false, nil
	}
	if msg.Error != nil {
		return nil, false, fmt.Errorf("codex model/list failed: %s", msg.Error.Message)
	}
	for _, item := range msg.Result.Data {
		id := strings.TrimSpace(item.Model)
		if id == "" {
			id = strings.TrimSpace(item.ID)
		}
		if id == "" || item.Hidden {
			continue
		}
		label := strings.TrimSpace(item.DisplayName)
		if label == "" {
			label = id
		}
		efforts := make([]string, 0, len(item.Efforts))
		for _, e := range item.Efforts {
			efforts = append(efforts, e.ReasoningEffort)
		}
		models = append(models, Model{
			ID: id, Label: label, Description: strings.TrimSpace(item.Description), IsDefault: item.IsDefault,
			Efforts: normalizeEfforts(efforts), DefaultEffort: strings.ToLower(strings.TrimSpace(item.DefaultReasoningEffort)),
		})
	}
	return models, true, nil
}

func probeOpenCode(ctx context.Context) ([]Model, error) {
	bin, err := findBinary("opencode")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, "models")
	cmd.Env = probeEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("opencode models failed: %w", err)
	}
	return parseOpenCodeModels(string(out)), nil
}

// parseOpenCodeModels reads `opencode models` output: one provider/model per line.
func parseOpenCodeModels(out string) []Model {
	var models []Model
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		id := strings.TrimSpace(line)
		if id == "" || strings.ContainsAny(id, " \t") || !strings.Contains(id, "/") || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, Model{ID: id, Label: id})
	}
	return models
}
