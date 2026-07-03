package session

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
)

func TestRemoteRunnerRewritesOpenCodeOpenPoetMCPToTunnel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	r := &RemoteRunner{
		envVars: map[string]string{
			"OPENPOET_SESSION_ID":     "sid",
			"OPENCODE_CONFIG_CONTENT": `{"mcp":{"openpoet":{"type":"local","command":["/tmp/openpoet","mcp-serve"],"enabled":true},"other":{"type":"local","command":["node","server.js"]}}}`,
		},
		backend:        &OpenCodeBackend{},
		tunnelListener: listener,
	}

	r.rewriteMCPConfigForRemote()

	var content map[string]interface{}
	if err := json.Unmarshal([]byte(r.envVars["OPENCODE_CONFIG_CONTENT"]), &content); err != nil {
		t.Fatal(err)
	}
	mcp := content["mcp"].(map[string]interface{})
	openpoet := mcp["openpoet"].(map[string]interface{})
	if openpoet["type"] != "remote" {
		t.Fatalf("openpoet type = %q", openpoet["type"])
	}
	url, _ := openpoet["url"].(string)
	if !strings.Contains(url, "/mcp?session_id=sid") {
		t.Fatalf("openpoet url = %q", url)
	}
	if _, ok := openpoet["command"]; ok {
		t.Fatalf("remote openpoet MCP should not keep command: %#v", openpoet)
	}
	if _, ok := mcp["other"]; !ok {
		t.Fatalf("other MCP was not preserved: %#v", mcp)
	}
}
