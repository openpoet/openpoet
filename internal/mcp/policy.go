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

// MergePolicy combines a global policy with a project-level policy.
// If the project policy has a non-empty mode, it overrides the global policy.
// If the project policy is empty/default, the global policy is used.
func MergePolicy(global, project ToolPolicy) ToolPolicy {
	if project.Mode == "" || project.Mode == "allow_all" && len(project.Denied) == 0 && len(project.Allowed) == 0 {
		return global
	}
	return project
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
