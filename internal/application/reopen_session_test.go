package application

import (
	"context"
	"testing"

	"openpoet/internal/database"
)

type reopenBackendRecorder struct {
	phase3SessionManager
	project *database.Project
	session *database.Session
}

func (m *reopenBackendRecorder) ReopenSession(_ context.Context, sess *database.Session, project *database.Project, _ map[string]string, _ func(string, string) (string, error)) error {
	m.project, m.session = project, sess
	return nil
}

type reopenConfigRecorder struct{ backend string }

func (r *reopenConfigRecorder) SyncToProject(_ context.Context, project *database.Project) error {
	r.backend = project.Backend
	return nil
}

func TestSessionServiceReopenPreservesBackendOverride(t *testing.T) {
	for _, backend := range []string{"codex", "claude_code"} {
		t.Run(backend, func(t *testing.T) {
			projectBackend := "claude_code"
			if backend == projectBackend {
				projectBackend = "codex"
			}
			store := &phase3Store{
				project: &database.Project{ID: 7, Type: "remote", Path: "/home/dev/project", Backend: projectBackend},
				session: &database.Session{ID: "s1", ProjectID: 7, Status: "error", Backend: backend, ProviderSessionID: "original-conversation"},
			}
			manager, syncer := &reopenBackendRecorder{}, &reopenConfigRecorder{}
			service := NewSessionService(store, manager, syncer, nil, nil, nil, func(s, _ string) (string, error) { return s, nil }, nil)
			if _, err := service.Reopen(context.Background(), ReopenSessionCommand{SessionID: "s1", Authorization: phase3Actor}); err != nil {
				t.Fatal(err)
			}
			if syncer.backend != backend || manager.project.Backend != backend {
				t.Fatalf("sync backend=%q, runner backend=%q, want %q", syncer.backend, manager.project.Backend, backend)
			}
			if manager.session.ProviderSessionID != "original-conversation" || len(manager.writes) != 0 {
				t.Fatal("reopen changed conversation or submitted input")
			}
			if store.project.Backend != projectBackend {
				t.Fatal("reopen changed the saved project default")
			}
		})
	}
}

func TestSessionServiceReopenAcceptsEveryEndedStatus(t *testing.T) {
	for _, status := range []string{"stopped", "completed", "error"} {
		t.Run(status, func(t *testing.T) {
			store := &phase3Store{
				project: &database.Project{ID: 7, Type: "local", Path: "/repo"},
				session: &database.Session{ID: "s1", ProjectID: 7, Status: status},
			}
			effects := &phase3SessionEffects{}
			service := NewSessionService(store, &phase3SessionManager{}, nil, nil, nil, nil, nil, effects)

			reopened, err := service.Reopen(context.Background(), ReopenSessionCommand{SessionID: "s1", Authorization: phase3Actor})
			if err != nil || reopened == nil || reopened.ID != "s1" {
				t.Fatalf("reopen of a %s session failed: session=%v err=%v", status, reopened, err)
			}
			if len(effects.changes) != 1 || effects.changes[0].Action != "reopened" {
				t.Fatalf("reopen must publish exactly once: %#v", effects.changes)
			}
		})
	}
}

func TestSessionServiceReopenRefusesLiveSessions(t *testing.T) {
	for _, status := range []string{"starting", "running"} {
		t.Run(status, func(t *testing.T) {
			store := &phase3Store{
				project: &database.Project{ID: 7, Type: "local", Path: "/repo"},
				session: &database.Session{ID: "s1", ProjectID: 7, Status: status},
			}
			effects := &phase3SessionEffects{}
			service := NewSessionService(store, &phase3SessionManager{}, nil, nil, nil, nil, nil, effects)

			_, err := service.Reopen(context.Background(), ReopenSessionCommand{SessionID: "s1", Authorization: phase3Actor})
			if !ErrorIsKind(err, ErrorConflict) || len(effects.changes) != 0 {
				t.Fatalf("reopen of a %s session must be a conflict without effects: err=%v effects=%d", status, err, len(effects.changes))
			}
		})
	}
}
