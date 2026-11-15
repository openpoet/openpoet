package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

// A proposal of the chat assistant (a change to a memory document, tasks to
// create, change or delete, a project skill, a tool to run) and the
// verification document of a task are artifacts that wait for the user's
// decision: a markdown artifact with a review (database.ArtifactReview). The
// user reads them in the artifact viewer and decides there.
//
// They are still named by a bare id where a Doc id used to be: on a chat card,
// in the approve and reject routes, in a task's verification_doc_id and in the
// /app/doc/<id> link a tool result carries. Whatever reads a document by id
// finds the artifact (application.DocumentService.GetTemp).

// RedirectDocumentLink serves GET /app/doc/{id}. A document id that stands for
// an artifact (a Doc copied into one, a proposal, a verification document) is
// redirected to that artifact's link, so links handed out when it was a Doc
// keep opening it. Every other id (a Doc, a session plan, a memory document)
// is left to the app, which next serves the page that opens it.
func (a *API) RedirectDocumentLink(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(chi.URLParam(r, "id"))
		if id != "" && len(id) <= 100 && a.db != nil {
			if artifactID, err := a.db.ResolveDocumentArtifactID(r.Context(), id); err == nil && artifactID != "" {
				w.Header().Set("Cache-Control", "no-store")
				http.Redirect(w, r, application.ArtifactLinkPath(artifactID, 0), http.StatusFound)
				return
			}
		}
		next(w, r)
	}
}

// reviewArtifactActor is the author of the artifacts the server publishes for
// the user to decide on.
var reviewArtifactActor = application.Actor{Type: "system", ID: "proposals"}

// reviewArtifact is what a proposal or a verification document is made of.
type reviewArtifact struct {
	Kind           string // database.ArtifactReview* kind
	Title          string
	Summary        string
	Markdown       string
	ProjectID      int64
	TaskID         int64
	ConversationID int64
	MessageID      int64
}

// createReviewArtifact publishes a proposal or a verification document and
// returns its id.
func (a *API) createReviewArtifact(ctx context.Context, proposal reviewArtifact) (string, error) {
	services, ready := a.platformApplicationServices()
	if !ready || services.Collaboration.Artifacts == nil {
		return "", errors.New("artifact service is unavailable")
	}
	// The title and the summary are built from names the user chose (a task, a
	// skill); they are cut to what an artifact takes rather than refused.
	command := application.CreateArtifactCommand{
		Title: clipRunes(proposal.Title, 200), Summary: clipRunes(proposal.Summary, 4000),
		Content: proposal.Markdown, Format: database.ArtifactFormatMarkdown,
		Review:        &application.ArtifactReviewRequest{Kind: proposal.Kind, MessageID: proposal.MessageID},
		Authorization: application.ActionAuthorization{Actor: reviewArtifactActor},
	}
	if proposal.ConversationID > 0 {
		command.ConversationID = &proposal.ConversationID
	}
	// A task names its project; the project alone is given otherwise.
	if proposal.TaskID > 0 {
		command.TaskID = &proposal.TaskID
	} else if proposal.ProjectID > 0 {
		command.ProjectID = &proposal.ProjectID
	}
	result, err := services.Collaboration.Artifacts.Create(ctx, command)
	if err != nil {
		return "", err
	}
	return result.Artifact.ID, nil
}

// clipRunes cuts text to at most limit runes, ending it with an ellipsis when
// it was cut.
func clipRunes(text string, limit int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

// reviewCardType is the type of the chat card of a proposal.
func reviewCardType(kind string) string {
	switch kind {
	case database.ArtifactReviewMemory:
		return "proposal"
	case database.ArtifactReviewTask:
		return "task_proposal"
	case database.ArtifactReviewSkill:
		return "skill_proposal"
	case database.ArtifactReviewTool:
		return "tool_proposal"
	}
	return "document"
}

// legacyDocCardType is the type of the chat card of a Doc: the Docs table held
// proposals and file cards too, told apart by the prefix of their title.
func legacyDocCardType(title string) string {
	switch {
	case strings.HasPrefix(title, "Memory Doc:"):
		return "proposal"
	case strings.HasPrefix(title, "Task:"), strings.HasPrefix(title, "Planning:"),
		strings.HasPrefix(title, "Update Task:"), strings.HasPrefix(title, "Delete Task:"):
		return "task_proposal"
	case strings.HasPrefix(title, "Tool:"):
		return "tool_proposal"
	case strings.HasPrefix(title, "Skill:"):
		return "skill_proposal"
	case strings.HasPrefix(title, "File:"):
		return "file"
	}
	return "document"
}

// chatDocCard is a card a chat conversation shows under one of its messages: a
// document, a proposal waiting for the user, or a file of a project.
type chatDocCard struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Title     string    `json:"title"`
	Summary   string    `json:"summary"`
	Status    string    `json:"status"`
	MessageID int64     `json:"message_id"`
	ProjectID string    `json:"project_id,omitempty"`
	Path      string    `json:"path,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// sse is the card as the doc_card event of a chat stream carries it.
func (c chatDocCard) sse() map[string]interface{} {
	card := map[string]interface{}{
		"doc_id": c.ID, "type": c.Type, "title": c.Title, "summary": c.Summary, "status": c.Status,
	}
	if c.Type == "file" {
		card["project_id"], card["path"] = c.ProjectID, c.Path
	}
	return card
}

// conversationDocCards returns the cards of a conversation, oldest first: its
// proposals (artifacts with a review), its file cards, and the Docs it still
// has from before both moved out of the Docs table.
func (a *API) conversationDocCards(ctx context.Context, conversationID int64) []chatDocCard {
	cards := []chatDocCard{}
	if conversationID <= 0 {
		return cards
	}
	legacy, _ := a.db.ListTempDocumentsByConversation(ctx, conversationID)
	for _, doc := range legacy {
		card := chatDocCard{
			ID: doc.ID, Type: legacyDocCardType(doc.Title), Title: doc.Title, Summary: doc.Summary,
			Status: doc.Status, MessageID: doc.MessageID, CreatedAt: doc.CreatedAt,
		}
		if card.Type == "file" {
			card.Title = strings.TrimPrefix(doc.Title, "File:")
			var meta struct {
				ProjectID string `json:"project_id"`
				Path      string `json:"path"`
			}
			if json.Unmarshal([]byte(doc.Content), &meta) == nil {
				card.ProjectID, card.Path = meta.ProjectID, meta.Path
			}
		}
		cards = append(cards, card)
	}
	proposals, _ := a.db.ListConversationReviewArtifacts(ctx, conversationID)
	for _, proposal := range proposals {
		cards = append(cards, chatDocCard{
			ID: proposal.ID, Type: reviewCardType(proposal.Kind), Title: proposal.Title, Summary: proposal.Summary,
			Status: proposal.Status, MessageID: proposal.MessageID, CreatedAt: proposal.CreatedAt,
		})
	}
	files, _ := a.db.ListChatFileCards(ctx, conversationID)
	for _, file := range files {
		cards = append(cards, chatDocCard{
			ID: file.ID, Type: "file", Title: path.Base(file.Path), Summary: file.Path, Status: "file",
			MessageID: file.MessageID, ProjectID: strconv.FormatInt(file.ProjectID, 10), Path: file.Path, CreatedAt: file.CreatedAt,
		})
	}
	sort.SliceStable(cards, func(i, j int) bool { return cards[i].CreatedAt.Before(cards[j].CreatedAt) })
	return cards
}

// createChatFileCard records a file card of a conversation: a file of a
// project the assistant pointed the user at.
func (a *API) createChatFileCard(ctx context.Context, conversationID, messageID, projectID int64, filePath string) error {
	if conversationID <= 0 {
		return nil // a card is shown from its conversation; there is none to show it
	}
	return a.db.CreateChatFileCard(ctx, &database.ChatFileCard{
		ID: uuid.NewString()[:8], ConversationID: conversationID, MessageID: messageID, ProjectID: projectID, Path: filePath,
	})
}
