package application

import (
	"sync"
	"time"
)

// SessionTurnState says whether the agent is inside a turn: between the
// prompt it accepted (UserPromptSubmit) and the end of its reply (Stop, or the
// agent reporting it is back at the prompt). Unlike the hook-derived mode,
// it has no inactivity timer, so a long tool run still reads as an open turn.
type SessionTurnState struct {
	Open bool
	// Since is when the turn opened (Open) or closed.
	Since time.Time
	// Reason names the signal that set the state, e.g. "UserPromptSubmit" or "Stop".
	Reason string
}

// SessionTurnReader is implemented by the hook handler. The bool is false
// while no turn signal has been seen for the session since this server
// started (backends without prompt hooks, or a session restored idle).
type SessionTurnReader interface {
	SessionTurnState(sessionID string) (SessionTurnState, bool)
}

// TurnState reports the session's turn, when a turn signal has been seen.
func (s *SessionService) TurnState(sessionID string) (SessionTurnState, bool) {
	if s == nil {
		return SessionTurnState{}, false
	}
	reader, ok := s.creation.Signals.(SessionTurnReader)
	if !ok || reader == nil {
		return SessionTurnState{}, false
	}
	return reader.SessionTurnState(sessionID)
}

// sessionBusyReason returns why input must not be typed now, or "" when the
// session is between turns. A known turn state is authoritative; without one
// it falls back to the hook mode, which is all older backends report.
func (s *SessionService) sessionBusyReason(sessionID string) string {
	if turn, known := s.TurnState(sessionID); known {
		if turn.Open {
			return "Session is mid-turn (open since " + turn.Since.UTC().Format(time.RFC3339) + "); retry after its turn completes or send with force"
		}
		return ""
	}
	if s.creation.Signals != nil && s.creation.Signals.GetSessionMode(sessionID) == "executing" {
		return "Session is mid-turn; retry when it is idle or send with force"
	}
	return ""
}

// sessionInputGate lets one guarded send per session be in flight: the turn
// only opens once the agent accepts the prompt, so without it two concurrent
// sends would both see the session idle and type into each other.
type sessionInputGate struct {
	mu       sync.Mutex
	inFlight map[string]struct{}
}

func (g *sessionInputGate) acquire(sessionID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight == nil {
		g.inFlight = make(map[string]struct{})
	}
	if _, busy := g.inFlight[sessionID]; busy {
		return false
	}
	g.inFlight[sessionID] = struct{}{}
	return true
}

func (g *sessionInputGate) release(sessionID string) {
	g.mu.Lock()
	delete(g.inFlight, sessionID)
	g.mu.Unlock()
}
