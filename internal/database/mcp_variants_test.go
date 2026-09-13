package database

import "testing"

func TestProjectTargetsWindowsFromStoredPathShape(t *testing.T) {
	cases := []struct {
		name    string
		project Project
		want    bool
	}{
		{"native windows path", Project{Type: "remote", Path: `C:\Users\dev\projects\sample`}, true},
		{"sftp windows path", Project{Type: "remote", Path: "/C:/Users/dev/sample"}, true},
		{"lowercase drive", Project{Type: "remote", Path: `d:\srv\app`}, true},
		{"posix remote", Project{Type: "remote", Path: "/home/dev/openpoet"}, false},
		{"posix remote root", Project{Type: "remote", Path: "/"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.project.TargetsWindows(); got != tc.want {
				t.Fatalf("TargetsWindows(%q) = %v, want %v", tc.project.Path, got, tc.want)
			}
		})
	}
}

func TestMCPServerPartsForSelectsVariantPerOS(t *testing.T) {
	server := MCPServer{
		Command: "/home/dev/.nvm/current/bin/playwright-mcp",
		Args:    `["--headless"]`,
		Env:     `{"PLAYWRIGHT_CHROMIUM_SANDBOX":"false"}`,
	}

	// Without a variant the primary command serves every host, which is what
	// every server that predates the variants relies on.
	for _, windows := range []bool{false, true} {
		command, args, env := server.PartsFor(windows)
		if command != server.Command || args != server.Args || env != server.Env {
			t.Fatalf("windows=%v changed a variant-less server: %q %q %q", windows, command, args, env)
		}
	}

	server.CommandWindows = "node"
	server.ArgsWindows = `["C:\\Users\\dev\\AppData\\Roaming\\npm\\node_modules\\@playwright\\mcp\\cli.js","--headless"]`

	command, args, env := server.PartsFor(false)
	if command != server.Command || args != server.Args {
		t.Fatalf("linux target got the windows variant: %q %q", command, args)
	}

	command, args, env = server.PartsFor(true)
	if command != "node" || args != server.ArgsWindows {
		t.Fatalf("windows target did not get the variant: %q %q", command, args)
	}
	// Env has no Windows counterpart here, so it falls back individually.
	if env != server.Env {
		t.Fatalf("windows env did not fall back to the primary one: %q", env)
	}
}
