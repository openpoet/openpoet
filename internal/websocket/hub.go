package websocket

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"
)

type MessageType string

const (
	MsgTypeSessionOutput      MessageType = "session_output"
	MsgTypeSessionStatus      MessageType = "session_status"
	MsgTypeNotification       MessageType = "notification"
	MsgTypeStateUpdate        MessageType = "state_update"
	MsgTypeError              MessageType = "error"
	MsgTypePing               MessageType = "ping"
	MsgTypePong               MessageType = "pong"
	MsgTypeCodexCommandResult MessageType = "codex_command_result"
	MsgTypeCodexState         MessageType = "codex_state"
	MsgTypeCodexTranscript    MessageType = "codex_transcript"

	// Sync progress
	MsgTypeSyncProgress MessageType = "sync_progress"

	// Hook message types
	MsgTypeHookPermission         MessageType = "hook_permission_request"
	MsgTypeHookPermissionResolved MessageType = "hook_permission_resolved"
	MsgTypeHookToolStart          MessageType = "hook_tool_start"
	MsgTypeHookToolDone           MessageType = "hook_tool_done"
	MsgTypeHookToolFailed         MessageType = "hook_tool_failed"
	MsgTypeHookNotify             MessageType = "hook_notification"
	MsgTypeHookStop               MessageType = "hook_stop"
	MsgTypeHookAskUser            MessageType = "hook_ask_user"
	MsgTypeHookExitPlan           MessageType = "hook_exit_plan"
	MsgTypeHookTaskLoaded         MessageType = "hook_task_loaded"
	MsgTypeSessionPlanUpdated     MessageType = "session_plan_updated"

	// AI proactive suggestions
	MsgTypeAISuggestion MessageType = "ai_suggestion"
	MsgTypeAIProactive  MessageType = "ai_proactive"
	MsgTypeChatDocCard  MessageType = "chat_doc_card"

	// Notification count updates
	MsgTypeNotificationCount MessageType = "notification_count"

	// Tunnel status
	MsgTypeTunnelStatus MessageType = "tunnel_status"

	// Structured view (JSONL event stream)
	MsgTypeSessionEvent MessageType = "session_event"
)

type Message struct {
	Type      MessageType `json:"type"`
	ChannelID string      `json:"channel_id,omitempty"`
	Data      interface{} `json:"data,omitempty"`
	Timestamp time.Time   `json:"timestamp"`
}

type Hub struct {
	mu sync.RWMutex

	// clients is a map of client ID to client
	clients map[string]*Client

	// channels is a map of channel ID to set of client IDs
	channels map[string]map[string]bool

	// broadcast channel for sending messages to all clients
	broadcast chan *Message

	// register channel for new clients
	register chan *Client

	// unregister channel for disconnecting clients
	unregister chan *Client

	ctx    context.Context
	cancel context.CancelFunc
}

func NewHub() *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{
		clients:    make(map[string]*Client),
		channels:   make(map[string]map[string]bool),
		broadcast:  make(chan *Message, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		ctx:        ctx,
		cancel:     cancel,
	}
	return h
}

func (h *Hub) Run() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-h.ctx.Done():
			return

		case client := <-h.register:
			h.mu.Lock()
			h.clients[client.ID] = client
			h.mu.Unlock()
			log.Printf("Client registered: %s", client.ID)

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client.ID]; ok {
				delete(h.clients, client.ID)
				// Remove client from all channels
				for channelID := range h.channels {
					delete(h.channels[channelID], client.ID)
					if len(h.channels[channelID]) == 0 {
						delete(h.channels, channelID)
					}
				}
				close(client.send)
			}
			h.mu.Unlock()
			log.Printf("Client unregistered: %s", client.ID)

		case msg := <-h.broadcast:
			h.mu.RLock()
			if msg.ChannelID != "" {
				// Send to clients in specific channel
				if clients, ok := h.channels[msg.ChannelID]; ok {
					for clientID := range clients {
						if client, ok := h.clients[clientID]; ok {
							select {
							case client.send <- msg:
							default:
								log.Printf("Dropping message for client %s: send buffer full (type=%s)", clientID[:8], msg.Type)
							}
						}
					}
				}
			} else {
				// Broadcast to all clients
				for _, client := range h.clients {
					select {
					case client.send <- msg:
					default:
						log.Printf("Dropping message for client %s: send buffer full (type=%s)", client.ID[:8], msg.Type)
					}
				}
			}
			h.mu.RUnlock()

		case <-ticker.C:
			// Send ping to all clients
			h.mu.RLock()
			for _, client := range h.clients {
				select {
				case client.send <- &Message{Type: MsgTypePing, Timestamp: time.Now()}:
				default:
				}
			}
			h.mu.RUnlock()
		}
	}
}

func (h *Hub) Stop() {
	h.cancel()
}

func (h *Hub) Register(client *Client) {
	h.register <- client
}

func (h *Hub) Unregister(client *Client) {
	h.unregister <- client
}

func (h *Hub) Subscribe(clientID, channelID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.channels[channelID]; !ok {
		h.channels[channelID] = make(map[string]bool)
	}
	h.channels[channelID][clientID] = true
	log.Printf("Client %s subscribed to channel %s", clientID, channelID)
}

func (h *Hub) Unsubscribe(clientID, channelID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if clients, ok := h.channels[channelID]; ok {
		delete(clients, clientID)
		if len(clients) == 0 {
			delete(h.channels, channelID)
		}
	}
	log.Printf("Client %s unsubscribed from channel %s", clientID, channelID)
}

func (h *Hub) Broadcast(msg *Message) {
	msg.Timestamp = time.Now()
	h.broadcast <- msg
}

func (h *Hub) BroadcastToChannel(channelID string, msg *Message) {
	msg.ChannelID = channelID
	msg.Timestamp = time.Now()
	h.broadcast <- msg
}

func (h *Hub) SendToClient(clientID string, msg *Message) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	msg.Timestamp = time.Now()
	if client, ok := h.clients[clientID]; ok {
		select {
		case client.send <- msg:
		default:
		}
	}
}

func (h *Hub) BroadcastSessionOutput(sessionID string, data []byte) {
	h.BroadcastToChannel("session:"+sessionID, &Message{
		Type: MsgTypeSessionOutput,
		Data: string(data),
	})
}

func (h *Hub) BroadcastSessionStatus(sessionID, status string) {
	h.BroadcastToChannel("session:"+sessionID, &Message{
		Type: MsgTypeSessionStatus,
		Data: map[string]string{"session_id": sessionID, "status": status},
	})
	// Also broadcast to global events channel
	h.BroadcastToChannel("events", &Message{
		Type: MsgTypeStateUpdate,
		Data: map[string]interface{}{
			"entity": "session",
			"id":     sessionID,
			"status": status,
		},
	})
}

func (h *Hub) BroadcastNotification(notification interface{}) {
	h.BroadcastToChannel("events", &Message{
		Type: MsgTypeNotification,
		Data: notification,
	})
}

func (h *Hub) BroadcastNotificationCount(count int) {
	h.BroadcastToChannel("events", &Message{
		Type: MsgTypeNotificationCount,
		Data: map[string]interface{}{
			"unread_count": count,
		},
	})
}

func (h *Hub) BroadcastStateUpdate(entity string, data interface{}) {
	h.BroadcastToChannel("events", &Message{
		Type: MsgTypeStateUpdate,
		Data: map[string]interface{}{
			"entity": entity,
			"data":   data,
		},
	})
}

func (h *Hub) BroadcastAISuggestion(suggestion interface{}) {
	h.BroadcastToChannel("events", &Message{
		Type: MsgTypeAISuggestion,
		Data: suggestion,
	})
}

func (h *Hub) BroadcastAIProactive(data interface{}) {
	h.BroadcastToChannel("events", &Message{
		Type: MsgTypeAIProactive,
		Data: data,
	})
}

func (h *Hub) BroadcastChatDocCard(data interface{}) {
	h.BroadcastToChannel("events", &Message{
		Type: MsgTypeChatDocCard,
		Data: data,
	})
}

func (h *Hub) GetClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *Hub) GetChannelClientCount(channelID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if clients, ok := h.channels[channelID]; ok {
		return len(clients)
	}
	return 0
}

// MarshalMessage marshals a message to JSON bytes
func MarshalMessage(msg *Message) ([]byte, error) {
	return json.Marshal(msg)
}

// UnmarshalMessage unmarshals JSON bytes to a Message
func UnmarshalMessage(data []byte) (*Message, error) {
	var msg Message
	err := json.Unmarshal(data, &msg)
	return &msg, err
}

// BroadcastSyncProgress sends a sync progress update to all event subscribers.
func (h *Hub) BroadcastSyncProgress(projectID int64, step, status, detail string) {
	h.BroadcastToChannel("events", &Message{
		Type: MsgTypeSyncProgress,
		Data: map[string]interface{}{
			"project_id": projectID,
			"step":       step,
			"status":     status,
			"detail":     detail,
		},
	})
}

// BroadcastHookEvent sends a hook event to clients subscribed to the hooks channel for a session.
// Also sends to the global "events" channel so that app.js receives hooks even without
// the specific session WebSocket open (e.g. screen off, tab not selected).
func (h *Hub) BroadcastHookEvent(sessionID string, msg *Message) {
	h.BroadcastToChannel("hooks:"+sessionID, msg)
	// Also broadcast to global events channel for reconnection recovery
	eventMsg := &Message{Type: msg.Type, Data: msg.Data}
	h.BroadcastToChannel("events", eventMsg)
}
