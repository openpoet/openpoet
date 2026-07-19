package automation

import (
	"context"
	"encoding/json"
	"testing"
)

type fakeScopeStore struct{ tags map[int64][]int64 }

func (f fakeScopeStore) ProjectIDsForTags(_ context.Context, tagIDs []int64) ([]int64, error) {
	var out []int64
	for _, t := range tagIDs {
		out = append(out, f.tags[t]...)
	}
	return out, nil
}

// TestProjectFilterBlocksOutOfScope: an actor's project_filter resolves to the
// exact allowed project set; out-of-scope projects are blocked, an empty filter
// is unrestricted, and a malformed filter fails CLOSED (never opens up).
func TestProjectFilterBlocksOutOfScope(t *testing.T) {
	ctx := context.Background()
	store := fakeScopeStore{tags: map[int64][]int64{7: {10, 20}}}

	// No filter → unrestricted.
	s := resolveActorProjectScope(ctx, store, Actor{})
	if !s.Unrestricted || !s.Allows(999) {
		t.Fatal("empty filter must be unrestricted")
	}

	// tag_ids filter expands to tag membership {10,20}.
	s = resolveActorProjectScope(ctx, store, Actor{ProjectFilter: `{"tag_ids":[7]}`})
	if s.Unrestricted {
		t.Fatal("a real filter must not be unrestricted")
	}
	if !s.Allows(10) || !s.Allows(20) {
		t.Fatal("in-scope project blocked")
	}
	if s.Allows(30) {
		t.Fatal("out-of-scope project allowed")
	}
	// project_id 0 (global/session target) is always allowed — scoping is per project.
	if !s.Allows(0) {
		t.Fatal("project-less target must be allowed")
	}

	// project_ids filter.
	s = resolveActorProjectScope(ctx, store, Actor{ProjectFilter: `{"project_ids":[5]}`})
	if !s.Allows(5) || s.Allows(6) {
		t.Fatal("project_ids scope wrong")
	}

	// Malformed filter → fail closed (deny all real projects).
	s = resolveActorProjectScope(ctx, store, Actor{ProjectFilter: `{bad json`})
	if s.Unrestricted || s.Allows(10) {
		t.Fatal("malformed filter must fail closed")
	}

	// targetProjectID decoding covers project_id, project-typed id, and non-project.
	if got := targetProjectID(json.RawMessage(`{"type":"project","project_id":42}`)); got != 42 {
		t.Fatalf("project_id target = %d", got)
	}
	if got := targetProjectID(json.RawMessage(`{"type":"project","id":42}`)); got != 42 {
		t.Fatalf("project id-typed target = %d", got)
	}
	if got := targetProjectID(json.RawMessage(`{"type":"session","id":"s1"}`)); got != 0 {
		t.Fatalf("session target should be 0, got %d", got)
	}
}
