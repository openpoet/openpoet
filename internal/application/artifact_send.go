package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"openpoet/internal/database"
)

// ArtifactAgentInput types a line into a running session. SessionService
// implements it; the artifact service only needs this one method of it.
type ArtifactAgentInput interface {
	SendInput(ctx context.Context, command SendSessionInputCommand) error
}

// SetAgentInput installs the way "Send to agent" reaches a session. Without
// one, SendAnnotationsToAgent refuses instead of recording a send that did not
// happen.
func (s *ArtifactService) SetAgentInput(input ArtifactAgentInput) {
	s.agentInput = input
}

type SendArtifactAnnotationsCommand struct {
	ArtifactID    string
	Authorization ActionAuthorization
}

// ArtifactSendResult is what "Send to agent" did: which session was told, when,
// about how many annotations, and the open annotations as they are stored now
// (each one carrying sent_at and sent_session_id).
type ArtifactSendResult struct {
	ArtifactID  string                   `json:"artifact_id"`
	SessionID   string                   `json:"session_id"`
	SentAt      time.Time                `json:"sent_at"`
	Sent        int                      `json:"sent"`
	Annotations []ArtifactAnnotationView `json:"annotations"`
}

// SendAnnotationsToAgent tells the session that published the artifact that
// there are open annotations to address, and records on each of them that the
// session was told, and when. The annotations are marked only after the line
// was typed into the session: a failed send marks nothing, so what the viewer
// shows as sent is what reached the session.
//
// The line carries counts and no annotation text: the agent reads the
// annotations through its tools.
func (s *ArtifactService) SendAnnotationsToAgent(ctx context.Context, command SendArtifactAnnotationsCommand) (ArtifactSendResult, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return ArtifactSendResult{}, err
	}
	// The button is the user's: a session pressing it would be marking the
	// user's notes as delivered on the user's behalf.
	if strings.TrimSpace(command.Authorization.Actor.Type) == "session" {
		return ArtifactSendResult{}, validationError("annotation_author_not_allowed", "Annotations are sent to the agent by the user reviewing the artifact, not by a session")
	}
	id, err := normalizeArtifactID(command.ArtifactID)
	if err != nil {
		return ArtifactSendResult{}, err
	}
	artifact, err := s.artifact(ctx, id)
	if err != nil {
		return ArtifactSendResult{}, err
	}
	sessionID := strings.TrimSpace(artifact.SessionID)
	if sessionID == "" {
		return ArtifactSendResult{}, validationError("artifact_has_no_session", "This artifact was not published by a session, so there is no agent to send to")
	}
	open, err := s.store.ListArtifactAnnotations(ctx, id, "open")
	if err != nil {
		return ArtifactSendResult{}, err
	}
	if len(open) == 0 {
		return ArtifactSendResult{}, validationError("no_open_annotations", "This artifact has no open annotations to send")
	}
	if s.agentInput == nil {
		return ArtifactSendResult{}, conflictError("session_input_unavailable", "Sending input to sessions is unavailable")
	}
	if err := s.agentInput.SendInput(ctx, SendSessionInputCommand{
		SessionID: sessionID, Text: artifactSendMessage(artifact, open), Authorization: command.Authorization,
	}); err != nil {
		return ArtifactSendResult{}, err
	}

	// The line is typed: what is recorded must not depend on the caller
	// staying connected.
	ctx = context.WithoutCancel(ctx)
	ids := make([]int64, 0, len(open))
	for i := range open {
		ids = append(ids, open[i].ID)
	}
	if _, err := s.store.MarkArtifactAnnotationsSent(ctx, id, ids, sessionID); err != nil {
		return ArtifactSendResult{}, fmt.Errorf("the session was told, but the send could not be recorded: %w", err)
	}
	stored, err := s.store.ListArtifactAnnotations(ctx, id, "open")
	if err != nil {
		return ArtifactSendResult{}, err
	}
	result := ArtifactSendResult{ArtifactID: id, SessionID: sessionID, Annotations: make([]ArtifactAnnotationView, 0, len(stored))}
	announced := make(map[int64]bool, len(ids))
	for _, annotationID := range ids {
		announced[annotationID] = true
	}
	for i := range stored {
		view := artifactAnnotationView(&stored[i])
		result.Annotations = append(result.Annotations, view)
		if announced[view.ID] && view.SentAt != nil {
			result.Sent++
			if view.SentAt.After(result.SentAt) {
				result.SentAt = *view.SentAt
			}
		}
	}
	s.annotationsChanged(ctx, artifact, "annotations_sent")
	return result, nil
}

// artifactSendMessage is the line typed into the session. It says where it
// comes from: it arrives like user input, but it is OpenPoet reporting a button
// press. The title is the only free text in it, kept to one printable line.
func artifactSendMessage(artifact *database.Artifact, open []database.ArtifactAnnotation) string {
	answers := 0
	for i := range open {
		if open[i].Kind == database.ArtifactAnnotationKindAnswer {
			answers++
		}
	}
	notes := len(open) - answers
	plural := func(count int, one, many string) string {
		if count == 1 {
			return fmt.Sprintf("%d %s", count, one)
		}
		return fmt.Sprintf("%d %s", count, many)
	}
	parts := []string{}
	if answers > 0 {
		parts = append(parts, plural(answers, "answer", "answers")+" to your questions")
	}
	if notes > 0 {
		parts = append(parts, plural(notes, "note", "notes"))
	}
	verb := "are"
	if len(open) == 1 {
		verb = "is"
	}
	return fmt.Sprintf(`[OpenPoet] The user pressed "Send to agent" on the HTML artifact "%s" (artifact_id %s, current version %d): %s %s open. `+
		`Read them with openpoet_list_artifact_annotations. For each one, act on it (publish a corrected version with openpoet_update_artifact `+
		`when the document has to change) and resolve it with openpoet_resolve_artifact_annotation saying what you did, `+
		`or leave it open and tell the user why.`,
		singleLine(artifact.Title), artifact.ID, artifact.CurrentVersion, strings.Join(parts, " and "), verb)
}
