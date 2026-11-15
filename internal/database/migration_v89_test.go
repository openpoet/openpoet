package database

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
)

func policyAllowed(t *testing.T, policy string) []string {
	t.Helper()
	var decoded struct {
		Allowed []string `json:"allowed"`
	}
	if err := json.Unmarshal([]byte(policy), &decoded); err != nil {
		t.Fatalf("policy is not JSON: %s", policy)
	}
	sort.Strings(decoded.Allowed)
	return decoded.Allowed
}

func TestMigrationV89GrantsArtifactToolsWhereTheDocsToolsWereGranted(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	project := &Project{Name: "docs-only", Path: "/tmp/docs-only", Type: "local"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	other := &Project{Name: "open", Path: "/tmp/open", Type: "local"}
	if err := db.CreateProject(ctx, other); err != nil {
		t.Fatal(err)
	}
	// Allowlists written after the earlier migrations: only the Docs tools.
	if _, err := db.Exec(`UPDATE projects SET tool_policy = ? WHERE id = ?`,
		`{"mode":"deny_all","allowed":["openpoet_create_document","openpoet_get_my_task"]}`, project.ID); err != nil {
		t.Fatal(err)
	}
	const allowsAll = `{"mode":"allow_all","denied":["openpoet_delete_task"]}`
	if _, err := db.Exec(`UPDATE projects SET tool_policy = ? WHERE id = ?`, allowsAll, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(ctx, "mcp_tool_policy_chat", `{"mode":"custom","allowed":["read_document","list_tasks"],"denied":[]}`); err != nil {
		t.Fatal(err)
	}

	migrate := func() {
		t.Helper()
		tx, err := db.Beginx()
		if err != nil {
			t.Fatal(err)
		}
		if err := migrateV89(tx); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	migrate()

	var projectPolicy, otherPolicy string
	if err := db.Get(&projectPolicy, `SELECT tool_policy FROM projects WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&otherPolicy, `SELECT tool_policy FROM projects WHERE id = ?`, other.ID); err != nil {
		t.Fatal(err)
	}
	chatPolicy, err := db.GetSetting(ctx, "mcp_tool_policy_chat")
	if err != nil {
		t.Fatal(err)
	}

	// Creating documents: the tools that publish and revise an artifact. The
	// retired name stays, so a caller that still names it is answered.
	wantProject := []string{"openpoet_create_artifact", "openpoet_create_document", "openpoet_get_my_task", "openpoet_patch_artifact", "openpoet_resolve_artifact_annotation", "openpoet_update_artifact"}
	if got := policyAllowed(t, projectPolicy); !equalStrings(got, wantProject) {
		t.Fatalf("project allowlist after V89 = %v, want %v", got, wantProject)
	}
	// Reading documents: the tools that read and list artifacts, and get_link. Chat names have no prefix.
	wantChat := []string{"get_link", "list_artifact_annotations", "list_artifacts", "list_tasks", "read_artifact", "read_document"}
	if got := policyAllowed(t, chatPolicy); !equalStrings(got, wantChat) {
		t.Fatalf("chat allowlist after V89 = %v, want %v", got, wantChat)
	}
	// A policy that allows everything already allows them: it is not touched.
	if otherPolicy != allowsAll {
		t.Fatalf("an allow-all policy was rewritten: %s", otherPolicy)
	}

	// A second run changes nothing.
	migrate()
	var again string
	if err := db.Get(&again, `SELECT tool_policy FROM projects WHERE id = ?`, project.ID); err != nil || again != projectPolicy {
		t.Fatalf("a second run changed the policy: %s (err %v)", again, err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
