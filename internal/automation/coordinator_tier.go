package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/sessiontoken"
)

// Phase 7.1 (Maestro): the coordinator tier. The session that starts a mission
// elects itself coordinator of a coordination group (a tag with coordination=1)
// by CAS-acquiring a lease on the group blackboard. While it holds the lease it
// acts through a scoped SESSION actor — {Type:"session", ProjectFilter:
// {"tag_ids":[group]}} — so every existing enforcement surface (central
// project-scope check, list/await/stream filtering) applies unchanged. Fence
// tokens travel with every coordinator mutation and fail closed when the lease
// changed hands. Host-agnostic by construction: everything operates on
// session/project ids, so remote (SSH) projects participate exactly like local
// ones.

// CoordinatorLeaseKey is the blackboard key of a group's coordinator lease.
const CoordinatorLeaseKey = "coordinator-lease"

const (
	coordinatorDefaultLeaseTTL = 300 // seconds
	coordinatorMinLeaseTTL     = 2
	coordinatorMaxLeaseTTL     = 3600
)

// coordinatorSessionScopes is the pinned scope set of the coordinator-tier
// session actor. Deliberately excludes approvals:grant / approvals:self —
// destructive verbs stay human-gated (same stance as the brain's client).
var coordinatorSessionScopes = []Scope{
	ScopeProjectsRead,
	ScopeEventsRead,
	ScopeSessionsRead,
	ScopeSessionsWrite,
	ScopeTasksRead,
	ScopeTasksWrite,
	ScopeWorkspacesRead,
	ScopeWorkspacesWrite,
	ScopeGroupsRead,
	ScopeBlackboardRead,
	ScopeBlackboardWrite,
	ScopeNotificationsRead,
}

// CoordinatorStore is what the coordinator tier needs from the database.
// *database.DB satisfies it.
type CoordinatorStore interface {
	GetSessionTokenHashes(ctx context.Context, id string) (mcpTokenHash, hookTokenHash string, err error)
	GetSession(ctx context.Context, id string) (*database.Session, error)
	GetTag(ctx context.Context, id int64) (*database.Tag, error)
	ListCoordinationTags(ctx context.Context) ([]database.Tag, error)
	ProjectIDsForTags(ctx context.Context, tagIDs []int64) ([]int64, error)
	BlackboardGet(ctx context.Context, scopeType string, scopeID int64, key string) (*database.BlackboardEntry, error)
	BlackboardPut(ctx context.Context, in database.BlackboardPutInput) (int64, error)
}

type coordinatorAPI struct {
	store CoordinatorStore
	api   *commandAPI // reused for awaitEvents/waitForSession internals
}

type coordinatorSessionKey struct{}

type coordinatorSession struct {
	SessionID string
	ProjectID int64
}

// NewCoordinatorHandler mounts the token-authenticated coordinator surface.
// Every route requires a VERIFIED opst1_ session bearer — the session id
// embedded in the token, checked against its stored digest, is the only
// identity honored. New authority is never reachable from the legacy
// unauthenticated localhost surface.
func NewCoordinatorHandler(store CoordinatorStore, deps Dependencies) http.Handler {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Events == nil {
		deps.Events, _ = store.(EventStore)
	}
	if deps.Waiter == nil {
		deps.Waiter, _ = store.(OutboxWaiter)
	}
	if deps.Sessions == nil {
		deps.Sessions, _ = store.(SessionStateStore)
	}
	if deps.ProjectScope == nil {
		deps.ProjectScope, _ = store.(ProjectScopeStore)
	}
	c := &coordinatorAPI{
		store: store,
		api: &commandAPI{
			capabilities: deps.Capabilities, platform: deps.PlatformCapabilities,
			events: deps.Events, waiter: deps.Waiter, sessions: deps.Sessions,
			scopeStore: deps.ProjectScope, now: deps.Now,
		},
	}

	router := chi.NewRouter()
	router.Use(BodyLimit(DefaultBodyLimit))
	router.Use(c.sessionAuthMiddleware)
	router.Post("/elect", c.elect)
	router.Get("/status", c.status)
	router.Get("/sessions", c.listGroupSessions)
	router.Post("/sessions", c.startWorker)
	router.Post("/sessions/{id}/input", c.sendToWorker)
	router.Get("/sessions/{id}/wait", c.waitForSession)
	router.Get("/events/await", c.awaitEvents)
	return router
}

// sessionAuthMiddleware verifies the opst1_ bearer digest and loads the calling
// session. The verified session id — never a query param — is the identity.
func (c *coordinatorAPI) sessionAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "authentication_required", "a session bearer token is required", false)
			return
		}
		bearer := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		sid, ok := sessiontoken.SessionIDFromMCPToken(bearer)
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication_required", "a session bearer token is required", false)
			return
		}
		mcpHash, _, err := c.store.GetSessionTokenHashes(r.Context(), sid)
		if err != nil || !sessiontoken.EqualHash(bearer, mcpHash) {
			writeError(w, http.StatusUnauthorized, "authentication_invalid", "the session token could not be verified", false)
			return
		}
		sess, err := c.store.GetSession(r.Context(), sid)
		if err != nil || sess == nil {
			writeError(w, http.StatusUnauthorized, "authentication_invalid", "the session no longer exists", false)
			return
		}
		ctx := context.WithValue(r.Context(), coordinatorSessionKey{}, coordinatorSession{
			SessionID: sid, ProjectID: sess.ProjectID,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func coordinatorSessionFromContext(ctx context.Context) (coordinatorSession, bool) {
	cs, ok := ctx.Value(coordinatorSessionKey{}).(coordinatorSession)
	return cs, ok
}

type coordinatorLeaseValue struct {
	SessionID string `json:"session_id"`
}

type coordinatorElectRequest struct {
	Group      int64 `json:"group"`
	TTLSeconds int   `json:"ttl_seconds"`
}

type coordinatorElectResponse struct {
	Elected         bool   `json:"elected"`
	Group           int64  `json:"group"`
	HolderSessionID string `json:"holder_session_id"`
	FenceVersion    int64  `json:"fence_version,omitempty"`
	TTLSeconds      int    `json:"ttl_seconds,omitempty"`
}

func clampCoordinatorTTL(ttl int) int {
	if ttl <= 0 {
		return coordinatorDefaultLeaseTTL
	}
	if ttl < coordinatorMinLeaseTTL {
		return coordinatorMinLeaseTTL
	}
	if ttl > coordinatorMaxLeaseTTL {
		return coordinatorMaxLeaseTTL
	}
	return ttl
}

// elect is the automatic election: the calling session CAS-acquires the group's
// coordinator lease (expected_version 0 when absent/expired). The current
// holder renews by CAS on the live version; anyone else gets a typed conflict
// naming the live holder. The returned version IS the fence token.
func (c *coordinatorAPI) elect(w http.ResponseWriter, r *http.Request) {
	cs, ok := coordinatorSessionFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "session identity is missing", false)
		return
	}
	var req coordinatorElectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Group <= 0 {
		writeError(w, http.StatusBadRequest, "coordinator_group_required", "a coordination group (tag id) is required", false)
		return
	}
	tag, err := c.store.GetTag(r.Context(), req.Group)
	if err != nil || tag == nil || tag.Coordination != 1 {
		writeError(w, http.StatusBadRequest, "coordinator_group_invalid", "the group is not a coordination tag (coordination=1)", false)
		return
	}
	memberIDs, err := c.store.ProjectIDsForTags(r.Context(), []int64{req.Group})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "coordinator_membership_failed", "group membership could not be resolved", true)
		return
	}
	member := false
	for _, id := range memberIDs {
		if id == cs.ProjectID {
			member = true
			break
		}
	}
	if !member {
		writeError(w, http.StatusForbidden, "coordinator_not_member", "the session's project is not a member of this coordination group", false)
		return
	}

	ttl := clampCoordinatorTTL(req.TTLSeconds)
	valueJSON, _ := json.Marshal(coordinatorLeaseValue{SessionID: cs.SessionID})
	lease, err := c.store.BlackboardGet(r.Context(), "group", req.Group, CoordinatorLeaseKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "coordinator_lease_failed", "the coordinator lease could not be read", true)
		return
	}
	expected := int64(0)
	if lease != nil {
		var held coordinatorLeaseValue
		_ = json.Unmarshal([]byte(lease.ValueJSON), &held)
		if held.SessionID != cs.SessionID {
			// Live lease held by someone else: the election is lost, not stolen.
			writeJSON(w, http.StatusConflict, coordinatorElectResponse{
				Elected: false, Group: req.Group, HolderSessionID: held.SessionID,
			})
			return
		}
		expected = lease.Version // renewal by the current holder
	}
	version, err := c.store.BlackboardPut(r.Context(), database.BlackboardPutInput{
		ScopeType: "group", ScopeID: req.Group, Key: CoordinatorLeaseKey,
		ValueJSON: string(valueJSON), ExpectedVersion: &expected, TTLSeconds: ttl,
		Actor: "session:" + cs.SessionID,
	})
	if err != nil {
		if errors.Is(err, database.ErrBlackboardCASConflict) {
			holder := ""
			if cur, getErr := c.store.BlackboardGet(r.Context(), "group", req.Group, CoordinatorLeaseKey); getErr == nil && cur != nil {
				var held coordinatorLeaseValue
				_ = json.Unmarshal([]byte(cur.ValueJSON), &held)
				holder = held.SessionID
			}
			writeJSON(w, http.StatusConflict, coordinatorElectResponse{
				Elected: false, Group: req.Group, HolderSessionID: holder,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "coordinator_lease_failed", "the coordinator lease could not be written", true)
		return
	}
	writeJSON(w, http.StatusOK, coordinatorElectResponse{
		Elected: true, Group: req.Group, HolderSessionID: cs.SessionID,
		FenceVersion: version, TTLSeconds: ttl,
	})
}

// heldLease finds the coordination group whose lease this session currently
// holds. Coordination tags are few, so a scan is cheap.
func (c *coordinatorAPI) heldLease(ctx context.Context, sessionID string) (int64, *database.BlackboardEntry) {
	tags, err := c.store.ListCoordinationTags(ctx)
	if err != nil {
		return 0, nil
	}
	for _, tag := range tags {
		lease, err := c.store.BlackboardGet(ctx, "group", tag.ID, CoordinatorLeaseKey)
		if err != nil || lease == nil {
			continue
		}
		var held coordinatorLeaseValue
		if json.Unmarshal([]byte(lease.ValueJSON), &held) == nil && held.SessionID == sessionID {
			return tag.ID, lease
		}
	}
	return 0, nil
}

// requireCoordinator resolves the caller's held lease, optionally validating a
// fence token (mutations). A caller presenting a fence for a lease it no longer
// holds — or a fence whose version is not the live one — fails CLOSED with
// coordinator_fence_stale before any side effect.
func (c *coordinatorAPI) requireCoordinator(w http.ResponseWriter, r *http.Request, fence *int64) (coordinatorSession, int64, bool) {
	cs, ok := coordinatorSessionFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "session identity is missing", false)
		return coordinatorSession{}, 0, false
	}
	group, lease := c.heldLease(r.Context(), cs.SessionID)
	if lease == nil {
		if fence != nil {
			writeError(w, http.StatusConflict, "coordinator_fence_stale", "the coordinator lease is stale — it expired or another session took over", false)
			return coordinatorSession{}, 0, false
		}
		writeError(w, http.StatusForbidden, "coordinator_not_coordinator", "this session holds no coordinator lease (call openpoet_coordinator_elect first)", false)
		return coordinatorSession{}, 0, false
	}
	if fence != nil && *fence != lease.Version {
		writeError(w, http.StatusConflict, "coordinator_fence_stale", "the fence token is stale — re-elect and retry with the current fence_version", false)
		return coordinatorSession{}, 0, false
	}
	return cs, group, true
}

// sessionActor builds the scoped session actor: the entire point of the tier.
// ProjectFilter names the coordination group, so resolveActorProjectScope and
// every downstream enforcement treat the coordinator exactly like a scoped
// automation client — but attributed (and lineage-stamped) as the session.
func coordinatorSessionActor(sessionID string, group int64) Actor {
	scopes, _ := NewScopeSet(coordinatorSessionScopes...)
	return Actor{
		Type: "session", ID: sessionID, Name: "coordinator-session",
		Scopes:        scopes,
		ClientID:      "", // ActionAuthorization actor id falls back to the session id
		ProjectFilter: fmt.Sprintf(`{"tag_ids":[%d]}`, group),
	}
}

func (c *coordinatorAPI) status(w http.ResponseWriter, r *http.Request) {
	cs, ok := coordinatorSessionFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "session identity is missing", false)
		return
	}
	group, lease := c.heldLease(r.Context(), cs.SessionID)
	if lease == nil {
		writeJSON(w, http.StatusOK, map[string]any{"coordinator": false, "session_id": cs.SessionID})
		return
	}
	members, _ := c.store.ProjectIDsForTags(r.Context(), []int64{group})
	writeJSON(w, http.StatusOK, map[string]any{
		"coordinator": true, "session_id": cs.SessionID, "group": group,
		"fence_version": lease.Version, "member_project_ids": members,
	})
}

// dispatch runs a platform capability as the coordinator's session actor,
// self-approving write-tier capabilities exactly like the HTTP command path and
// the brain do. Destructive capabilities are not offered by this tier at all.
func (c *coordinatorAPI) dispatch(ctx context.Context, cs coordinatorSession, group int64, capability string, target, payload any, dryRun bool) (PlatformDispatchResult, error) {
	actor := coordinatorSessionActor(cs.SessionID, group)
	approval, err := NewValidatedPlatformApproval("coordinator-session:" + cs.SessionID)
	if err != nil {
		return PlatformDispatchResult{}, err
	}
	targetJSON, _ := json.Marshal(target)
	payloadJSON, _ := json.Marshal(payload)
	return DispatchPlatformCapability(ctx, c.api.platform, PlatformDispatchRequest{
		Capability: application.CapabilityName(capability),
		Target:     targetJSON, Payload: payloadJSON,
		Actor: actor, Reason: "coordinator session " + cs.SessionID + " (group lease fence-checked)",
		Approval: approval, DryRun: dryRun,
	})
}

func writeCoordinatorDispatchError(w http.ResponseWriter, err error) {
	var platformErr *PlatformDispatchError
	if errors.As(err, &platformErr) {
		status := http.StatusBadRequest
		switch platformErr.Code {
		case "platform_project_out_of_scope", "platform_insufficient_scope":
			status = http.StatusForbidden
		case "platform_approval_required":
			status = http.StatusConflict
		}
		writeError(w, status, platformErr.Code, platformErr.Message, platformErr.Retryable)
		return
	}
	writeError(w, http.StatusInternalServerError, "coordinator_dispatch_failed", "the coordinator action could not be completed", true)
}

func (c *coordinatorAPI) listGroupSessions(w http.ResponseWriter, r *http.Request) {
	cs, group, ok := c.requireCoordinator(w, r, nil)
	if !ok {
		return
	}
	payload := map[string]any{}
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" {
		payload["status"] = status
	}
	result, err := c.dispatch(r.Context(), cs, group, "sessions.list", map[string]any{"type": "global"}, payload, false)
	if err != nil {
		writeCoordinatorDispatchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result.Result)
}

type coordinatorSpawnRequest struct {
	ProjectID    int64  `json:"project_id"`
	FenceVersion *int64 `json:"fence_version"`
	TaskID       *int64 `json:"task_id"`
	Backend      string `json:"backend"`
	WorkspaceID  string `json:"workspace_id"`
	Isolation    string `json:"isolation"`
	CustomPrompt string `json:"custom_prompt"`
	DryRun       bool   `json:"dry_run"`
}

// startWorker spawns a real worker session in any member project of the group —
// local or remote (SSH) alike; the target is a project id and sessions.create
// already knows how to start either. Lineage (parent_session_id = the verified
// coordinator session) is stamped by the application layer because the actor is
// a session actor.
func (c *coordinatorAPI) startWorker(w http.ResponseWriter, r *http.Request) {
	var req coordinatorSpawnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ProjectID <= 0 {
		writeError(w, http.StatusBadRequest, "coordinator_project_required", "a target project_id is required", false)
		return
	}
	if req.FenceVersion == nil {
		writeError(w, http.StatusBadRequest, "coordinator_fence_required", "a fence_version is required for coordinator mutations", false)
		return
	}
	cs, group, ok := c.requireCoordinator(w, r, req.FenceVersion)
	if !ok {
		return
	}
	payload := map[string]any{}
	if req.TaskID != nil {
		payload["task_id"] = *req.TaskID
	}
	if req.Backend != "" {
		payload["backend"] = req.Backend
	}
	if req.WorkspaceID != "" {
		payload["workspace_id"] = req.WorkspaceID
	}
	if req.Isolation != "" {
		payload["isolation"] = req.Isolation
	}
	if req.CustomPrompt != "" {
		payload["custom_prompt"] = req.CustomPrompt
	}
	result, err := c.dispatch(r.Context(), cs, group, "sessions.create",
		map[string]any{"type": "project", "project_id": req.ProjectID}, payload, req.DryRun)
	if err != nil {
		writeCoordinatorDispatchError(w, err)
		return
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, map[string]any{"dry_run": true, "project_id": req.ProjectID, "preview": result.Result})
		return
	}
	sessionID := ""
	if encoded, marshalErr := json.Marshal(result.Result); marshalErr == nil {
		var view struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(encoded, &view) == nil {
			sessionID = view.ID
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"session_id": sessionID, "project_id": req.ProjectID, "status": "created", "result": result.Result,
	})
}

type coordinatorSendRequest struct {
	Text         string `json:"text"`
	FenceVersion *int64 `json:"fence_version"`
	Force        bool   `json:"force"`
}

// sendToWorker steers a worker session (send_input with ack). The central
// dispatch cannot derive a project from a session target, so the tier resolves
// the target session's project itself and fails closed when it is outside the
// coordinated group.
func (c *coordinatorAPI) sendToWorker(w http.ResponseWriter, r *http.Request) {
	targetID := strings.TrimSpace(chi.URLParam(r, "id"))
	var req coordinatorSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || targetID == "" || strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "coordinator_input_required", "a target session id and text are required", false)
		return
	}
	if req.FenceVersion == nil {
		writeError(w, http.StatusBadRequest, "coordinator_fence_required", "a fence_version is required for coordinator mutations", false)
		return
	}
	cs, group, ok := c.requireCoordinator(w, r, req.FenceVersion)
	if !ok {
		return
	}
	if !c.sessionInScope(r.Context(), cs, group, targetID, w) {
		return
	}
	result, err := c.dispatch(r.Context(), cs, group, "sessions.send_input",
		map[string]any{"type": "session", "id": targetID},
		map[string]any{"text": req.Text, "await_ack": true, "force": req.Force}, false)
	if err != nil {
		writeCoordinatorDispatchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result.Result)
}

// sessionInScope enforces group scope for session-target operations (the
// central dispatch cannot: a session target carries no project id).
func (c *coordinatorAPI) sessionInScope(ctx context.Context, cs coordinatorSession, group int64, targetID string, w http.ResponseWriter) bool {
	sess, err := c.store.GetSession(ctx, targetID)
	if err != nil || sess == nil {
		writeError(w, http.StatusNotFound, "session_not_found", "the target session does not exist", false)
		return false
	}
	actor := coordinatorSessionActor(cs.SessionID, group)
	scope := resolveActorProjectScope(ctx, c.api.scopeStore, actor)
	if scope.Restricted() && !scope.Allowed[sess.ProjectID] {
		writeError(w, http.StatusForbidden, "platform_project_out_of_scope", "the target session belongs to a project outside the coordinated group", false)
		return false
	}
	return true
}

// waitForSession delegates to the existing long-poll (signal_quality semantics
// intact) after scope-checking the target and injecting the session actor.
func (c *coordinatorAPI) waitForSession(w http.ResponseWriter, r *http.Request) {
	cs, group, ok := c.requireCoordinator(w, r, nil)
	if !ok {
		return
	}
	targetID := strings.TrimSpace(chi.URLParam(r, "id"))
	if targetID == "" {
		writeError(w, http.StatusBadRequest, "session_id_required", "session id is required", false)
		return
	}
	if !c.sessionInScope(r.Context(), cs, group, targetID, w) {
		return
	}
	actor := coordinatorSessionActor(cs.SessionID, group)
	actor.ClientID = "coordinator-session:" + cs.SessionID
	c.api.waitForSession(w, r.WithContext(WithActor(r.Context(), actor)))
}

// awaitEvents delegates to the existing long-poll. The session actor's
// ProjectFilter makes filterEventsByScope drop out-of-group events, and the
// synthetic ClientID gives the coordinator its own durable consumer cursors.
func (c *coordinatorAPI) awaitEvents(w http.ResponseWriter, r *http.Request) {
	cs, group, ok := c.requireCoordinator(w, r, nil)
	if !ok {
		return
	}
	actor := coordinatorSessionActor(cs.SessionID, group)
	actor.ClientID = "coordinator-session:" + cs.SessionID
	c.api.awaitEvents(w, r.WithContext(WithActor(r.Context(), actor)))
}
