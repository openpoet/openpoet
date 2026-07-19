package automation

import (
	"context"
	"encoding/json"
	"strings"
)

// ProjectScopeStore resolves coordination-tag membership so a client's
// project_filter {"tag_ids":[...]} expands to concrete project ids. *database.DB
// implements it via the project_tags junction.
type ProjectScopeStore interface {
	ProjectIDsForTags(ctx context.Context, tagIDs []int64) ([]int64, error)
}

// ProjectScopeSet is a resolved project_filter. Unrestricted actors (no filter)
// may touch any project; otherwise Allowed is the exact set of projects they are
// scoped to, and AllowedTags the coordination groups (tag ids) named directly in
// the filter (for group-scoped resources like the blackboard).
type ProjectScopeSet struct {
	Unrestricted bool
	Allowed      map[int64]bool
	AllowedTags  map[int64]bool
}

// Allows reports whether the actor may touch projectID. A nil/unrestricted scope
// allows everything; a projectID of 0 (unknown/global target) is allowed so
// project-less capabilities are never blocked by scoping. Callers that must NOT
// treat a project-less value as allowed (e.g. list-all guards) check
// Restricted() first.
func (s *ProjectScopeSet) Allows(projectID int64) bool {
	if s == nil || s.Unrestricted {
		return true
	}
	if projectID <= 0 {
		return true
	}
	return s.Allowed[projectID]
}

// Restricted reports whether a real project_filter is in force (the actor is NOT
// unrestricted). List/broadcast surfaces use this to decide they must filter
// results rather than return everything.
func (s *ProjectScopeSet) Restricted() bool {
	return !(s == nil || s.Unrestricted)
}

// AllowsTag reports whether the actor's filter names this coordination group tag
// directly (unrestricted actors allow any tag).
func (s *ProjectScopeSet) AllowsTag(tagID int64) bool {
	if s == nil || s.Unrestricted {
		return true
	}
	if tagID <= 0 {
		return false
	}
	return s.AllowedTags[tagID]
}

type projectFilterSpec struct {
	ProjectIDs []int64 `json:"project_ids"`
	TagIDs     []int64 `json:"tag_ids"`
}

// resolveActorProjectScope expands an actor's raw project_filter into a
// ProjectScopeSet. A malformed filter fails CLOSED (deny all) rather than open —
// a broken scope must never silently grant cross-project reach.
func resolveActorProjectScope(ctx context.Context, store ProjectScopeStore, actor Actor) *ProjectScopeSet {
	raw := strings.TrimSpace(actor.ProjectFilter)
	if raw == "" || raw == "{}" || raw == "null" {
		return &ProjectScopeSet{Unrestricted: true}
	}
	var spec projectFilterSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return &ProjectScopeSet{Allowed: map[int64]bool{}} // fail closed
	}
	if len(spec.ProjectIDs) == 0 && len(spec.TagIDs) == 0 {
		return &ProjectScopeSet{Unrestricted: true}
	}
	allowed := make(map[int64]bool, len(spec.ProjectIDs))
	for _, id := range spec.ProjectIDs {
		if id > 0 {
			allowed[id] = true
		}
	}
	tags := make(map[int64]bool, len(spec.TagIDs))
	for _, id := range spec.TagIDs {
		if id > 0 {
			tags[id] = true
		}
	}
	if len(spec.TagIDs) > 0 && store != nil {
		if ids, err := store.ProjectIDsForTags(ctx, spec.TagIDs); err == nil {
			for _, id := range ids {
				allowed[id] = true
			}
		}
	}
	return &ProjectScopeSet{Allowed: allowed, AllowedTags: tags}
}

// targetProjectID decodes a capability target's project id (0 if the target does
// not name a project — e.g. a session or global target, which project scoping
// cannot generically restrict).
func targetProjectID(target json.RawMessage) int64 {
	if len(strings.TrimSpace(string(target))) == 0 {
		return 0
	}
	var t executionCommandTarget
	if err := json.Unmarshal(target, &t); err != nil {
		return 0
	}
	if t.ProjectID > 0 {
		return t.ProjectID
	}
	if (t.Type == "project" || t.Kind == "project") && len(t.ID) > 0 {
		var id int64
		if err := json.Unmarshal(t.ID, &id); err == nil {
			return id
		}
	}
	return 0
}
