package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// TunnelHeader is set on requests forwarded through the tunnel.
const TunnelHeader = "X-Tunnel-Request"

// Client connects to a relay server and forwards requests to the local OpenPoet.
type Client struct {
	relayURL  string // wss://tunnel-connect.relay.example.com/
	authToken string
	localAddr string // localhost:8080

	conn      *websocket.Conn
	publicURL string
	subdomain string
	status    string // disconnected, connecting, connected

	OnStatus func(status, url string)

	// OnAuthFailed is called when the relay rejects the token.
	// It should clear the old token, register a new one, and return it.
	// If nil or returns error, the client stops retrying.
	OnAuthFailed func() (newToken string, err error)

	// Encryption key for app-layer encryption (raw 32-byte AES key)
	encryptionKey []byte

	// WebSocket passthrough sessions
	localWS map[string]*websocket.Conn // wsID → local WS connection
	wsMu    sync.Mutex

	httpClient *http.Client

	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
}

// NewClient creates a new tunnel client.
// The relayURL must use wss:// — plaintext ws:// is rejected.
func NewClient(relayURL, authToken, localAddr string) *Client {
	if !strings.HasPrefix(relayURL, "wss://") {
		log.Printf("[TUNNEL] WARNING: relay URL must use wss://, got: %s", relayURL)
	}
	return &Client{
		relayURL:  relayURL,
		authToken: authToken,
		localAddr: localAddr,
		status:    "disconnected",
		localWS:   make(map[string]*websocket.Conn),
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion:         tls.VersionTLS12,
					InsecureSkipVerify: false,
				},
			},
		},
	}
}

// SetEncryptionKey sets the AES-256 key for app-layer encryption.
// When set, all WebSocket frames forwarded through the tunnel are encrypted.
func (c *Client) SetEncryptionKey(key []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.encryptionKey = key
}

// Status returns the current tunnel status.
func (c *Client) Status() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// PublicURL returns the tunnel's public URL.
func (c *Client) PublicURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.publicURL
}

// Subdomain returns the tunnel's assigned subdomain.
func (c *Client) Subdomain() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subdomain
}

func (c *Client) setStatus(status, url string) {
	c.mu.Lock()
	c.status = status
	c.publicURL = url
	c.mu.Unlock()
	if c.OnStatus != nil {
		c.OnStatus(status, url)
	}
}

// Connect establishes a tunnel connection with auto-reconnect.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.mu.Unlock()

	go c.reconnectLoop()
	return nil
}

// Disconnect closes the tunnel connection.
func (c *Client) Disconnect() {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()

	c.wsMu.Lock()
	for id, conn := range c.localWS {
		conn.Close(websocket.StatusGoingAway, "tunnel closing")
		delete(c.localWS, id)
	}
	c.wsMu.Unlock()

	c.setStatus("disconnected", "")
}

func (c *Client) reconnectLoop() {
	backoff := time.Second
	maxBackoff := 60 * time.Second

	for {
		c.mu.Lock()
		ctx := c.ctx
		c.mu.Unlock()

		if ctx == nil || ctx.Err() != nil {
			return
		}

		connStart := time.Now()
		err := c.connectOnce(ctx)
		if ctx.Err() != nil {
			return // context cancelled, intentional disconnect
		}

		// Reset backoff if the connection was stable (lasted > 30s)
		if time.Since(connStart) > 30*time.Second {
			backoff = time.Second
		}

		if err != nil {
			log.Printf("[TUNNEL] Connection error: %v, reconnecting in %s", err, backoff)

			// On auth failure, try to re-register automatically
			if strings.Contains(err.Error(), "registration rejected") && c.OnAuthFailed != nil {
				log.Printf("[TUNNEL] Token rejected, attempting re-registration...")
				newToken, reregErr := c.OnAuthFailed()
				if reregErr != nil {
					log.Printf("[TUNNEL] Re-registration failed: %v", reregErr)
				} else {
					c.mu.Lock()
					c.authToken = newToken
					c.mu.Unlock()
					log.Printf("[TUNNEL] Re-registered with new token, retrying immediately")
					backoff = time.Second
					c.setStatus("disconnected", "")
					continue // retry immediately with new token
				}
			}
		} else {
			log.Printf("[TUNNEL] Disconnected, reconnecting in %s", backoff)
		}

		c.setStatus("disconnected", "")

		select {
		case <-time.After(backoff):
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		case <-ctx.Done():
			return
		}
	}
}

func (c *Client) connectOnce(ctx context.Context) error {
	c.setStatus("connecting", "")

	if !strings.HasPrefix(c.relayURL, "wss://") {
		return fmt.Errorf("relay URL must use wss:// (TLS required), got: %s", c.relayURL)
	}

	conn, _, err := websocket.Dial(ctx, c.relayURL, &websocket.DialOptions{
		HTTPClient: c.httpClient,
	})
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}
	conn.SetReadLimit(10 << 20) // 10MB

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	defer func() {
		conn.Close(websocket.StatusGoingAway, "disconnecting")
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
	}()

	// Send registration
	regMsg, _ := NewMessage(MsgRegister, "", &RegisterData{
		Token: c.authToken,
	})
	b, _ := json.Marshal(regMsg)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	// Wait for registration response
	_, respBytes, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read register response: %w", err)
	}

	var respMsg Message
	if err := json.Unmarshal(respBytes, &respMsg); err != nil {
		return fmt.Errorf("parse register response: %w", err)
	}

	if respMsg.Type == MsgError {
		errData, _ := ParseData[ErrorData](&respMsg)
		if errData != nil {
			return fmt.Errorf("registration rejected: %s", errData.Message)
		}
		return fmt.Errorf("registration rejected")
	}

	if respMsg.Type != MsgRegistered {
		return fmt.Errorf("unexpected response type: %s", respMsg.Type)
	}

	regResp, err := ParseData[RegisteredData](&respMsg)
	if err != nil {
		return fmt.Errorf("parse registered data: %w", err)
	}

	c.mu.Lock()
	c.subdomain = regResp.Subdomain
	c.mu.Unlock()

	c.setStatus("connected", regResp.URL)
	log.Printf("[TUNNEL] Connected: %s", regResp.URL)

	// Reset backoff on successful connection (handled by caller checking error)

	// Main message loop
	return c.readMessages(ctx, conn)
}

func (c *Client) readMessages(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, msgBytes, err := conn.Read(ctx)
		if err != nil {
			return err
		}

		var msg Message
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			log.Printf("[TUNNEL] Invalid message: %v", err)
			continue
		}

		switch msg.Type {
		case MsgHTTPReq:
			go c.handleHTTPRequest(ctx, conn, &msg)
		case MsgWSOpen:
			go c.handleWSOpen(ctx, conn, &msg)
		case MsgWSData:
			go c.handleWSData(&msg)
		case MsgWSClose:
			go c.handleWSClose(&msg)
		case MsgPing:
			pong, _ := NewMessage(MsgPong, "", nil)
			b, _ := json.Marshal(pong)
			conn.Write(ctx, websocket.MessageText, b)
		}
	}
}

func (c *Client) handleHTTPRequest(ctx context.Context, conn *websocket.Conn, msg *Message) {
	reqData, err := ParseData[HTTPRequestData](msg)
	if err != nil {
		c.sendHTTPError(ctx, conn, msg.ID, http.StatusBadRequest, "invalid request data")
		return
	}

	// Build local HTTP request
	localURL := fmt.Sprintf("http://%s%s", c.localAddr, reqData.Path)

	var body io.Reader
	if reqData.Body != "" {
		body = strings.NewReader(reqData.Body)
	}

	localReq, err := http.NewRequestWithContext(ctx, reqData.Method, localURL, body)
	if err != nil {
		c.sendHTTPError(ctx, conn, msg.ID, http.StatusInternalServerError, "failed to create request")
		return
	}

	// Copy headers, add tunnel marker.
	// Remove Accept-Encoding so Go's http.Client doesn't negotiate
	// compression — the response body will be sent as plain text through
	// the tunnel, so we need it uncompressed with matching Content-Length.
	for k, vals := range reqData.Headers {
		lower := strings.ToLower(k)
		if lower == "accept-encoding" {
			continue
		}
		for _, v := range vals {
			localReq.Header.Add(k, v)
		}
	}
	localReq.Header.Set(TunnelHeader, "true")

	// Execute local request
	resp, err := c.httpClient.Do(localReq)
	if err != nil {
		c.sendHTTPError(ctx, conn, msg.ID, http.StatusBadGateway, "local request failed")
		return
	}
	defer resp.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB limit
	if err != nil {
		c.sendHTTPError(ctx, conn, msg.ID, http.StatusBadGateway, "failed to read response")
		return
	}

	// Send response back through tunnel
	httpRes := &HTTPResponseData{
		Status:  resp.StatusCode,
		Headers: resp.Header,
		Body:    string(respBody),
	}

	resMsg, _ := NewMessage(MsgHTTPRes, msg.ID, httpRes)
	b, _ := json.Marshal(resMsg)
	conn.Write(ctx, websocket.MessageText, b)
}

func (c *Client) sendHTTPError(ctx context.Context, conn *websocket.Conn, reqID string, status int, message string) {
	httpRes := &HTTPResponseData{
		Status:  status,
		Headers: http.Header{"Content-Type": {"text/plain"}},
		Body:    message,
	}
	resMsg, _ := NewMessage(MsgHTTPRes, reqID, httpRes)
	b, _ := json.Marshal(resMsg)
	conn.Write(ctx, websocket.MessageText, b)
}

func (c *Client) handleWSOpen(ctx context.Context, conn *websocket.Conn, msg *Message) {
	wsData, err := ParseData[WSOpenData](msg)
	if err != nil {
		return
	}

	// Connect to local WebSocket
	wsScheme := "ws"
	localURL := fmt.Sprintf("%s://%s%s", wsScheme, c.localAddr, wsData.Path)

	// Forward original request headers (includes cookies for auth)
	dialHeaders := http.Header{
		TunnelHeader: {"true"},
	}
	for k, vals := range wsData.Headers {
		lower := strings.ToLower(k)
		// Skip WebSocket hop-by-hop headers
		if lower == "upgrade" || lower == "connection" || lower == "sec-websocket-key" ||
			lower == "sec-websocket-version" || lower == "sec-websocket-extensions" ||
			lower == "sec-websocket-protocol" || lower == "accept-encoding" {
			continue
		}
		for _, v := range vals {
			dialHeaders.Add(k, v)
		}
	}
	localConn, _, err := websocket.Dial(ctx, localURL, &websocket.DialOptions{
		HTTPHeader: dialHeaders,
	})
	if err != nil {
		log.Printf("[TUNNEL] Failed to open local WS %s: %v", wsData.Path, err)
		closeMsg, _ := NewMessage(MsgWSClose, msg.ID, nil)
		b, _ := json.Marshal(closeMsg)
		conn.Write(ctx, websocket.MessageText, b)
		return
	}
	localConn.SetReadLimit(1 << 20) // 1MB

	c.wsMu.Lock()
	c.localWS[msg.ID] = localConn
	c.wsMu.Unlock()

	// Send opened confirmation
	openedMsg, _ := NewMessage(MsgWSOpened, msg.ID, nil)
	b, _ := json.Marshal(openedMsg)
	conn.Write(ctx, websocket.MessageText, b)

	// Pipe: local WS → relay
	go func() {
		defer func() {
			c.wsMu.Lock()
			delete(c.localWS, msg.ID)
			c.wsMu.Unlock()
			localConn.Close(websocket.StatusGoingAway, "closing")

			closeMsg, _ := NewMessage(MsgWSClose, msg.ID, nil)
			b, _ := json.Marshal(closeMsg)
			conn.Write(ctx, websocket.MessageText, b)
		}()

		for {
			msgType, data, err := localConn.Read(ctx)
			if err != nil {
				return
			}

			isText := msgType == websocket.MessageText
			c.mu.Lock()
			encKey := c.encryptionKey
			c.mu.Unlock()

			var frameData string
			if encKey != nil {
				// Encrypt with AES-256-GCM — browser JS decrypts with Web Crypto API.
				// EncryptFrame returns JSON: {"_encrypted":true,"data":"...","iv":"..."}
				encrypted, encErr := EncryptFrame(data, encKey)
				if encErr == nil {
					frameData = string(encrypted)
					isText = true // encrypted JSON envelope is always text
				} else {
					frameData = base64.StdEncoding.EncodeToString(data)
				}
			} else {
				// No encryption — base64-encode for safe JSON transport
				frameData = base64.StdEncoding.EncodeToString(data)
			}

			payload := &WSDataPayload{
				Data:   frameData,
				IsText: isText,
			}
			dataMsg, _ := NewMessage(MsgWSData, msg.ID, payload)
			b, _ := json.Marshal(dataMsg)
			if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
				return
			}
		}
	}()
}

func (c *Client) handleWSData(msg *Message) {
	c.wsMu.Lock()
	localConn, ok := c.localWS[msg.ID]
	c.wsMu.Unlock()
	if !ok {
		return
	}

	wsData, err := ParseData[WSDataPayload](msg)
	if err != nil {
		return
	}

	// Decode base64-encoded data
	rawData, decErr := base64.StdEncoding.DecodeString(wsData.Data)
	if decErr != nil {
		// Fallback: treat as raw string (backwards compat)
		rawData = []byte(wsData.Data)
	}

	msgType := websocket.MessageBinary
	if wsData.IsText {
		msgType = websocket.MessageText
	}
	localConn.Write(c.ctx, msgType, rawData)
}

func (c *Client) handleWSClose(msg *Message) {
	c.wsMu.Lock()
	localConn, ok := c.localWS[msg.ID]
	if ok {
		delete(c.localWS, msg.ID)
	}
	c.wsMu.Unlock()

	if ok {
		localConn.Close(websocket.StatusGoingAway, "remote closed")
	}
}

// RegisterWithRelay calls POST on the relay's registration subdomain to get a per-user token.
// The endpoint is open (no auth required) — rate limited by IP on the relay side.
// The relayURL must use wss:// — plaintext connections are rejected.
func RegisterWithRelay(relayURL string) (string, error) {
	if !strings.HasPrefix(relayURL, "wss://") {
		return "", fmt.Errorf("relay URL must use wss:// (TLS required), got: %s", relayURL)
	}

	// Convert wss://tunnel-connect.relay.example.com/ → https://tunnel-register.relay.example.com/
	registerURL := strings.Replace(relayURL, "wss://", "https://", 1)
	registerURL = strings.Replace(registerURL, "tunnel-connect.", "tunnel-register.", 1)

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: false,
			},
		},
	}
	resp, err := client.Post(registerURL, "application/json", nil)
	if err != nil {
		return "", fmt.Errorf("register request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read register response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(body, &errResp)
		if errResp.Error != "" {
			return "", fmt.Errorf("registration failed: %s", errResp.Error)
		}
		return "", fmt.Errorf("registration failed with status %d", resp.StatusCode)
	}

	var result struct {
		Token    string `json:"token"`
		RelayURL string `json:"relay_url"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse register response: %w", err)
	}

	if result.Token == "" {
		return "", fmt.Errorf("empty token in register response")
	}

	return result.Token, nil
}
