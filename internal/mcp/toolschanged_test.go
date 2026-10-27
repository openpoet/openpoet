package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// fakeToolsAPI serves the endpoints fetchPolicy reads for session s1 in
// project 7, whose global policy allows only openpoet_list_projects; shared
// toggles whether project 7 has a share (which auto-allows the share tools).
// It also serves the tool-changes stream the stdio transport subscribes to.
func fakeToolsAPI(t *testing.T, shared *atomic.Bool) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/config/tool-policies", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"session": `{"mode":"deny_all","allowed":["openpoet_list_projects"]}`})
	})
	r.Get("/api/sessions/s1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"project_id":7}`))
	})
	r.Get("/api/projects/7/tool-policy", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"tool_policy":""}`))
	})
	r.Get("/api/projects/7/shares", func(w http.ResponseWriter, _ *http.Request) {
		if shared.Load() {
			w.Write([]byte(`[{"project_id":8}]`))
			return
		}
		w.Write([]byte(`[]`))
	})
	r.Get("/api/mcp/tool-changes", ToolChangesStream)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func listedToolNames(t *testing.T, resp *jsonRPCResponse) string {
	t.Helper()
	raw, _ := json.Marshal(resp.Result)
	var result struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return toolNamesKey(result.Tools)
}

func hasShareTools(names string) bool {
	return strings.Contains(names, "openpoet_read_shared_file")
}

func TestInitializeDeclaresToolsListChanged(t *testing.T) {
	h := NewRequestHandler(nil, "", "session", "", DefaultPolicy())
	resp := h.Handle(&jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize"})
	raw, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(raw), `"tools":{"listChanged":true}`) {
		t.Fatalf("initialize result = %s, want tools.listChanged true", raw)
	}
}

// stdio transport: a session opened before its project got a share is told
// (through the server's tool-changes stream) that its list changed, re-lists
// and sees the share tools; removing the share notifies again.
func TestStdioSessionNotifiedWhenShareChanges(t *testing.T) {
	var shared atomic.Bool
	srv := fakeToolsAPI(t, &shared)
	client := NewAPIClient(srv.URL)
	h := NewRequestHandler(client, "s1", "session", "", ToolPolicy{Mode: "deny_all"})

	list := func() string {
		return listedToolNames(t, h.Handle(&jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "tools/list"}))
	}
	if names := list(); hasShareTools(names) {
		t.Fatalf("share tools listed before any share: %s", names)
	}

	notified := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchToolChanges(ctx, client, func() {
		if h.toolListChanged() {
			notified <- struct{}{}
		}
	})
	waitForSubscriber(t)

	for _, step := range []struct {
		shared bool
		want   bool
	}{{true, true}, {false, false}} {
		shared.Store(step.shared)
		NotifyToolsChanged()
		select {
		case <-notified:
		case <-time.After(2 * time.Second):
			t.Fatalf("shared=%v: no tools/list_changed", step.shared)
		}
		if names := list(); hasShareTools(names) != step.want {
			t.Fatalf("shared=%v: tools/list = %s", step.shared, names)
		}
	}

	// A change signal that leaves this session's list as it is stays silent.
	NotifyToolsChanged()
	select {
	case <-notified:
		t.Fatal("notified although the tool list did not change")
	case <-time.After(200 * time.Millisecond):
	}
}

// HTTP transport (remote sessions through the tunnel): the notification goes
// out on the session's GET /mcp SSE stream.
func TestHTTPSessionNotifiedOnStreamWhenShareChanges(t *testing.T) {
	var shared atomic.Bool
	api := fakeToolsAPI(t, &shared)
	h := &HTTPHandler{
		apiURL:    api.URL,
		getAPIKey: func() string { return "" },
		getPolicy: DefaultPolicy,
		sessions:  map[string]*httpSession{},
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	post := func(sessID, body string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp?session_id=s1", strings.NewReader(body))
		if sessID != "" {
			req.Header.Set("Mcp-Session-Id", sessID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out jsonRPCResponse
		json.NewDecoder(resp.Body).Decode(&out)
		if out.Result == nil {
			return resp, ""
		}
		return resp, listedToolNames(t, &out)
	}
	initResp, _ := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	sessID := initResp.Header.Get("Mcp-Session-Id")
	if _, names := post(sessID, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); hasShareTools(names) {
		t.Fatalf("share tools listed before any share: %s", names)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp?session_id=s1", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", sessID)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	data := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(stream.Body)
		for sc.Scan() {
			if line, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				data <- line
			}
		}
	}()
	waitForSubscriber(t)

	for _, step := range []struct {
		shared bool
		want   bool
	}{{true, true}, {false, false}} {
		shared.Store(step.shared)
		NotifyToolsChanged()
		select {
		case msg := <-data:
			if msg != string(toolListChangedNotification) {
				t.Fatalf("stream data = %s", msg)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("shared=%v: no tools/list_changed on the stream", step.shared)
		}
		if _, names := post(sessID, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`); hasShareTools(names) != step.want {
			t.Fatalf("shared=%v: tools/list = %s", step.shared, names)
		}
	}
}

// waitForSubscriber waits until some stream has subscribed to the hub, so a
// NotifyToolsChanged sent next is not lost.
func waitForSubscriber(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		toolsChanged.mu.Lock()
		n := len(toolsChanged.subs)
		toolsChanged.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(fmt.Errorf("no tool-changes subscriber"))
}
