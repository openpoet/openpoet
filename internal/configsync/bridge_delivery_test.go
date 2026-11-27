package configsync

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type bridgeHit struct {
	event, id, at string
}

// bridgeServer records every /api/hooks/event delivery and fails the first
// failFirst attempts of each event id with 503.
func bridgeServer(t *testing.T, failFirst int) (*httptest.Server, func() []bridgeHit) {
	t.Helper()
	var mu sync.Mutex
	var hits []bridgeHit
	attempts := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		event := "?"
		if i := strings.Index(string(body), `"hook_event_name":"`); i >= 0 {
			rest := string(body)[i+len(`"hook_event_name":"`):]
			event = rest[:strings.Index(rest, `"`)]
		}
		id := r.Header.Get("X-Hook-Event-ID")
		mu.Lock()
		hits = append(hits, bridgeHit{event: event, id: id, at: r.Header.Get("X-Hook-Event-At")})
		attempts[id]++
		fail := attempts[id] <= failFirst
		mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server, func() []bridgeHit {
		mu.Lock()
		defer mu.Unlock()
		return append([]bridgeHit(nil), hits...)
	}
}

func writeBridge(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func runBridge(t *testing.T, bridge, url, input string) {
	t.Helper()
	cmd := exec.Command("bash", bridge)
	cmd.Env = append(os.Environ(), "OPENPOET_HOOK_URL="+url, "OPENPOET_SESSION_ID=sess-bridge", "OPENPOET_HOOK_TOKEN=tok")
	cmd.Stdin = strings.NewReader(input)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bridge failed: %v %s", err, out)
	}
}

func TestBridgeScriptsParse(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	for name, script := range map[string]string{"local": localBridgeScript, "remote": remoteBridgeScript} {
		if out, err := exec.Command("bash", "-n", writeBridge(t, script)).CombinedOutput(); err != nil {
			t.Fatalf("%s bridge does not parse: %v %s", name, err, out)
		}
		for _, want := range []string{"X-Hook-Event-ID: ${EVENT_ID}", "X-Hook-Event-At: ${EVENT_AT}", "--retry-connrefused", "Stop|Notification)\n        bridge_post_event\n"} {
			if !strings.Contains(script, want) {
				t.Fatalf("%s bridge lacks %q", name, want)
			}
		}
	}
}

// Stop is retried after a 503 with the same id and emission time, and the
// hook does not return before the server has it (it closes the turn).
func TestBridgeDeliversStopSynchronouslyWithRetry(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	server, hits := bridgeServer(t, 1)
	bridge := writeBridge(t, localBridgeScript)
	started := time.Now()
	runBridge(t, bridge, server.URL, `{"hook_event_name":"Stop","session_id":"x"}`)
	got := hits() // read right after the hook returned: no waiting
	if len(got) != 2 {
		t.Fatalf("deliveries when the Stop hook returned = %+v, want a failed attempt and its retry", got)
	}
	if got[0].id == "" || got[0].id != got[1].id || got[0].at != got[1].at {
		t.Fatalf("retry changed identity: %+v", got)
	}
	if at, err := strconv.ParseInt(got[0].at, 10, 64); err != nil || at < started.Add(-time.Minute).UnixNano() || at > time.Now().Add(time.Minute).UnixNano() {
		t.Fatalf("emission time %q is not nanoseconds near now", got[0].at)
	}
}

// UserPromptSubmit stays in the background (a prompt never waits on the
// server) and is still retried; two events get distinct ids.
func TestBridgeDeliversPromptInBackgroundWithRetry(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	server, hits := bridgeServer(t, 1)
	bridge := writeBridge(t, remoteBridgeScript)
	runBridge(t, bridge, server.URL, `{"hook_event_name":"UserPromptSubmit","prompt":"hi"}`)
	runBridge(t, bridge, server.URL, `{"hook_event_name":"Notification","message":"Claude is waiting for your input"}`)
	deadline := time.Now().Add(15 * time.Second)
	for {
		ids := map[string]map[string]int{}
		for _, hit := range hits() {
			if ids[hit.event] == nil {
				ids[hit.event] = map[string]int{}
			}
			ids[hit.event][hit.id]++
		}
		if len(ids["UserPromptSubmit"]) == 1 && len(ids["Notification"]) == 1 {
			var promptID, notifyID string
			for id, n := range ids["UserPromptSubmit"] {
				promptID = id
				if n < 2 {
					break
				}
			}
			for id := range ids["Notification"] {
				notifyID = id
			}
			if ids["UserPromptSubmit"][promptID] >= 2 {
				if promptID == notifyID {
					t.Fatalf("two events share id %q", promptID)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("deliveries = %+v", hits())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
