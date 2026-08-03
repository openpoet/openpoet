package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"openpoet/internal/database"
	"openpoet/internal/session"
	"openpoet/internal/sessiontoken"
	"openpoet/internal/tunnel"
)

// Test-only HTTP handlers. Registered ONLY under OPENPOET_TEST_MODE=1 so the
// E2E harness can mint synthetic sessions and tunnel principals without a PTY,
// an LLM, or the pairing UI. They must never exist in a normal server.

// testCreateSession mints a synthetic session row (no PTY, no runtime) with
// valid per-session credentials and returns the plaintext tokens once. It lets
// the phase checks exercise hook-token and MCP-actor enforcement at $0.
func testCreateSession(db *database.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ProjectID       int64  `json:"project_id"`
			TaskID          *int64 `json:"task_id"`
			SkipPermissions bool   `json:"skip_permissions"`
			// WorkspaceID/WorkDir mint a synthetic LANE session, so the harness can
			// exercise the tree-scoped conflict rules (same-tree collision vs
			// cross-tree divergence) without provisioning a runner per tree.
			WorkspaceID string `json:"workspace_id"`
			WorkDir     string `json:"work_dir"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ProjectID <= 0 {
			http.Error(w, `{"error":"project_id is required"}`, http.StatusBadRequest)
			return
		}
		ctx := r.Context()
		sessionID := uuid.NewString()
		session := &database.Session{
			ID:              sessionID,
			ProjectID:       body.ProjectID,
			Status:          "running",
			StartTime:       time.Now(),
			Backend:         "claude_code",
			SkipPermissions: body.SkipPermissions,
			Model:           "unknown",
			RequestedModel:  "default",
			Effort:          "default",
		}
		if body.TaskID != nil {
			session.TaskID.Int64 = *body.TaskID
			session.TaskID.Valid = true
		}
		if body.WorkDir != "" {
			session.WorkDir = body.WorkDir
		}
		if body.WorkspaceID != "" {
			session.WorkspaceID.String = body.WorkspaceID
			session.WorkspaceID.Valid = true
		}
		if err := db.CreateSession(ctx, session); err != nil {
			http.Error(w, `{"error":"create session failed"}`, http.StatusInternalServerError)
			return
		}
		mcpToken, mcpHash, err1 := sessiontoken.NewMCPToken(sessionID)
		hookToken, hookHash, err2 := sessiontoken.NewHookToken()
		if err1 != nil || err2 != nil {
			http.Error(w, `{"error":"mint credentials failed"}`, http.StatusInternalServerError)
			return
		}
		if err := db.UpdateSessionTokenHashes(ctx, sessionID, mcpHash, hookHash); err != nil {
			http.Error(w, `{"error":"persist credentials failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"session_id":    sessionID,
			"hook_token":    hookToken,
			"session_token": mcpToken,
		})
	}
}

// testInjectSessionPTY feeds synthetic bytes through the manager's attention
// sentinel — the same entry point every real runner's outputHandler uses — so
// a phase check can prove PTY-question detection without a live PTY.
func testInjectSessionPTY(sessionMgr *session.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := chi.URLParam(r, "sessionID")
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || sessionID == "" || body.Text == "" {
			http.Error(w, `{"error":"session id and text are required"}`, http.StatusBadRequest)
			return
		}
		sessionMgr.ScanOutputForAttention(sessionID, []byte(body.Text))
		w.WriteHeader(http.StatusNoContent)
	}
}

// testMintTunnelJWT provisions a paired device and returns a valid device
// session JWT, so a phase check can prove the tunnel-JWT credential path of the
// REST actor resolver without the interactive pairing flow.
func testMintTunnelJWT(db *database.DB, jwtSecret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceID := uuid.NewString()
		device := &database.PairedDevice{
			ID:         deviceID,
			DeviceName: "test-device",
			UserAgent:  "phase0-check",
			CreatedAt:  time.Now(),
			LastSeenAt: time.Now(),
		}
		if err := db.CreatePairedDevice(r.Context(), device); err != nil {
			http.Error(w, `{"error":"create device failed"}`, http.StatusInternalServerError)
			return
		}
		token, err := tunnel.CreateSessionToken(deviceID, []byte(jwtSecret))
		if err != nil {
			http.Error(w, `{"error":"mint jwt failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"device_id": deviceID,
			"token":     token,
		})
	}
}
