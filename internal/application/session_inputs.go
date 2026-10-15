package application

import (
	"context"

	"openpoet/internal/sessioninput"
)

// SessionInputRecorder attributes the prompts a session's agent accepts to
// the sender that typed them. Implemented by the HookHandler, which sees the
// prompts; optional on SessionSignalPort.
type SessionInputRecorder interface {
	ExpectSessionInput(sessionID string, input sessioninput.Expected)
	LastSessionInput(sessionID string) (sessioninput.LastInput, bool)
}

// expectedSessionInput describes text the actor is about to type. The command
// id comes from the automation command being executed, if any.
func expectedSessionInput(ctx context.Context, text string, actor Actor, via string) sessioninput.Expected {
	return sessioninput.Expected{
		Text: text, Actor: actor.eventValue(), ActorType: actor.Type,
		CommandID: eventMetadataFromContext(ctx).CommandID, Via: via,
	}
}

func (s *SessionService) expectInput(ctx context.Context, sessionID, text string, actor Actor, via string) {
	if recorder, ok := s.creation.Signals.(SessionInputRecorder); ok {
		recorder.ExpectSessionInput(sessionID, expectedSessionInput(ctx, text, actor, via))
	}
}

// LastInput says who wrote the last prompt the session's agent accepted.
func (s *SessionService) LastInput(sessionID string) (sessioninput.LastInput, bool) {
	if s == nil {
		return sessioninput.LastInput{}, false
	}
	recorder, ok := s.creation.Signals.(SessionInputRecorder)
	if !ok {
		return sessioninput.LastInput{}, false
	}
	return recorder.LastSessionInput(sessionID)
}
