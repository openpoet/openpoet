package application

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"openpoet/internal/database"
)

// PublicBaseURLSetting is the setting that holds the address this OpenPoet
// instance is reached at from outside the machine it runs on (for example
// "https://openpoet.example.com"). Every link OpenPoet hands out for use
// outside the app is built on it.
const PublicBaseURLSetting = "public_base_url"

// PublicBaseURLEnv overrides the setting. It is for deployments configured
// through the environment.
const PublicBaseURLEnv = "OPENPOET_PUBLIC_BASE_URL"

// Link kinds a link can be requested for.
const (
	LinkKindArtifact = "artifact"
	LinkKindDocument = "document"
	LinkKindSession  = "session"
	LinkKindTask     = "task"
	LinkKindProject  = "project"
)

// publicBaseURLCacheTTL bounds how stale the cached base may be. The Host
// guard asks for it on every request, so it is not read from the database
// each time.
const publicBaseURLCacheTTL = 5 * time.Second

// LinkStore is the persistence the link service needs: the setting, and the
// lookups that tell whether the thing a link points at exists.
type LinkStore interface {
	GetSetting(context.Context, string) (string, error)
	GetArtifact(context.Context, string) (*database.Artifact, error)
	GetTempDocument(context.Context, string) (*database.TempDocument, error)
	GetSession(context.Context, string) (*database.Session, error)
	GetTask(context.Context, int64) (*database.ProjectTask, error)
	GetProject(context.Context, int64) (*database.Project, error)
}

// LinkView is a link to something in OpenPoet.
type LinkView struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	// Path is the in-app path ("/app/artifact/ab12cd34"). It always exists.
	Path string `json:"path"`
	// URL is Path on the public base URL. Empty when none is configured.
	URL string `json:"url,omitempty"`
	// LocalURL is Path on the address the request came in on. It only works on
	// the machine (or network) that address belongs to.
	LocalURL string `json:"local_url,omitempty"`
	// Public reports whether URL is set, i.e. a public base URL is configured.
	Public bool `json:"public"`
}

// LinkQuery asks for the link of one thing. Version only applies to artifacts.
type LinkQuery struct {
	Kind    string
	ID      string
	Version int
	// LocalBase is the scheme and host the request came in on, used for
	// LocalURL ("http://localhost:8080"). Optional.
	LocalBase string
}

// LinkService builds the links OpenPoet hands out. It is the only place that
// knows the path of each kind of thing and the public address of the instance.
type LinkService struct {
	store LinkStore

	mu       sync.Mutex
	cached   string
	cachedAt time.Time
}

func NewLinkService(store LinkStore) *LinkService {
	return &LinkService{store: store}
}

// NormalizePublicBaseURL validates a public base URL and returns it without a
// trailing slash. An empty value is valid and means "not configured".
func NormalizePublicBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > 2048 {
		return "", validationError("invalid_public_base_url", "Public base URL is too long")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", validationError("invalid_public_base_url", "Public base URL must be an http or https address, for example https://openpoet.example.com")
	}
	if parsed.User != nil {
		return "", validationError("invalid_public_base_url", "Public base URL cannot embed credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", validationError("invalid_public_base_url", "Public base URL cannot carry a query string or a fragment")
	}
	if strings.ContainsAny(parsed.Host, " \t\r\n\"'<>\\") || parsed.Hostname() == "" {
		return "", validationError("invalid_public_base_url", "Public base URL has an invalid host")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed.Scheme + "://" + parsed.Host + parsed.Path, nil
}

// PublicBaseURL returns the configured public address, or "" when there is
// none. The environment variable wins over the setting. An invalid stored
// value counts as not configured.
func (s *LinkService) PublicBaseURL(ctx context.Context) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cachedAt.IsZero() && time.Since(s.cachedAt) < publicBaseURLCacheTTL {
		return s.cached
	}
	raw := os.Getenv(PublicBaseURLEnv)
	if strings.TrimSpace(raw) == "" && s.store != nil {
		raw, _ = s.store.GetSetting(ctx, PublicBaseURLSetting)
	}
	base, err := NormalizePublicBaseURL(raw)
	if err != nil {
		base = ""
	}
	s.cached, s.cachedAt = base, time.Now()
	return base
}

// Invalidate drops the cached public base URL, so a change to the setting is
// seen at once.
func (s *LinkService) Invalidate() {
	s.mu.Lock()
	s.cachedAt = time.Time{}
	s.mu.Unlock()
}

// PublicHost returns the host name of the public base URL (lower case, without
// the port), or "" when none is configured.
func (s *LinkService) PublicHost(ctx context.Context) string {
	base := s.PublicBaseURL(ctx)
	if base == "" {
		return ""
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// Absolute returns path on the public base URL, or "" when none is configured.
func (s *LinkService) Absolute(ctx context.Context, path string) string {
	base := s.PublicBaseURL(ctx)
	if base == "" || !strings.HasPrefix(path, "/") {
		return ""
	}
	return base + path
}

// ArtifactLinkPath is the in-app path that opens an artifact in its viewer.
// version 0 means the current version.
func ArtifactLinkPath(id string, version int) string {
	path := "/app/artifact/" + url.PathEscape(id)
	if version > 0 {
		path += "?v=" + strconv.Itoa(version)
	}
	return path
}

// DocumentLinkPath is the in-app path that opens a Doc in the document viewer.
func DocumentLinkPath(id string) string {
	return "/app/doc/" + url.PathEscape(id)
}

// SessionLinkPath is the in-app path that opens a session's terminal.
func SessionLinkPath(id string) string {
	return "/app/session/" + url.PathEscape(id)
}

// TaskLinkPath is the in-app path that opens a task.
func TaskLinkPath(projectID, taskID int64) string {
	return fmt.Sprintf("/app/task/%d/%d", projectID, taskID)
}

// ProjectLinkPath is the in-app path that opens a project.
func ProjectLinkPath(projectID int64) string {
	return fmt.Sprintf("/app/project/%d", projectID)
}

// Link resolves the link of one existing artifact, Doc, session, task or
// project. Asking for something that does not exist is an error: a link that
// opens nothing is worse than no link.
func (s *LinkService) Link(ctx context.Context, query LinkQuery) (LinkView, error) {
	kind := strings.ToLower(strings.TrimSpace(query.Kind))
	id := strings.TrimSpace(query.ID)
	if id == "" || len(id) > 200 {
		return LinkView{}, validationError("invalid_link_id", "id is required")
	}
	view := LinkView{Kind: kind}
	switch kind {
	case LinkKindArtifact:
		id = strings.TrimPrefix(id, "artifact:")
		artifact, err := s.store.GetArtifact(ctx, id)
		if err != nil || artifact == nil {
			return LinkView{}, notFoundError("artifact_not_found", "Artifact not found", err)
		}
		if query.Version < 0 || query.Version > artifact.CurrentVersion {
			return LinkView{}, notFoundError("artifact_version_not_found", "Artifact version not found", nil)
		}
		view.ID, view.Title, view.Path = artifact.ID, artifact.Title, ArtifactLinkPath(artifact.ID, query.Version)
	case LinkKindDocument, "doc":
		view.Kind = LinkKindDocument
		// The document listings name artifacts "artifact:<id>": hand those over.
		if strings.HasPrefix(id, "artifact:") {
			return s.Link(ctx, LinkQuery{Kind: LinkKindArtifact, ID: id, Version: query.Version, LocalBase: query.LocalBase})
		}
		document, err := s.store.GetTempDocument(ctx, id)
		if err != nil || document == nil {
			return LinkView{}, notFoundError("document_not_found", "Document not found", err)
		}
		view.ID, view.Title, view.Path = document.ID, document.Title, DocumentLinkPath(document.ID)
	case LinkKindSession:
		session, err := s.store.GetSession(ctx, id)
		if err != nil || session == nil || session.ID == "" {
			return LinkView{}, notFoundError("session_not_found", "Session not found", err)
		}
		view.ID, view.Title, view.Path = session.ID, session.Name, SessionLinkPath(session.ID)
	case LinkKindTask:
		taskID, err := strconv.ParseInt(strings.TrimPrefix(id, "#"), 10, 64)
		if err != nil || taskID <= 0 {
			return LinkView{}, validationError("invalid_link_id", "A task link needs the numeric task id")
		}
		task, err := s.store.GetTask(ctx, taskID)
		if err != nil || task == nil {
			return LinkView{}, notFoundError("task_not_found", "Task not found", err)
		}
		view.ID, view.Title, view.Path = strconv.FormatInt(task.ID, 10), task.Title, TaskLinkPath(task.ProjectID, task.ID)
	case LinkKindProject:
		projectID, err := strconv.ParseInt(id, 10, 64)
		if err != nil || projectID <= 0 {
			return LinkView{}, validationError("invalid_link_id", "A project link needs the numeric project id")
		}
		project, err := s.store.GetProject(ctx, projectID)
		if err != nil || project == nil {
			return LinkView{}, notFoundError("project_not_found", "Project not found", err)
		}
		view.ID, view.Title, view.Path = strconv.FormatInt(project.ID, 10), project.Name, ProjectLinkPath(project.ID)
	default:
		return LinkView{}, validationError("invalid_link_kind", "kind must be artifact, document, session, task or project")
	}
	view.Title = singleLine(view.Title)
	view.URL = s.Absolute(ctx, view.Path)
	view.Public = view.URL != ""
	if local := strings.TrimRight(strings.TrimSpace(query.LocalBase), "/"); local != "" {
		view.LocalURL = local + view.Path
	}
	return view, nil
}
