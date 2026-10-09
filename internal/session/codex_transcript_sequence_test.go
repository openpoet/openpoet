package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"openpoet/internal/database"
)

func TestCodexTranscriptSequenceIDsKeepsOneSequenceUnchanged(t *testing.T) {
	ids := []int{1, 2, 2, 3, 2, 4}
	appends := []bool{false, false, true, false, true, false}
	if got := CodexTranscriptSequenceIDs(ids, appends); !reflect.DeepEqual(got, ids) {
		t.Fatalf("ids = %v, want unchanged %v", got, ids)
	}
}

func TestCodexTranscriptSequenceIDsShiftsARestartedSequence(t *testing.T) {
	// Old run 1..3 (3 streamed), then a runner that started without the
	// saved history numbered from 1 again (1 streamed).
	ids := []int{1, 2, 3, 3, 1, 1, 2}
	appends := []bool{false, false, false, true, false, true, false}
	want := []int{1, 2, 3, 3, 4, 4, 5}
	if got := CodexTranscriptSequenceIDs(ids, appends); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestMergeCodexTranscriptEventsKeepsRestartedSequenceApart(t *testing.T) {
	merged := mergeCodexTranscriptEvents([]codexTranscriptEvent{
		{ID: 1, Kind: "user", Text: "old question"},
		{ID: 2, Kind: "assistant", Text: "old answer"},
		{ID: 1, Kind: "warning", Text: "after reopen"},
		{ID: 2, Kind: "assistant", Text: "new "},
		{ID: 2, Kind: "assistant", Text: "answer", Append: true},
	})
	var got []string
	for _, event := range merged {
		got = append(got, event.Kind+":"+event.Text)
	}
	want := []string{"user:old question", "assistant:old answer", "warning:after reopen", "assistant:new answer"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
}

// TestRemoteCodexReopenRestoresPersistedTranscript: a reopened remote Codex
// session gets its saved history back, including rows an earlier runner
// wrote after restarting its numbering, and new events number after it.
func TestRemoteCodexReopenRestoresPersistedTranscript(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(filepath.Join(t.TempDir(), "remote-codex-history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	project := &database.Project{
		Name: "remote-codex", Path: "/home/dev/project", Type: "remote", Backend: string(BackendCodex),
		BackendConfig: `{"runtime":"app-server"}`,
		SSHHost:       sql.NullString{String: "192.0.2.10", Valid: true}, SSHPort: sql.NullInt64{Int64: 22, Valid: true},
		SSHUser: sql.NullString{String: "dev", Valid: true}, SSHAuthType: sql.NullString{String: "password", Valid: true},
	}
	if err := db.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	sess := &database.Session{ID: "remote-codex-session", ProjectID: project.ID, Status: "running", StartTime: time.Now(), Backend: string(BackendCodex)}
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	rows := []database.CodexTranscriptEvent{
		{EventID: 1, Kind: "user", Text: "first question"},
		{EventID: 2, Kind: "assistant", Text: "first "},
		{EventID: 2, Kind: "assistant", Text: "answer", Append: true},
		// an earlier reopen that lost the history numbered from 1 again
		{EventID: 1, Kind: "warning", Text: "reopened"},
		{EventID: 2, Kind: "user", Text: "second question"},
		{EventID: 3, Kind: "assistant", Text: "second answer"},
	}
	for i := range rows {
		rows[i].SessionID = sess.ID
		rows[i].CreatedAt = time.Now().Add(time.Duration(i) * time.Second)
		if err := db.InsertCodexTranscriptEvent(ctx, &rows[i]); err != nil {
			t.Fatal(err)
		}
	}

	runner, err := NewRemoteCodexRunner(project, map[string]string{}, func([]byte) {}, &SessionConfig{SessionID: sess.ID}, db, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// No SSH password: Start fails before dialing, after loading the history.
	if err := runner.Start(ctx); err == nil || !strings.Contains(err.Error(), "SSH") {
		t.Fatalf("Start err = %v, want the SSH config error", err)
	}

	snapshot, _ := runner.inner.codexTranscriptSnapshot()["events"].([]codexTranscriptEvent)
	var got []string
	for _, event := range snapshot {
		got = append(got, event.Kind+":"+event.Text)
	}
	want := []string{"user:first question", "assistant:first answer", "warning:reopened", "user:second question", "assistant:second answer"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("history after reopen = %v, want %v", got, want)
	}

	runner.inner.addCodexTranscriptBlock("user", "third question", "", "", "")
	if seq := runner.inner.transcriptSeq; seq != 6 {
		t.Fatalf("new event numbered %d, want 6 (after the restored history)", seq)
	}
	persisted, err := db.ListCodexTranscriptEvents(ctx, sess.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if last := persisted[len(persisted)-1]; last.EventID != 6 || last.Text != "third question" {
		t.Fatalf("last persisted row = %+v, want event 6", last)
	}
}
