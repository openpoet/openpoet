// Package workspace provisions per-workspace environments from a project's
// `.openpoet/environment.yaml` manifest: it runs approved setup commands,
// allocates ports, spawns per-sandbox services, and renders their coordinates
// into session env vars. Executing repo-authored commands is gated behind a
// content-hash approval (Phase 6's one load-bearing security property).
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// portTemplateRe matches {{PORT}} and {{PORT.<name>}} (with optional inner
// whitespace). In this single-port MVP every named form renders to the same
// allocated port.
var portTemplateRe = regexp.MustCompile(`\{\{\s*PORT(\.[A-Za-z0-9_-]+)?\s*\}\}`)

// Manifest is the parsed `.openpoet/environment.yaml`.
type Manifest struct {
	Version  int                        `yaml:"version"`
	Setup    []string                   `yaml:"setup"`
	Services map[string]ManifestService `yaml:"services"`
	Env      map[string]string          `yaml:"env"`
	Reuse    *ManifestReuse             `yaml:"reuse"`
}

// ManifestService declares one per-sandbox (or shared) process to run.
type ManifestService struct {
	Command string `yaml:"command"`
	Port    string `yaml:"port"`   // "{{PORT}}" (allocate) or a literal port number
	Health  string `yaml:"health"` // URL, may contain {{PORT}}
	Scope   string `yaml:"scope"`  // per-sandbox | shared
}

// ManifestReuse is the workspace pooling policy.
type ManifestReuse struct {
	Policy  string `yaml:"policy"` // pool | ephemeral
	MaxIdle int    `yaml:"max_idle"`
}

// ManifestSHA256 is the canonical content hash — the exact value pinned in the
// approval ledger. It hashes the RAW bytes, so any drift (even a comment) changes
// the hash and re-requires approval.
func ManifestSHA256(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// ParseManifest parses and lightly validates a manifest.
func ParseManifest(content []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(content, &m); err != nil {
		return nil, fmt.Errorf("manifest parse: %w", err)
	}
	if m.Version == 0 {
		return nil, errors.New("manifest: version is required")
	}
	for name, svc := range m.Services {
		if strings.TrimSpace(svc.Command) == "" {
			return nil, fmt.Errorf("manifest: service %q has no command", name)
		}
	}
	return &m, nil
}

// RenderTemplate substitutes {{PORT}} and {{PORT.<name>}} (all rendering to the
// same allocated port in this single-port MVP) — so a service whose command or
// health URL uses the per-name form binds and health-checks the real port
// instead of a literal placeholder.
func RenderTemplate(s string, port int) string {
	return portTemplateRe.ReplaceAllString(s, fmt.Sprintf("%d", port))
}

// PortIsTemplated reports whether a service's port field asks the allocator for a
// port ("{{PORT}}") vs hard-codes a literal one.
func PortIsTemplated(port string) bool {
	p := strings.TrimSpace(port)
	return p == "{{PORT}}" || p == "{{ PORT }}" || p == ""
}

// RenderedEnv returns the manifest env with {{PORT}} expanded — the trusted vars
// a session inherits (plus PORT itself).
func (m *Manifest) RenderedEnv(port int) map[string]string {
	out := map[string]string{"PORT": fmt.Sprintf("%d", port)}
	for k, v := range m.Env {
		out[k] = RenderTemplate(v, port)
	}
	return out
}
