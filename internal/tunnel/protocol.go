package tunnel

import (
	"encoding/json"
	"net/http"
)

// Tunnel message types
const (
	MsgRegister   = "register"   // client→relay: send auth token
	MsgRegistered = "registered" // relay→client: assigned subdomain + URL
	MsgHTTPReq    = "http_req"   // relay→client: proxied HTTP request
	MsgHTTPRes    = "http_res"   // client→relay: HTTP response
	MsgWSOpen     = "ws_open"    // relay→client: open WebSocket passthrough
	MsgWSOpened   = "ws_opened"  // client→relay: WebSocket successfully opened
	MsgWSData     = "ws_data"    // bidirectional: WebSocket frame data
	MsgWSClose    = "ws_close"   // either side: close WebSocket
	MsgPing       = "ping"       // keep-alive ping
	MsgPong       = "pong"       // keep-alive pong
	MsgError      = "error"      // error message
)

// Message is the envelope for all tunnel communication.
type Message struct {
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"` // request/ws session ID for multiplexing
	Data json.RawMessage `json:"data,omitempty"`
}

// RegisterData is sent by the client to authenticate with the relay.
type RegisterData struct {
	Token      string `json:"token"`
	DeviceName string `json:"device_name,omitempty"`
}

// RegisteredData is sent by the relay after successful registration.
type RegisteredData struct {
	Subdomain string     `json:"subdomain"`
	URL       string     `json:"url"`
	Usage     *UsageInfo `json:"usage,omitempty"`
}

// UsageInfo is returned by the relay to report per-token usage and limits.
type UsageInfo struct {
	RequestsUsed  int   `json:"requests_used"`
	RequestsLimit int   `json:"requests_limit"`
	BytesUsed     int64 `json:"bytes_used"`
	BytesLimit    int64 `json:"bytes_limit"`
	ResetsAt      int64 `json:"resets_at"` // unix timestamp
}

// HTTPRequestData represents an HTTP request forwarded through the tunnel.
type HTTPRequestData struct {
	Method  string      `json:"method"`
	Path    string      `json:"path"` // includes query string
	Headers http.Header `json:"headers"`
	Body    string      `json:"body,omitempty"` // base64-encoded for binary data
}

// HTTPResponseData represents an HTTP response sent back through the tunnel.
type HTTPResponseData struct {
	Status  int         `json:"status"`
	Headers http.Header `json:"headers"`
	Body    string      `json:"body,omitempty"` // base64-encoded for binary data
}

// WSOpenData is sent when an external WebSocket connection needs to be proxied.
type WSOpenData struct {
	Path    string      `json:"path"`
	Headers http.Header `json:"headers,omitempty"`
}

// WSDataPayload carries WebSocket frame data.
type WSDataPayload struct {
	Data   string `json:"data"`             // base64-encoded binary data
	IsText bool   `json:"is_text,omitempty"` // true if text frame, false if binary
}

// ErrorData carries error information.
type ErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Helper functions to create typed messages

func NewMessage(msgType string, id string, data interface{}) (*Message, error) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return &Message{Type: msgType, ID: id, Data: raw}, nil
}

func ParseData[T any](msg *Message) (*T, error) {
	var v T
	if err := json.Unmarshal(msg.Data, &v); err != nil {
		return nil, err
	}
	return &v, nil
}
