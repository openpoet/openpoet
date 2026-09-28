package database

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestMatchSessionIDsExactThenEscapedPrefix(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "match.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	project := &Project{Name: "p1", Path: "/tmp/p1", Type: "local", Backend: "claude_code"}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"5ee5b896-82af", "abcdef12-0001", "abcdef13-0002", "abc_x", "abcdef12"} {
		session := &Session{ID: id, ProjectID: project.ID, Status: "running", StartTime: time.Now(),
			Backend: "claude_code", Model: "unknown", RequestedModel: "default", Effort: "default"}
		if err := db.CreateSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		value string
		want  []string
	}{
		{"5ee5b896", []string{"5ee5b896-82af"}},
		{"abcdef12", []string{"abcdef12"}}, // an exact id wins over longer ids sharing its prefix
		{"abcdef1", []string{"abcdef12", "abcdef12-0001"}},
		{"abc_", []string{"abc_x"}}, // LIKE wildcards are matched literally
		{"zzz", nil},
	}
	for _, testCase := range cases {
		got, err := db.MatchSessionIDs(ctx, testCase.value, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 && len(testCase.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, testCase.want) {
			t.Errorf("MatchSessionIDs(%q) = %v, want %v", testCase.value, got, testCase.want)
		}
	}
}
