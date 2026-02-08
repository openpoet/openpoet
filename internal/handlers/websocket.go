package handlers

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"devmanager/internal/notifications"
	"devmanager/internal/websocket"
)

type WebSocketHandler struct {
	hub          *websocket.Hub
	api          *API
	webpush      *notifications.WebPushService
}

func NewWebSocketHandler(hub *websocket.Hub, api *API, webpush *notifications.WebPushService) *WebSocketHandler {
	return &WebSocketHandler{
		hub:     hub,
		api:     api,
		webpush: webpush,
	}
}

// HandleSessionWS handles WebSocket connections for session I/O
func (h *WebSocketHandler) HandleSessionWS(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")

	client, err := websocket.UpgradeAndServe(h.hub, w, r)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	// Subscribe to session channel and hooks channel
	client.Subscribe("session:" + sessionID)
	client.Subscribe("hooks:" + sessionID)

	// Set up input handler to forward to session
	client.SetInputHandler(func(data []byte) {
		if err := h.api.sessionMgr.WriteToSession(sessionID, data); err != nil {
			log.Printf("Failed to write to session: %v", err)
		}
	})

	// Set up resize handler - tracks per-client size and uses minimum
	client.SetResizeHandler(func(rows, cols uint16) {
		if err := h.api.sessionMgr.RegisterClientSize(sessionID, client.ID, rows, cols); err != nil {
			log.Printf("Failed to register client size: %v", err)
		}
	})

	// Clean up client size tracking on disconnect
	client.SetDisconnectHandler(func() {
		h.api.sessionMgr.UnregisterClientSize(sessionID, client.ID)
	})
}

// HandleMacroWS handles WebSocket connections for macro output
func (h *WebSocketHandler) HandleMacroWS(w http.ResponseWriter, r *http.Request) {
	executionID := chi.URLParam(r, "execution_id")

	client, err := websocket.UpgradeAndServe(h.hub, w, r)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	// Subscribe to macro channel
	client.Subscribe("macro:" + executionID)
}

// HandleEventsWS handles WebSocket connections for global events
func (h *WebSocketHandler) HandleEventsWS(w http.ResponseWriter, r *http.Request) {
	client, err := websocket.UpgradeAndServe(h.hub, w, r)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	// Subscribe to global events channel
	client.Subscribe("events")
}

// HandlePushSubscribe handles push notification subscription
func (h *WebSocketHandler) HandlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if h.webpush == nil {
		respondError(w, http.StatusServiceUnavailable, "Push notifications not configured")
		return
	}

	var sub struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}

	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := h.webpush.Subscribe(r.Context(), sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "subscribed"})
}

// HandlePushUnsubscribe handles push notification unsubscription
func (h *WebSocketHandler) HandlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if h.webpush == nil {
		respondError(w, http.StatusServiceUnavailable, "Push notifications not configured")
		return
	}

	var sub struct {
		Endpoint string `json:"endpoint"`
	}

	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := h.webpush.Unsubscribe(r.Context(), sub.Endpoint); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "unsubscribed"})
}

// HandleVAPIDPublicKey returns the VAPID public key for client subscription
func (h *WebSocketHandler) HandleVAPIDPublicKey(w http.ResponseWriter, r *http.Request) {
	if h.webpush == nil {
		respondError(w, http.StatusServiceUnavailable, "Push notifications not configured")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{
		"publicKey": h.webpush.GetPublicKey(),
	})
}

// HandleNotificationPreference returns the server-side push notification opt-out preference
func (h *WebSocketHandler) HandleNotificationPreference(w http.ResponseWriter, r *http.Request) {
	val, _ := h.api.GetDB().GetSetting(r.Context(), "push_notifications_disabled")
	disabled := val == "true"
	respondJSON(w, http.StatusOK, map[string]bool{"disabled": disabled})
}

// HandleSetNotificationPreference sets the server-side push notification opt-out preference
func (h *WebSocketHandler) HandleSetNotificationPreference(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Disabled bool `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	ctx := r.Context()
	if body.Disabled {
		if err := h.api.GetDB().SetSetting(ctx, "push_notifications_disabled", "true"); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		if err := h.api.GetDB().DeleteSetting(ctx, "push_notifications_disabled"); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// HandleTestPush sends a test push notification to all subscriptions
func (h *WebSocketHandler) HandleTestPush(w http.ResponseWriter, r *http.Request) {
	if h.webpush == nil {
		respondError(w, http.StatusServiceUnavailable, "Push notifications not configured")
		return
	}

	err := h.webpush.SendToAll(r.Context(), "DevManager Test", "Push notifications are working!", map[string]string{
		"type": "test",
	})
	if err != nil {
		respondJSON(w, http.StatusOK, map[string]string{
			"status":  "partial",
			"message": "Some notifications failed: " + err.Error(),
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{
		"status":  "sent",
		"message": "Test notification sent to all subscriptions",
	})
}
