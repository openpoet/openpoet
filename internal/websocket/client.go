package websocket

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	maxMessageSize = 512 * 1024 // 512KB
)

type Client struct {
	ID   string
	hub  *Hub
	conn *websocket.Conn
	send chan *Message

	mu           sync.Mutex
	channels     map[string]bool
	inputHandler func(data []byte)
}

func NewClient(hub *Hub, conn *websocket.Conn) *Client {
	return &Client{
		ID:       uuid.New().String(),
		hub:      hub,
		conn:     conn,
		send:     make(chan *Message, 256),
		channels: make(map[string]bool),
	}
}

func (c *Client) SetInputHandler(handler func(data []byte)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inputHandler = handler
}

func (c *Client) Subscribe(channelID string) {
	c.mu.Lock()
	c.channels[channelID] = true
	c.mu.Unlock()
	c.hub.Subscribe(c.ID, channelID)
}

func (c *Client) Unsubscribe(channelID string) {
	c.mu.Lock()
	delete(c.channels, channelID)
	c.mu.Unlock()
	c.hub.Unsubscribe(c.ID, channelID)
}

func (c *Client) ReadPump(ctx context.Context) {
	defer func() {
		c.hub.Unregister(c)
		c.conn.Close(websocket.StatusNormalClosure, "")
	}()

	c.conn.SetReadLimit(maxMessageSize)

	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				log.Printf("WebSocket read error: %v", err)
			}
			return
		}

		// Parse message
		var msg struct {
			Type    string          `json:"type"`
			Channel string          `json:"channel,omitempty"`
			Data    json.RawMessage `json:"data,omitempty"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("Failed to parse WebSocket message: %v", err)
			continue
		}

		switch msg.Type {
		case "subscribe":
			if msg.Channel != "" {
				c.Subscribe(msg.Channel)
			}
		case "unsubscribe":
			if msg.Channel != "" {
				c.Unsubscribe(msg.Channel)
			}
		case "pong":
			// Client responded to ping
		case "input":
			// Terminal input from client
			c.mu.Lock()
			handler := c.inputHandler
			c.mu.Unlock()
			if handler != nil && msg.Data != nil {
				var input string
				if err := json.Unmarshal(msg.Data, &input); err == nil {
					handler([]byte(input))
				}
			}
		case "resize":
			// Handle terminal resize if needed
			// Could be passed to PTY
		}
	}
}

func (c *Client) WritePump(ctx context.Context) {
	defer func() {
		c.conn.Close(websocket.StatusNormalClosure, "")
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-c.send:
			if !ok {
				c.conn.Close(websocket.StatusNormalClosure, "")
				return
			}

			ctx, cancel := context.WithTimeout(ctx, writeWait)
			data, err := json.Marshal(msg)
			if err != nil {
				cancel()
				log.Printf("Failed to marshal message: %v", err)
				continue
			}

			if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
				cancel()
				log.Printf("WebSocket write error: %v", err)
				return
			}
			cancel()
		}
	}
}

func (c *Client) Send(msg *Message) {
	select {
	case c.send <- msg:
	default:
		// Buffer full
	}
}

// UpgradeAndServe upgrades an HTTP connection to WebSocket and starts the client
func UpgradeAndServe(hub *Hub, w http.ResponseWriter, r *http.Request) (*Client, error) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return nil, err
	}

	client := NewClient(hub, conn)
	hub.Register(client)

	// Use background context - not r.Context() which closes when handler returns
	ctx := context.Background()
	go client.ReadPump(ctx)
	go client.WritePump(ctx)

	return client, nil
}
