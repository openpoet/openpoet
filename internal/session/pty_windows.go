//go:build windows

// NOTE: Windows support is experimental and not functional.
// This code is retained for potential future use but is not built,
// tested, or supported. Windows users should use WSL instead.

package session

import (
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/UserExistsError/conpty"
)

// windowsPTY wraps a Windows ConPTY pseudo-console.
type windowsPTY struct {
	cpty *conpty.ConPty
}

func startPTY(cmd *exec.Cmd, rows, cols uint16) (ptyHandle, error) {
	var cmdLine string

	if needsWSL(cmd.Path) {
		// The binary doesn't exist on Windows — run it inside WSL.
		cmdLine = buildWSLCommandLine(
			windowsToWSLPath(cmd.Dir),
			cmd.Path,
			cmd.Args[1:],
			cmd.Env,
		)
	} else {
		// Native Windows binary — run directly with ConPTY.
		cmdLine = buildNativeCommandLine(cmd.Path, cmd.Args[1:])
	}

	log.Printf("[conpty] Starting: %s", cmdLine)

	cpty, err := conpty.Start(cmdLine,
		conpty.ConPtyDimensions(int(cols), int(rows)),
		conpty.ConPtyWorkDir(cmd.Dir),
		conpty.ConPtyEnv(cmd.Env),
	)
	if err != nil {
		return nil, fmt.Errorf("conpty start: %w", err)
	}

	return &windowsPTY{cpty: cpty}, nil
}

func (p *windowsPTY) Read(buf []byte) (int, error)  { return p.cpty.Read(buf) }
func (p *windowsPTY) Write(buf []byte) (int, error) { return p.cpty.Write(buf) }
func (p *windowsPTY) Close() error                  { return p.cpty.Close() }

func (p *windowsPTY) Resize(rows, cols uint16) error {
	return p.cpty.Resize(int(cols), int(rows))
}

// needsWSL returns true if the binary path doesn't look like a native Windows
// executable. Self-exec (openpoet.exe) and .exe binaries run natively;
// everything else (e.g. "claude", "/usr/bin/claude") needs WSL.
func needsWSL(binaryPath string) bool {
	lower := strings.ToLower(binaryPath)

	// Explicit .exe extension → native
	if strings.HasSuffix(lower, ".exe") {
		return false
	}

	// Windows-style absolute path (C:\...) → native
	if len(lower) >= 3 && lower[1] == ':' && (lower[2] == '\\' || lower[2] == '/') {
		return false
	}

	// Unix-style path or bare command name → needs WSL
	return true
}

// buildNativeCommandLine constructs a command line for native Windows execution.
func buildNativeCommandLine(binary string, args []string) string {
	parts := []string{windowsQuote(binary)}
	for _, arg := range args {
		parts = append(parts, windowsQuote(arg))
	}
	return strings.Join(parts, " ")
}

// windowsQuote quotes an argument for Windows command-line parsing.
// If the argument contains spaces, quotes, or is empty, it wraps in double
// quotes with internal quotes escaped.
func windowsQuote(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, ` "`) {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// windowsToWSLPath converts a Windows path like C:\Users\foo\project
// to its WSL equivalent /mnt/c/Users/foo/project.
func windowsToWSLPath(winPath string) string {
	// Already a WSL/Unix path
	if strings.HasPrefix(winPath, "/") {
		return winPath
	}

	winPath = strings.ReplaceAll(winPath, "\\", "/")

	// C:/Users/foo → /mnt/c/Users/foo
	if len(winPath) >= 2 && winPath[1] == ':' {
		drive := strings.ToLower(string(winPath[0]))
		rest := winPath[2:] // skip "C:"
		return "/mnt/" + drive + rest
	}

	return winPath
}

// buildWSLCommandLine constructs the full command line string for wsl.exe
// to run a Linux binary inside WSL with the proper environment.
func buildWSLCommandLine(wslDir, binary string, args []string, env []string) string {
	wslBinary := windowsToWSLPath(binary)

	// Build env exports for the WSL shell.
	var exports []string
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := parts[0]
		val := parts[1]

		// Skip Windows-specific env vars
		switch key {
		case "SYSTEMROOT", "COMSPEC", "PATHEXT", "WINDIR",
			"APPDATA", "LOCALAPPDATA", "PROGRAMFILES", "USERPROFILE":
			continue
		}

		// For PATH, use Linux-style paths
		if key == "PATH" {
			val = "/usr/local/bin:/usr/bin:/bin:$HOME/.local/bin:$HOME/.nvm/current/bin"
			exports = append(exports, fmt.Sprintf("export PATH='%s'", shellEscapeSingle(val)))
			continue
		}

		exports = append(exports, fmt.Sprintf("export %s='%s'", key, shellEscapeSingle(val)))
	}

	// Build the inner command: cd + exports + exec
	var sb strings.Builder
	sb.WriteString("wsl.exe -- bash -lc '")

	// cd to project directory
	sb.WriteString(fmt.Sprintf("cd %s && ", shellEscapeSingle(wslDir)))

	// Export env vars
	for _, exp := range exports {
		sb.WriteString(exp + " && ")
	}

	// Execute the binary
	sb.WriteString(shellEscapeSingle(wslBinary))
	for _, arg := range args {
		sb.WriteString(" " + shellEscapeSingle(arg))
	}

	sb.WriteString("'")
	return sb.String()
}

// shellEscapeSingle escapes a string for use inside single quotes in bash.
func shellEscapeSingle(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}
