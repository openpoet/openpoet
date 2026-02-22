package mcp

import "encoding/json"

// ToolPolicy controls which tools are available in a given context.
type ToolPolicy struct {
	Mode    string   `json:"mode"`    // "allow_all", "deny_all", "custom"
	Allowed []string `json:"allowed"` // tools allowed (used when mode is "deny_all" or "custom")
	Denied  []string `json:"denied"`  // tools denied (used when mode is "allow_all")
}

// DefaultPolicy returns a policy that allows all tools.
func DefaultPolicy() ToolPolicy {
	return ToolPolicy{Mode: "allow_all"}
}

// ParsePolicy parses a JSON string into a ToolPolicy. Returns DefaultPolicy on error or empty input.
func ParsePolicy(s string) ToolPolicy {
	if s == "" {
		return DefaultPolicy()
	}
	var p ToolPolicy
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return DefaultPolicy()
	}
	if p.Mode == "" {
		return DefaultPolicy()
	}
	return p
}

// ResolveProjectPolicy returns the effective policy for a project.
// If the project has an explicit policy (non-empty JSON), it overrides the global entirely.
// If the project has no explicit policy (empty string), the global policy is inherited.
func ResolveProjectPolicy(globalPolicy ToolPolicy, projectPolicyJSON string) ToolPolicy {
	if projectPolicyJSON != "" {
		return ParsePolicy(projectPolicyJSON)
	}
	return globalPolicy
}

// HasTools returns true if this policy would allow at least one tool.
func (p ToolPolicy) HasTools(allTools []MCPTool) bool {
	if p.Mode == "" || p.Mode == "allow_all" && len(p.Denied) == 0 {
		return len(allTools) > 0
	}
	for _, t := range allTools {
		if p.IsAllowed(t.Name) {
			return true
		}
	}
	return false
}

// IsAllowed checks if a specific tool name is permitted by this policy.
func (p ToolPolicy) IsAllowed(name string) bool {
	switch p.Mode {
	case "deny_all":
		for _, a := range p.Allowed {
			if a == name {
				return true
			}
		}
		return false
	case "custom":
		// Custom: if allowed list is non-empty, tool must be in it
		if len(p.Allowed) > 0 {
			found := false
			for _, a := range p.Allowed {
				if a == name {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		// Also check denied list
		for _, d := range p.Denied {
			if d == name {
				return false
			}
		}
		return true
	default: // "allow_all" or ""
		for _, d := range p.Denied {
			if d == name {
				return false
			}
		}
		return true
	}
}

// FilterTools returns only the tools permitted by this policy.
func (p ToolPolicy) FilterTools(tools []MCPTool) []MCPTool {
	if p.Mode == "" || p.Mode == "allow_all" && len(p.Denied) == 0 {
		return tools
	}
	var filtered []MCPTool
	for _, t := range tools {
		if p.IsAllowed(t.Name) {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

// ShareToolNames are the MCP tool names for cross-project file access.
// These are auto-allowed when the project has shares configured.
var ShareToolNames = []string{
	"openpoet_list_shared_projects",
	"openpoet_list_shared_files",
	"openpoet_read_shared_file",
	"openpoet_copy_shared_file",
	"openpoet_copy_shared_folder",
}

// AllowTools returns a copy of the policy with the given tools explicitly allowed.
// For "deny_all": adds them to the Allowed list.
// For "allow_all"/"custom": removes them from the Denied list.
func (p ToolPolicy) AllowTools(names []string) ToolPolicy {
	result := ToolPolicy{Mode: p.Mode}
	switch p.Mode {
	case "deny_all":
		allowed := make(map[string]bool)
		for _, a := range p.Allowed {
			allowed[a] = true
		}
		for _, n := range names {
			allowed[n] = true
		}
		result.Allowed = make([]string, 0, len(allowed))
		for name := range allowed {
			result.Allowed = append(result.Allowed, name)
		}
		result.Denied = append([]string{}, p.Denied...)
	default: // "allow_all", "custom", ""
		result.Allowed = append([]string{}, p.Allowed...)
		remove := make(map[string]bool)
		for _, n := range names {
			remove[n] = true
		}
		for _, d := range p.Denied {
			if !remove[d] {
				result.Denied = append(result.Denied, d)
			}
		}
	}
	return result
}

// FilterLLMTools filters LLM tool definitions (used by AI chat).
// Takes tool name extractor to work with different tool struct types.
func FilterByPolicy(policy ToolPolicy, names []string) []string {
	if policy.Mode == "" || policy.Mode == "allow_all" && len(policy.Denied) == 0 {
		return names
	}
	var filtered []string
	for _, n := range names {
		if policy.IsAllowed(n) {
			filtered = append(filtered, n)
		}
	}
	return filtered
}
