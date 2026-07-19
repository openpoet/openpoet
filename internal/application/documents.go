package application

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"openpoet/internal/database"
)

type DocumentStore interface {
	GetProject(context.Context, int64) (*database.Project, error)
	GetMemoryDoc(context.Context, int64) (*database.MemoryDoc, error)
	UpsertMemoryDoc(context.Context, int64, string, string, string) (*database.MemoryDoc, error)
	CreateTempDocument(context.Context, *database.TempDocument) error
	GetTempDocument(context.Context, string) (*database.TempDocument, error)
}

// MemoryDocumentMirror is a post-commit effect. Implementations should record
// their own delivery failure; database state remains the source of truth.
type MemoryDocumentMirror interface {
	MirrorMemoryDocument(context.Context, *database.Project, string)
}

type MemoryDocumentView struct {
	ProjectID    int64     `json:"project_id"`
	Content      string    `json:"content"`
	Version      int       `json:"version"`
	UpdatedBy    string    `json:"last_updated_by,omitempty"`
	Summary      string    `json:"summary,omitempty"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
	Exists       bool      `json:"exists"`
	WasTruncated bool      `json:"was_truncated,omitempty"`
}

type TempDocumentView struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	Content        string    `json:"content,omitempty"`
	Summary        string    `json:"summary,omitempty"`
	Status         string    `json:"status"`
	ConversationID *int64    `json:"conversation_id,omitempty"`
	TaskID         *int64    `json:"task_id,omitempty"`
	SessionID      string    `json:"session_id,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	WasTruncated   bool      `json:"was_truncated,omitempty"`
}

type CreateTempDocumentCommand struct {
	Title          string
	Content        string
	Summary        string
	ConversationID *int64
	TaskID         *int64
	MissionID      *int64
	SessionID      string
	Authorization  ActionAuthorization
}

type UpdateMemoryDocumentCommand struct {
	ProjectID     int64
	Content       string
	Summary       string
	Authorization ActionAuthorization
}

type DocumentService struct {
	store   DocumentStore
	mirror  MemoryDocumentMirror
	effects ApplicationEffects
}

func NewDocumentService(store DocumentStore, mirror MemoryDocumentMirror, effects ApplicationEffects) *DocumentService {
	return &DocumentService{store: store, mirror: mirror, effects: effects}
}

func (s *DocumentService) CapabilityServiceName() CapabilityServiceName {
	return CapabilityServiceName("documents")
}

func (s *DocumentService) GetMemory(ctx context.Context, projectID int64) (MemoryDocumentView, error) {
	if err := validateBoundedID(projectID, "Project"); err != nil {
		return MemoryDocumentView{}, err
	}
	if _, err := s.project(ctx, projectID); err != nil {
		return MemoryDocumentView{}, err
	}
	doc, err := s.store.GetMemoryDoc(ctx, projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MemoryDocumentView{ProjectID: projectID, Exists: false}, nil
		}
		return MemoryDocumentView{}, err
	}
	return memoryDocumentView(doc), nil
}

func (s *DocumentService) UpdateMemory(ctx context.Context, command UpdateMemoryDocumentCommand) (MemoryDocumentView, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return MemoryDocumentView{}, err
	}
	project, err := s.project(ctx, command.ProjectID)
	if err != nil {
		return MemoryDocumentView{}, err
	}
	content, err := boundedRedactedInput(command.Content, maxApplicationContentRunes, "Memory document content", false)
	if err != nil {
		return MemoryDocumentView{}, err
	}
	summary, err := boundedRedactedInput(command.Summary, maxApplicationSummaryRunes, "Memory document summary", false)
	if err != nil {
		return MemoryDocumentView{}, err
	}
	updatedBy := command.Authorization.Actor.eventValue()
	doc, err := s.store.UpsertMemoryDoc(ctx, command.ProjectID, content, updatedBy, summary)
	if err != nil {
		return MemoryDocumentView{}, err
	}
	if s.mirror != nil {
		s.mirror.MirrorMemoryDocument(ctx, project, content)
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "documents", Action: "memory_updated", ID: command.ProjectID, Meta: map[string]any{"version": doc.Version}})
	return memoryDocumentView(doc), nil
}

func (s *DocumentService) CreateTemp(ctx context.Context, command CreateTempDocumentCommand) (TempDocumentView, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return TempDocumentView{}, err
	}
	title, err := boundedRedactedInput(command.Title, maxApplicationTitleRunes, "Document title", false)
	if err != nil {
		return TempDocumentView{}, err
	}
	if title == "" {
		title = "Documento"
	}
	content, err := boundedRedactedInput(command.Content, maxApplicationContentRunes, "Document content", true)
	if err != nil {
		return TempDocumentView{}, err
	}
	summary, err := boundedRedactedInput(command.Summary, maxApplicationSummaryRunes, "Document summary", false)
	if err != nil {
		return TempDocumentView{}, err
	}
	if command.ConversationID != nil && *command.ConversationID <= 0 {
		return TempDocumentView{}, validationError("invalid_conversation_id", "Conversation ID must be positive")
	}
	if command.TaskID != nil && *command.TaskID <= 0 {
		return TempDocumentView{}, validationError("invalid_task_id", "Task ID must be positive")
	}
	if command.MissionID != nil && *command.MissionID <= 0 {
		return TempDocumentView{}, validationError("invalid_mission_id", "Mission ID must be positive")
	}
	if len(command.SessionID) > 200 {
		return TempDocumentView{}, validationError("invalid_session_id", "Session ID exceeds its bounded limit")
	}
	doc := &database.TempDocument{ID: uuid.NewString()[:8], Title: title, Content: content, Summary: summary, SessionID: strings.TrimSpace(command.SessionID)}
	if command.ConversationID != nil {
		doc.ConversationID = sql.NullInt64{Int64: *command.ConversationID, Valid: true}
	}
	if command.TaskID != nil {
		doc.TaskID = sql.NullInt64{Int64: *command.TaskID, Valid: true}
	}
	if command.MissionID != nil {
		doc.MissionID = sql.NullInt64{Int64: *command.MissionID, Valid: true}
	}
	if err = s.store.CreateTempDocument(ctx, doc); err != nil {
		return TempDocumentView{}, err
	}
	publishApplicationChange(ctx, s.effects, ApplicationChange{Domain: "documents", Action: "temp_created", Meta: map[string]any{"document_id": doc.ID}})
	return tempDocumentView(doc, false), nil
}

func (s *DocumentService) GetTemp(ctx context.Context, id string) (TempDocumentView, error) {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 100 {
		return TempDocumentView{}, validationError("invalid_document_id", "Document ID is invalid")
	}
	doc, err := s.store.GetTempDocument(ctx, id)
	if err != nil || doc == nil {
		return TempDocumentView{}, notFoundError("document_not_found", "Document not found", err)
	}
	return tempDocumentView(doc, true), nil
}

func memoryDocumentView(doc *database.MemoryDoc) MemoryDocumentView {
	content, truncated := boundedRedactedOutput(doc.Content, maxApplicationContentRunes)
	summary, summaryTruncated := boundedRedactedOutput(doc.Summary.String, maxApplicationSummaryRunes)
	updatedBy, updatedByTruncated := boundedRedactedOutput(doc.LastUpdatedBy, 200)
	return MemoryDocumentView{ProjectID: doc.ProjectID, Content: content, Version: doc.Version, UpdatedBy: updatedBy, Summary: summary, UpdatedAt: doc.UpdatedAt, Exists: true, WasTruncated: truncated || summaryTruncated || updatedByTruncated}
}

func tempDocumentView(doc *database.TempDocument, includeContent bool) TempDocumentView {
	title, titleTruncated := boundedRedactedOutput(doc.Title, maxApplicationTitleRunes)
	status, statusTruncated := boundedRedactedOutput(doc.Status, 100)
	sessionID, sessionTruncated := boundedRedactedOutput(doc.SessionID, 200)
	view := TempDocumentView{ID: doc.ID, Title: title, Status: status, SessionID: sessionID, CreatedAt: doc.CreatedAt, WasTruncated: titleTruncated || statusTruncated || sessionTruncated}
	if doc.ConversationID.Valid {
		id := doc.ConversationID.Int64
		view.ConversationID = &id
	}
	if doc.TaskID.Valid {
		id := doc.TaskID.Int64
		view.TaskID = &id
	}
	if includeContent {
		var contentTruncated, summaryTruncated bool
		view.Content, contentTruncated = boundedRedactedOutput(doc.Content, maxApplicationContentRunes)
		view.Summary, summaryTruncated = boundedRedactedOutput(doc.Summary, maxApplicationSummaryRunes)
		view.WasTruncated = view.WasTruncated || contentTruncated || summaryTruncated
	}
	return view
}

func (s *DocumentService) project(ctx context.Context, id int64) (*database.Project, error) {
	if err := validateBoundedID(id, "Project"); err != nil {
		return nil, err
	}
	project, err := s.store.GetProject(ctx, id)
	if err != nil || project == nil {
		return nil, notFoundError("project_not_found", "Project not found", err)
	}
	return project, nil
}
