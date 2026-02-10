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

	mu                sync.Mutex
	channels          map[string]bool
	inputHandler      func(data []byte)
	resizeHandler     func(rows, cols uint16)
	disconnectHandler func()
}

func NewClient(hub *Hub, conn *websocket.Conn) *Client {
	return &Client{
		ID:       uuid.New().String(),
		hub:      hub,
		conn:     conn,
		send:     make(chan *Message, 2048),
		channels: make(map[string]bool),
	}
}

func (c *Client) SetInputHandler(handler func(data []byte)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inputHandler = handler
}

func (c *Client) SetResizeHandler(handler func(rows, cols uint16)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resizeHandler = handler
}

func (c *Client) SetDisconnectHandler(handler func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disconnectHandler = handler
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
		c.mu.Lock()
		handler := c.disconnectHandler
		c.mu.Unlock()
		if handler != nil {
			handler()
		}
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
			c.mu.Lock()
			handler := c.resizeHandler
			c.mu.Unlock()
			if handler != nil && msg.Data != nil {
				var size struct {
					Cols uint16 `json:"cols"`
					Rows uint16 `json:"rows"`
				}
				if err := json.Unmarshal(msg.Data, &size); err == nil && size.Cols > 0 && size.Rows > 0 {
					handler(size.Rows, size.Cols)
				}
			}
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

			// Drain all pending messages from the channel to batch them
			batch := []*Message{msg}
		drain:
			for {
				select {
				case next, ok := <-c.send:
					if !ok {
						break drain
					}
					batch = append(batch, next)
				default:
					break drain
				}
			}

			// Coalesce consecutive session_output messages to reduce WebSocket frames
			coalesced := coalesceMessages(batch)

			// Write all coalesced messages
			for _, m := range coalesced {
				wctx, cancel := context.WithTimeout(ctx, writeWait)
				data, err := json.Marshal(m)
				if err != nil {
					cancel()
					log.Printf("Failed to marshal message: %v", err)
					continue
				}

				if err := c.conn.Write(wctx, websocket.MessageText, data); err != nil {
					cancel()
					log.Printf("WebSocket write error: %v", err)
					return
				}
				cancel()
			}
		}
	}
}

// coalesceMessages merges consecutive session_output messages with the same
// ChannelID into a single message by concatenating their Data strings.
// Non-session_output messages and messages with different channels act as
// boundaries, preserving message ordering.
func coalesceMessages(msgs []*Message) []*Message {
	if len(msgs) <= 1 {
		return msgs
	}

	result := make([]*Message, 0, len(msgs))
	var acc *Message // accumulator for session_output coalescing

	for _, m := range msgs {
		if m.Type == MsgTypeSessionOutput {
			if acc != nil && acc.ChannelID == m.ChannelID {
				// Same type+channel: concatenate data strings
				accStr, _ := acc.Data.(string)
				mStr, _ := m.Data.(string)
				acc.Data = accStr + mStr
			} else {
				// Different channel or first output msg: flush previous, start new
				if acc != nil {
					result = append(result, acc)
				}
				cpy := *m
				acc = &cpy
			}
		} else {
			// Non-output message: flush accumulator, pass through
			if acc != nil {
				result = append(result, acc)
				acc = nil
			}
			result = append(result, m)
		}
	}
	if acc != nil {
		result = append(result, acc)
	}
	return result
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
