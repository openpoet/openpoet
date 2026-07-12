package application

import (
	"context"
	"database/sql"
	"errors"

	"openpoet/internal/database"
)

type SessionEventWatcherPort interface {
	StartSessionWatcher(context.Context, string) (status string, reason string, err error)
	StopSessionWatcher(context.Context, string) (stopped bool, err error)
}

type SessionWatcherChange struct {
	Action    string
	SessionID string
	Status    string
	Reason    string
	Actor     Actor
}

type SessionWatcherEffects interface {
	PublishSessionWatcherChange(context.Context, SessionWatcherChange)
}

type SessionEventWatcherService struct {
	store   SessionStore
	watcher SessionEventWatcherPort
	effects SessionWatcherEffects
}

func NewSessionEventWatcherService(store SessionStore, watcher SessionEventWatcherPort, effects SessionWatcherEffects) *SessionEventWatcherService {
	return &SessionEventWatcherService{store: store, watcher: watcher, effects: effects}
}

type StartSessionWatcherCommand struct {
	SessionID     string
	Authorization ActionAuthorization
}

type SessionWatcherResult struct {
	Status string
	Reason string
}

func (s *SessionEventWatcherService) Start(ctx context.Context, command StartSessionWatcherCommand) (SessionWatcherResult, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return SessionWatcherResult{}, err
	}
	session, err := s.getSession(ctx, command.SessionID)
	if err != nil {
		return SessionWatcherResult{}, err
	}
	if session.Status != "starting" && session.Status != "running" {
		return SessionWatcherResult{}, conflictError("session_watcher_state_invalid", "Only starting or running sessions can be watched")
	}
	if s.watcher == nil {
		return SessionWatcherResult{}, validationError("session_watcher_unavailable", "Session watcher is unavailable")
	}
	status, reason, err := s.watcher.StartSessionWatcher(ctx, session.ID)
	if err != nil {
		return SessionWatcherResult{}, err
	}
	result := SessionWatcherResult{Status: status, Reason: reason}
	s.publish(ctx, SessionWatcherChange{
		Action: "started", SessionID: session.ID, Status: status, Reason: reason, Actor: command.Authorization.Actor,
	})
	return result, nil
}

type StopSessionWatcherCommand struct {
	SessionID     string
	Authorization ActionAuthorization
}

func (s *SessionEventWatcherService) Stop(ctx context.Context, command StopSessionWatcherCommand) (SessionWatcherResult, error) {
	if err := requireExplicitActionApproval(command.Authorization); err != nil {
		return SessionWatcherResult{}, err
	}
	session, err := s.getSession(ctx, command.SessionID)
	if err != nil {
		return SessionWatcherResult{}, err
	}
	if s.watcher == nil {
		return SessionWatcherResult{}, validationError("session_watcher_unavailable", "Session watcher is unavailable")
	}
	stopped, err := s.watcher.StopSessionWatcher(ctx, session.ID)
	if err != nil {
		return SessionWatcherResult{}, err
	}
	status := "not_watching"
	if stopped {
		status = "stopped"
	}
	result := SessionWatcherResult{Status: status}
	s.publish(ctx, SessionWatcherChange{
		Action: "stopped", SessionID: session.ID, Status: status, Actor: command.Authorization.Actor,
	})
	return result, nil
}

func (s *SessionEventWatcherService) getSession(ctx context.Context, sessionID string) (*database.Session, error) {
	sessionID, err := validateActionSessionID(sessionID)
	if err != nil {
		return nil, err
	}
	session, err := s.store.GetSession(ctx, sessionID)
	if err != nil || session == nil {
		if errors.Is(err, sql.ErrNoRows) || session == nil {
			return nil, notFoundError("session_not_found", "Session not found", err)
		}
		return nil, err
	}
	return session, nil
}

func (s *SessionEventWatcherService) publish(ctx context.Context, change SessionWatcherChange) {
	if s.effects != nil {
		s.effects.PublishSessionWatcherChange(ctx, change)
	}
}
