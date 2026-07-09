package automation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type Scope string

const (
	ScopeProjectsRead      Scope = "projects:read"
	ScopeProjectsWrite     Scope = "projects:write"
	ScopeTasksRead         Scope = "tasks:read"
	ScopeTasksWrite        Scope = "tasks:write"
	ScopeSessionsRead      Scope = "sessions:read"
	ScopeSessionsControl   Scope = "sessions:control"
	ScopeSessionsUnsafe    Scope = "sessions:unsafe"
	ScopeNotificationsRead Scope = "notifications:read"
	ScopeNotificationsAck  Scope = "notifications:ack"
	ScopeEventsRead        Scope = "events:read"
	ScopeReportsRead       Scope = "reports:read"
	ScopeWorkRunsRead      Scope = "work_runs:read"
	ScopeWorkRunsWrite     Scope = "work_runs:write"
	ScopePlansRead         Scope = "plans:read"
	ScopePlansWrite        Scope = "plans:write"
)

var knownScopes = map[Scope]struct{}{
	ScopeProjectsRead:      {},
	ScopeProjectsWrite:     {},
	ScopeTasksRead:         {},
	ScopeTasksWrite:        {},
	ScopeSessionsRead:      {},
	ScopeSessionsControl:   {},
	ScopeSessionsUnsafe:    {},
	ScopeNotificationsRead: {},
	ScopeNotificationsAck:  {},
	ScopeEventsRead:        {},
	ScopeReportsRead:       {},
	ScopeWorkRunsRead:      {},
	ScopeWorkRunsWrite:     {},
	ScopePlansRead:         {},
	ScopePlansWrite:        {},
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
