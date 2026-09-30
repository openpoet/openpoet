package application

import (
	"context"
	"testing"

	"openpoet/internal/database"
)

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
