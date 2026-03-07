package llm

import (
	"encoding/json"
	"log"
	"sync"
)

// Slot represents an AI operation category.
type Slot string

const (
	SlotChat       Slot = "ai_chat"
	SlotBackground Slot = "ai_background"
	SlotSession    Slot = "claude_session"
)

// ProviderConfig holds the decoded configuration needed to create a provider.
type ProviderConfig struct {
	ProviderType string `json:"provider_type"`
	APIKey       string `json:"api_key"` // decrypted plaintext (never stored)
	Model        string `json:"model"`
	BaseURL      string `json:"base_url"`
	ExtraJSON    string `json:"extra_json"` // raw JSON for future use (e.g. budget)
}

// extraConfig is the parsed form of ExtraJSON.
type extraConfig struct {
	BudgetUSD float64 `json:"budget_usd,omitempty"`
}

// ProviderManager manages per-slot providers, creating/caching them as needed.
// It replaces the single global provider from the old architecture.
type ProviderManager struct {
	mu                  sync.RWMutex
	configs             map[Slot]*ProviderConfig
	providers           map[Slot]Provider
	apiURL              string
	toolExecutor        ToolExecutor
	customToolsProvider CustomToolsProvider
}

// NewProviderManager creates a new ProviderManager.
func NewProviderManager(apiURL string) *ProviderManager {
	return &ProviderManager{
		configs:   make(map[Slot]*ProviderConfig),
		providers: make(map[Slot]Provider),
		apiURL:    apiURL,
	}
}

// SetSlotConfig updates the configuration for a slot and invalidates the cached provider.
func (pm *ProviderManager) SetSlotConfig(slot Slot, config *ProviderConfig) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Close existing provider if it's a SessionProvider
	if existing, ok := pm.providers[slot]; ok {
		if sp, ok := existing.(SessionProvider); ok {
			sp.CloseAllSessions()
		}
	}

	pm.configs[slot] = config
	delete(pm.providers, slot) // invalidate cache
}

// GetProvider returns the provider for the given slot, creating it lazily if needed.
// Returns nil if no config is set and auto-detect fails.
func (pm *ProviderManager) GetProvider(slot Slot) Provider {
	pm.mu.RLock()
	if p, ok := pm.providers[slot]; ok {
		pm.mu.RUnlock()
		return p
	}
	pm.mu.RUnlock()

	// Need to create — upgrade to write lock
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Double-check after acquiring write lock
	if p, ok := pm.providers[slot]; ok {
		return p
	}

	config := pm.configs[slot]
	p := pm.createProvider(config, slot)
	if p != nil {
		pm.providers[slot] = p
	}
	return p
}

// GetSlotConfig returns the configuration for a slot, or nil if not set.
func (pm *ProviderManager) GetSlotConfig(slot Slot) *ProviderConfig {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.configs[slot]
}

// SetToolExecutor stores the tool executor and wires it into any existing GoSDK providers.
func (pm *ProviderManager) SetToolExecutor(executor ToolExecutor) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.toolExecutor = executor

	// Wire into existing GoSDK providers
	for _, p := range pm.providers {
		if goSDK, ok := p.(*GoSDKProvider); ok {
			goSDK.SetToolExecutor(executor)
		}
	}
}

// SetCustomToolsProvider stores the custom tools provider and wires it into any existing GoSDK providers.
func (pm *ProviderManager) SetCustomToolsProvider(provider CustomToolsProvider) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.customToolsProvider = provider

	for _, p := range pm.providers {
		if goSDK, ok := p.(*GoSDKProvider); ok {
			goSDK.SetCustomToolsProvider(provider)
		}
	}
}

// DisconnectSession disconnects a specific conversation session on the chat provider,
// preserving the session ID for resume. Forces MCP server rebuild on next query.
func (pm *ProviderManager) DisconnectSession(conversationID int64) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	if p, ok := pm.providers[SlotChat]; ok {
		if sp, ok := p.(SessionProvider); ok {
			sp.DisconnectSession(conversationID)
		}
	}
}

// ReinitAll clears all cached providers, forcing them to be recreated on next access.
func (pm *ProviderManager) ReinitAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Close existing session providers
	for _, p := range pm.providers {
		if sp, ok := p.(SessionProvider); ok {
			sp.CloseAllSessions()
		}
	}
	pm.providers = make(map[Slot]Provider)
}

// CloseAll closes all active providers (for shutdown).
func (pm *ProviderManager) CloseAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, p := range pm.providers {
		if sp, ok := p.(SessionProvider); ok {
			sp.CloseAllSessions()
		}
	}
	pm.providers = make(map[Slot]Provider)
	pm.configs = make(map[Slot]*ProviderConfig)
}

// createProvider creates a provider from configuration. Must be called under write lock.
func (pm *ProviderManager) createProvider(config *ProviderConfig, slot Slot) Provider {
	if config == nil {
		return pm.autoDetect(slot)
	}

	var extra extraConfig
	if config.ExtraJSON != "" && config.ExtraJSON != "{}" {
		_ = json.Unmarshal([]byte(config.ExtraJSON), &extra)
	}

	switch config.ProviderType {
	case "apikey":
		if config.APIKey == "" {
			log.Printf("[ProviderMgr] Slot %s: apikey provider has no API key, falling back", slot)
			return pm.autoDetect(slot)
		}
		log.Printf("[ProviderMgr] Slot %s: using Anthropic API key provider", slot)
		return NewAnthropicProvider(config.APIKey)

	case "gosdk":
		if !IsClaudeCLIAvailable() {
			log.Printf("[ProviderMgr] Slot %s: gosdk provider but Claude CLI not available", slot)
			return nil
		}
		p := NewGoSDKProvider(pm.apiURL)
		if extra.BudgetUSD > 0 {
			p.SetBudgetUSD(extra.BudgetUSD)
		}
		if pm.toolExecutor != nil {
			p.SetToolExecutor(pm.toolExecutor)
		}
		if pm.customToolsProvider != nil {
			p.SetCustomToolsProvider(pm.customToolsProvider)
		}
		log.Printf("[ProviderMgr] Slot %s: using Go Agent SDK provider", slot)
		return p

	case "ollama":
		baseURL := config.BaseURL
		if baseURL == "" {
			baseURL = "http://localhost:11434"
		}
		model := config.Model
		if model == "" {
			model = "qwen3-coder"
		}
		log.Printf("[ProviderMgr] Slot %s: using Ollama provider at %s model=%s", slot, baseURL, model)
		return NewOllamaProvider(baseURL, config.APIKey, model)

	case "ollama-sdk":
		if !IsClaudeCLIAvailable() {
			log.Printf("[ProviderMgr] Slot %s: ollama-sdk but Claude CLI not available", slot)
			return nil
		}
		baseURL := config.BaseURL
		if baseURL == "" {
			baseURL = "http://localhost:11434"
		}
		model := config.Model
		if model == "" {
			model = "qwen3-coder"
		}
		env := map[string]string{
			"ANTHROPIC_BASE_URL":             baseURL,
			"ANTHROPIC_API_KEY":              "",
			"ANTHROPIC_DEFAULT_OPUS_MODEL":   model,
			"ANTHROPIC_DEFAULT_SONNET_MODEL": model,
			"ANTHROPIC_DEFAULT_HAIKU_MODEL":  model,
			"CLAUDE_CODE_SUBAGENT_MODEL":     model,
		}
		if config.APIKey != "" {
			env["ANTHROPIC_AUTH_TOKEN"] = config.APIKey
		} else {
			env["ANTHROPIC_AUTH_TOKEN"] = "ollama"
		}
		p := NewGoSDKProvider(pm.apiURL)
		p.SetExtraEnv(env)
		p.SetProviderName("ollama-sdk")
		if extra.BudgetUSD > 0 {
			p.SetBudgetUSD(extra.BudgetUSD)
		}
		if pm.toolExecutor != nil {
			p.SetToolExecutor(pm.toolExecutor)
		}
		if pm.customToolsProvider != nil {
			p.SetCustomToolsProvider(pm.customToolsProvider)
		}
		log.Printf("[ProviderMgr] Slot %s: using Ollama via Go Agent SDK at %s model=%s", slot, baseURL, model)
		return p

	default:
		log.Printf("[ProviderMgr] Slot %s: unknown provider type %q, falling back", slot, config.ProviderType)
		return pm.autoDetect(slot)
	}
}

// autoDetect tries to create a provider using auto-detection (Claude CLI first, then nothing).
func (pm *ProviderManager) autoDetect(slot Slot) Provider {
	if IsClaudeCLIAvailable() {
		p := NewGoSDKProvider(pm.apiURL)
		if pm.toolExecutor != nil {
			p.SetToolExecutor(pm.toolExecutor)
		}
		if pm.customToolsProvider != nil {
			p.SetCustomToolsProvider(pm.customToolsProvider)
		}
		log.Printf("[ProviderMgr] Slot %s: auto-detected Go Agent SDK (Claude CLI)", slot)
		return p
	}
	log.Printf("[ProviderMgr] Slot %s: no provider available (no config, no Claude CLI)", slot)
	return nil
}
