package jsonlview

import "testing"

func TestClaudeConfigDir(t *testing.T) {
	cases := []struct{ env, want string }{
		{"", "/home/dev/.claude"},
		{"   ", "/home/dev/.claude"},
		{"/home/dev/.claude-dev", "/home/dev/.claude-dev"},
		{"/home/dev/.claude-dev/", "/home/dev/.claude-dev"},
		{"~/.claude-dev", "/home/dev/.claude-dev"},
		{"~", "/home/dev"},
		{`C:\Users\dev\.claude`, "/home/dev/.claude"},
		{"relative/dir", "/home/dev/.claude"},
	}
	for _, c := range cases {
		if got := ClaudeConfigDir("/home/dev", c.env); got != c.want {
			t.Errorf("ClaudeConfigDir(%q) = %q, want %q", c.env, got, c.want)
		}
	}
}

func TestResolveRemoteJSONLPathUsesConfigDir(t *testing.T) {
	got := ResolveRemoteJSONLPath("/workspace/mylifeos", "abc", "/home/developer/.claude-dev")
	want := "/home/developer/.claude-dev/projects/-workspace-mylifeos/abc.jsonl"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveJSONLPathHonorsConfigDirEnv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/opt/claude-cfg")
	got := ResolveJSONLPath("/nonexistent/my_proj", "abc")
	want := "/opt/claude-cfg/projects/-nonexistent-my-proj/abc.jsonl"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
