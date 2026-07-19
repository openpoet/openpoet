package automation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type Scope string

const (
	ScopeProjectsRead       Scope = "projects:read"
	ScopeProjectsWrite      Scope = "projects:write"
	ScopeTasksRead          Scope = "tasks:read"
	ScopeTasksWrite         Scope = "tasks:write"
	ScopeSessionsRead       Scope = "sessions:read"
	ScopeSessionsControl    Scope = "sessions:control"
	ScopeSessionsUnsafe     Scope = "sessions:unsafe"
	ScopeNotificationsRead  Scope = "notifications:read"
	ScopeNotificationsAck   Scope = "notifications:ack"
	ScopeEventsRead         Scope = "events:read"
	ScopeConflictsRead      Scope = "conflicts:read"
	ScopeWorkspacesRead     Scope = "workspaces:read"
	ScopeWorkspacesWrite    Scope = "workspaces:write"
	ScopeReportsRead        Scope = "reports:read"
	ScopeWorkRunsRead       Scope = "work_runs:read"
	ScopeWorkRunsWrite      Scope = "work_runs:write"
	ScopePlansRead          Scope = "plans:read"
	ScopePlansWrite         Scope = "plans:write"
	ScopeApprovalsGrant     Scope = "approvals:grant"
	ScopeApprovalsSelf      Scope = "approvals:self"
	ScopeAgentsRead         Scope = "agents:read"
	ScopeAgentsWrite        Scope = "agents:write"
	ScopeAIUse              Scope = "ai:use"
	ScopeAIRead             Scope = "ai:read"
	ScopeAIWrite            Scope = "ai:write"
	ScopeAIConfigsRead      Scope = "ai_configs:read"
	ScopeAIConfigsWrite     Scope = "ai_configs:write"
	ScopeConfigWrite        Scope = "config:write"
	ScopeCredentialsRead    Scope = "credentials:read"
	ScopeCredentialsUse     Scope = "credentials:use"
	ScopeCredentialsWrite   Scope = "credentials:write"
	ScopeDocumentsRead      Scope = "documents:read"
	ScopeDocumentsWrite     Scope = "documents:write"
	ScopeFilesRead          Scope = "files:read"
	ScopeFilesWrite         Scope = "files:write"
	ScopeGitRead            Scope = "git:read"
	ScopeGitWrite           Scope = "git:write"
	ScopeHooksRespond       Scope = "hooks:respond"
	ScopeMCPRead            Scope = "mcp:read"
	ScopeMCPWrite           Scope = "mcp:write"
	ScopeNotificationsWrite Scope = "notifications:write"
	ScopeSettingsRead       Scope = "settings:read"
	ScopeSettingsWrite      Scope = "settings:write"
	ScopeSkillsRead         Scope = "skills:read"
	ScopeSkillsWrite        Scope = "skills:write"
	ScopeTagsRead           Scope = "tags:read"
	ScopeTagsWrite          Scope = "tags:write"
	ScopeTokenUsageRead     Scope = "token_usage:read"
	ScopeTokenUsageWrite    Scope = "token_usage:write"
	ScopeToolsExecute       Scope = "tools:execute"
	ScopeToolsPolicy        Scope = "tools:policy"
	ScopeToolsRead          Scope = "tools:read"
	ScopeToolsWrite         Scope = "tools:write"
	ScopeTunnelRead         Scope = "tunnel:read"
	ScopeTunnelAdmin        Scope = "tunnel:admin"
	ScopeUpdateRead         Scope = "update:read"
	ScopeUpdateExecute      Scope = "update:execute"
	ScopeVoiceUse           Scope = "voice:use"
	ScopeSessionsWrite      Scope = "sessions:write"
)

var knownScopes = map[Scope]struct{}{
	ScopeProjectsRead:       {},
	ScopeProjectsWrite:      {},
	ScopeTasksRead:          {},
	ScopeTasksWrite:         {},
	ScopeSessionsRead:       {},
	ScopeSessionsControl:    {},
	ScopeSessionsUnsafe:     {},
	ScopeNotificationsRead:  {},
	ScopeNotificationsAck:   {},
	ScopeEventsRead:         {},
	ScopeConflictsRead:      {},
	ScopeWorkspacesRead:     {},
	ScopeWorkspacesWrite:    {},
	ScopeReportsRead:        {},
	ScopeWorkRunsRead:       {},
	ScopeWorkRunsWrite:      {},
	ScopePlansRead:          {},
	ScopePlansWrite:         {},
	ScopeApprovalsGrant:     {},
	ScopeApprovalsSelf:      {},
	ScopeAgentsRead:         {},
	ScopeAgentsWrite:        {},
	ScopeAIUse:              {},
	ScopeAIRead:             {},
	ScopeAIWrite:            {},
	ScopeAIConfigsRead:      {},
	ScopeAIConfigsWrite:     {},
	ScopeConfigWrite:        {},
	ScopeCredentialsRead:    {},
	ScopeCredentialsUse:     {},
	ScopeCredentialsWrite:   {},
	ScopeDocumentsRead:      {},
	ScopeDocumentsWrite:     {},
	ScopeFilesRead:          {},
	ScopeFilesWrite:         {},
	ScopeGitRead:            {},
	ScopeGitWrite:           {},
	ScopeHooksRespond:       {},
	ScopeMCPRead:            {},
	ScopeMCPWrite:           {},
	ScopeNotificationsWrite: {},
	ScopeSettingsRead:       {},
	ScopeSettingsWrite:      {},
	ScopeSkillsRead:         {},
	ScopeSkillsWrite:        {},
	ScopeTagsRead:           {},
	ScopeTagsWrite:          {},
	ScopeTokenUsageRead:     {},
	ScopeTokenUsageWrite:    {},
	ScopeToolsExecute:       {},
	ScopeToolsPolicy:        {},
	ScopeToolsRead:          {},
	ScopeToolsWrite:         {},
	ScopeTunnelRead:         {},
	ScopeTunnelAdmin:        {},
	ScopeUpdateRead:         {},
	ScopeUpdateExecute:      {},
	ScopeVoiceUse:           {},
	ScopeSessionsWrite:      {},
}

type ScopeSet map[Scope]struct{}

func NewScopeSet(scopes ...Scope) (ScopeSet, error) {
	set := make(ScopeSet, len(scopes))
	for _, scope := range scopes {
		scope = Scope(strings.TrimSpace(string(scope)))
		if _, ok := knownScopes[scope]; !ok {
			return nil, fmt.Errorf("unknown automation scope %q", scope)
		}
		set[scope] = struct{}{}
	}
	return set, nil
}

func ParseScopeSet(raw string) (ScopeSet, error) {
	var values []Scope
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, fmt.Errorf("parse automation scopes: %w", err)
	}
	return NewScopeSet(values...)
}

func (s ScopeSet) Has(scope Scope) bool {
	_, ok := s[scope]
	return ok
}

func (s ScopeSet) HasAll(scopes ...Scope) bool {
	for _, scope := range scopes {
		if !s.Has(scope) {
			return false
		}
	}
	return true
}

func (s ScopeSet) MarshalJSON() ([]byte, error) {
	values := make([]string, 0, len(s))
	for scope := range s {
		values = append(values, string(scope))
	}
	sort.Strings(values)
	return json.Marshal(values)
}
