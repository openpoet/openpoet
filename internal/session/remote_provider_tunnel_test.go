package session

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type localRemoteListenerClient struct {
	listenCalls int
}

func (c *localRemoteListenerClient) Listen(network, address string) (net.Listener, error) {
	c.listenCalls++
	return net.Listen(network, address)
}

func TestRemoteProviderTunnelForwardsAuthenticatedRequests(t *testing.T) {
	type upstreamRequest struct {
		method        string
		path          string
		query         string
		authorization string
		apiKey        string
		body          string
	}
	requests := make(chan upstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- upstreamRequest{
			method:        r.Method,
			path:          r.URL.Path,
			query:         r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"),
			apiKey:        r.Header.Get("X-Api-Key"),
			body:          string(body),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	runner := &RemoteRunner{envVars: map[string]string{
		RemoteProviderTunnelEnv: "1",
		"ANTHROPIC_BASE_URL":    upstream.URL,
		"ANTHROPIC_AUTH_TOKEN":  "unused",
	}}
	client := &localRemoteListenerClient{}
	if err := runner.setupProviderTunnel(client); err != nil {
		t.Fatal(err)
	}
	defer runner.providerTunnelServer.Close()

	if client.listenCalls != 1 {
		t.Fatalf("remote listen calls = %d, want 1", client.listenCalls)
	}
	if _, ok := runner.envVars[RemoteProviderTunnelEnv]; ok {
		t.Fatal("internal tunnel marker was exported")
	}
	if runner.envVars["ANTHROPIC_BASE_URL"] == upstream.URL {
		t.Fatal("remote process received the local bridge URL")
	}
	token := runner.envVars["ANTHROPIC_AUTH_TOKEN"]
	if token == "" || token == "unused" {
		t.Fatalf("session tunnel credential = %q", token)
	}

	request, err := http.NewRequest(http.MethodPost, runner.envVars["ANTHROPIC_BASE_URL"]+"/v1/messages?beta=1", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("response status = %d", response.StatusCode)
	}

	select {
	case got := <-requests:
		if got.method != http.MethodPost || got.path != "/v1/messages" || got.query != "beta=1" || got.body != "payload" {
			t.Fatalf("upstream request = %+v", got)
		}
		if got.authorization != "Bearer unused" || got.apiKey != "" {
			t.Fatalf("forwarded authentication = authorization %q, api key %q", got.authorization, got.apiKey)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provider tunnel did not reach the local bridge")
	}

	remoteBaseURL := runner.envVars["ANTHROPIC_BASE_URL"]
	if err := runner.Stop(); err != nil {
		t.Fatal(err)
	}
	probe := &http.Client{Timeout: time.Second}
	if response, err := probe.Get(remoteBaseURL + "/v1/messages"); err == nil {
		response.Body.Close()
		t.Fatal("provider tunnel still accepted connections after runner stop")
	}
}

func TestRemoteProviderTunnelRejectsUnauthenticatedRequests(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	runner := &RemoteRunner{envVars: map[string]string{
		RemoteProviderTunnelEnv: "1",
		"ANTHROPIC_BASE_URL":    upstream.URL,
	}}
	if err := runner.setupProviderTunnel(&localRemoteListenerClient{}); err != nil {
		t.Fatal(err)
	}
	defer runner.providerTunnelServer.Close()

	response, err := http.Get(runner.envVars["ANTHROPIC_BASE_URL"] + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("response status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("unauthenticated request reached upstream %d times", upstreamCalls.Load())
	}
}

func TestProviderTunnelTargetMustBeLoopback(t *testing.T) {
	for _, raw := range []string{
		"https://127.0.0.1:443",
		"http://example.com:8080",
		"http://127.0.0.1",
		"http://user:password@127.0.0.1:8080",
	} {
		if _, err := parseProviderTunnelTarget(raw); err == nil {
			t.Errorf("parseProviderTunnelTarget(%q) unexpectedly succeeded", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"} {
		if _, err := parseProviderTunnelTarget(raw); err != nil {
			t.Errorf("parseProviderTunnelTarget(%q): %v", raw, err)
		}
	}
}

func TestRemoteProviderTunnelMarkerIsConsumedWhenDisabled(t *testing.T) {
	runner := &RemoteRunner{envVars: map[string]string{
		RemoteProviderTunnelEnv: "0",
		"ANTHROPIC_BASE_URL":    "http://127.0.0.1:8080",
	}}
	client := &localRemoteListenerClient{}
	if err := runner.setupProviderTunnel(client); err != nil {
		t.Fatal(err)
	}
	if client.listenCalls != 0 {
		t.Fatalf("remote listen calls = %d, want 0", client.listenCalls)
	}
	if _, ok := runner.envVars[RemoteProviderTunnelEnv]; ok {
		t.Fatal("disabled internal tunnel marker was exported")
	}
}
