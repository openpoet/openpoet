package coordinator

import "testing"

func TestExtractTouchesClassifiesTools(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input map[string]interface{}
		want  []Touch
	}{
		{"edit is a write claim", "Edit", map[string]interface{}{"file_path": "/p/shared.go"}, []Touch{{Kind: KindWrite, Path: "/p/shared.go"}}},
		{"write is a write claim", "Write", map[string]interface{}{"file_path": "/p/a.go"}, []Touch{{Kind: KindWrite, Path: "/p/a.go"}}},
		{"multiedit is a write claim", "MultiEdit", map[string]interface{}{"file_path": "/p/b.go"}, []Touch{{Kind: KindWrite, Path: "/p/b.go"}}},
		{"notebookedit uses notebook_path", "NotebookEdit", map[string]interface{}{"notebook_path": "/p/n.ipynb"}, []Touch{{Kind: KindWrite, Path: "/p/n.ipynb"}}},
		{"notebookedit ignores file_path", "NotebookEdit", map[string]interface{}{"file_path": "/p/wrong.go"}, nil},
		{"read is a read touch", "Read", map[string]interface{}{"file_path": "/p/c.go"}, []Touch{{Kind: KindRead, Path: "/p/c.go"}}},
		{"grep is a scope touch", "Grep", map[string]interface{}{"path": "/p", "pattern": "x"}, []Touch{{Kind: KindRead, Path: "/p", Scope: true}}},
		{"glob is a scope touch", "Glob", map[string]interface{}{"path": "/p"}, []Touch{{Kind: KindRead, Path: "/p", Scope: true}}},
		{"bash is opaque, never a path", "Bash", map[string]interface{}{"command": "sed -i s/x/y/ /p/shared.go"}, []Touch{{Kind: KindOpaque}}},
		{"unknown tool probes keys as read", "SomeTool", map[string]interface{}{"filePath": "/p/d.go"}, []Touch{{Kind: KindRead, Path: "/p/d.go"}}},
		{"unknown tool with no path is opaque", "SomeTool", map[string]interface{}{"foo": "bar"}, []Touch{{Kind: KindOpaque}}},
		{"edit missing path yields nothing", "Edit", map[string]interface{}{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractTouches(tc.tool, tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("touch count = %d, want %d (%+v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("touch[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestBashNeverProducesPathClaim is the negative control the whole
// deterministic-extraction posture rests on (spec N6): a Bash edit is opaque,
// so two Bash sessions cannot produce a false file_overlap.
func TestBashNeverProducesPathClaim(t *testing.T) {
	touches := ExtractTouches("Bash", map[string]interface{}{"command": "echo hi >> /p/shared.go"})
	for _, tch := range touches {
		if tch.Path != "" || tch.Kind == KindWrite {
			t.Fatalf("Bash produced a path/write claim: %+v", tch)
		}
	}
}

func TestIsClaudeDirPath(t *testing.T) {
	yes := []string{".claude/settings.json", ".claude", "sub/.claude/x", "./.claude/hooks/bridge.sh"}
	no := []string{"shared.go", "claude.go", "src/claude/x.go", "a/b.go"}
	for _, p := range yes {
		if !IsClaudeDirPath(p) {
			t.Errorf("IsClaudeDirPath(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if IsClaudeDirPath(p) {
			t.Errorf("IsClaudeDirPath(%q) = true, want false", p)
		}
	}
}
