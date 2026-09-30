package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sessionMessagesServer(t *testing.T, body string, query *string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/sessions/s1/messages" {
			t.Fatalf("request = %s %s, want GET /api/sessions/s1/messages", r.Method, r.URL.Path)
		}
		*query = r.URL.RawQuery
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSessionMessagesToolForwardsQueryAndFormatsList(t *testing.T) {
	var query string
	server := sessionMessagesServer(t, `{"session_id":"s1","source":"transcript","mode":"list","total":42,
		"messages":[{"id":"0000000a","role":"user","at":"2026-09-29T22:20:22.533Z","chars":464,"text":"Faça o deploy…","truncated":true},
		{"id":"0000000b","role":"assistant","model":"claude-opus-5-5","at":"2026-09-29T22:21:00Z","chars":7,"text":"Feito."}],
		"has_more":true,"next_before_id":"0000000a"}`, &query)

	result, err := executeTool(NewAPIClient(server.URL), "openpoet_session_messages",
		json.RawMessage(`{"session_id":"s1","last_n":"2","role":"user","max_chars":"300","before_id":"0000000c"}`), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if query != "before_id=0000000c&last_n=2&max_chars=300&role=user" {
		t.Fatalf("query = %q", query)
	}
	want := "Session messages: s1 | source: transcript | mode: list | showing 2 of 42 | older: before_id=0000000a\n\n" +
		"[0000000a] user · 2026-09-29 22:20 UTC · 464 chars (truncated; expand for full text)\nFaça o deploy…\n\n" +
		"[0000000b] assistant · 2026-09-29 22:21 UTC · 7 chars\nFeito."
	if result != want {
		t.Fatalf("result =\n%s\nwant\n%s", result, want)
	}
}

func TestSessionMessagesToolFormatsSearchAndExpand(t *testing.T) {
	var query string
	server := sessionMessagesServer(t, `{"session_id":"s1","source":"transcript","mode":"search","search":"deploy",
		"hits":[{"id":"0000000a","role":"user","at":"2026-09-29T22:20:22Z","chars":464,"matches":2,"offset":8,"snippet":"Faça o deploy agora"}],"has_more":false}`, &query)
	result, err := executeTool(NewAPIClient(server.URL), "openpoet_session_messages", json.RawMessage(`{"session_id":"s1","search":"deploy"}`), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if query != "search=deploy" || !strings.Contains(result, `mode: search "deploy" | hits: 1`) ||
		!strings.Contains(result, "[0000000a] user · 2026-09-29 22:20 UTC · 464 chars · 2 match(es), first at offset 8\nFaça o deploy agora") {
		t.Fatalf("query = %q, result =\n%s", query, result)
	}

	server = sessionMessagesServer(t, `{"session_id":"s1","source":"transcript","mode":"expand",
		"message":{"id":"0000000a","role":"user","at":"2026-09-29T22:20:22Z","chars":16003,"offset":8000,"text":"meio","next_offset":16000},"has_more":true}`, &query)
	result, err = executeTool(NewAPIClient(server.URL), "openpoet_session_messages", json.RawMessage(`{"session_id":"s1","expand":"0000000a","offset":8000}`), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if query != "expand=0000000a&offset=8000" || !strings.HasSuffix(result, "16003 chars · offset 8000-8004 · next_offset: 16000\n\nmeio") {
		t.Fatalf("query = %q, result =\n%s", query, result)
	}
}

func TestSessionMessagesToolRequiresSessionID(t *testing.T) {
	if _, err := executeTool(NewAPIClient("http://127.0.0.1:1"), "openpoet_session_messages", json.RawMessage(`{}`), "", ""); err == nil {
		t.Fatal("missing session_id was accepted")
	}
}
