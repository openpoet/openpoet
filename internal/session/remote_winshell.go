package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// isWindowsServer returns true if the SSH server banner indicates an OpenSSH
// for Windows server. Format example: "SSH-2.0-OpenSSH_for_Windows_8.6".
func isWindowsServer(client *ssh.Client) bool {
	return strings.Contains(string(client.ServerVersion()), "Windows")
}

// buildWindowsBatchScript renders a .cmd script that sets the given env vars,
// changes to the project path, and execs the selected backend with the supplied args.
// The .cmd file is the only safe way to pass complex args (like JSON for
// --mcp-config) to a Windows command — cmd.exe inline quoting is too fragile.
//
// IMPORTANT: cmd.exe's batch parser truncates a quoted arg at the first newline,
// so callers must not pass cliArgs whose values contain `\r` or `\n`. For
// multi-line content (e.g. a task system prompt), put it elsewhere (env var,
// file claude reads via MCP, etc.) and keep cliArgs single-line.
func buildWindowsBatchScript(envVars map[string]string, projectPath string, command string, cliArgs []string) string {
	var sb strings.Builder
	sb.WriteString("@echo off\r\n")
	// Add common npm-global location to PATH so CLI shims like `claude.cmd`
	// and `codex.cmd` are found in non-interactive OpenSSH sessions.
	sb.WriteString(`set "PATH=%APPDATA%\npm;%LOCALAPPDATA%\Programs\claude;%PATH%"` + "\r\n")
	for k, v := range envVars {
		sb.WriteString(fmt.Sprintf("set \"%s=%s\"\r\n", k, batchSetValue(v)))
	}

	cdPath := normalizeWindowsPath(projectPath)
	if cdPath != "" {
		// `cd /d` silently sets ERRORLEVEL but keeps the script running, which would
		// launch claude from the user's home directory — and claude wouldn't pick up
		// the project's .claude/settings.json (so hooks never fire). Abort loudly
		// instead, so the cause shows up in the OpenPoet terminal.
		sb.WriteString(fmt.Sprintf("cd /d \"%s\"\r\n", cdPath))
		sb.WriteString(fmt.Sprintf("if errorlevel 1 (echo [openpoet] cannot enter project path: %s 1>&2 & exit /b 1)\r\n", cdPath))
	}

	if strings.TrimSpace(command) == "" {
		command = "claude"
	}
	sb.WriteString(cmdQuoteArg(command))
	for _, a := range cliArgs {
		sb.WriteByte(' ')
		sb.WriteString(cmdQuoteArg(a))
	}
	sb.WriteString("\r\n")
	return sb.String()
}

// normalizeWindowsPath maps unix-style paths to a form cmd.exe accepts in `cd /d`.
//
// The remote SSH/SFTP file browser stores the path that Windows OpenSSH SFTP
// returns ("/C:/Users/foo"), which cmd.exe rejects because the leading "/" is
// parsed as the option-prefix character. Strip the leading slash and convert
// "/" to "\" so we end up with a native form ("C:\Users\foo") that cd /d, the
// `if exist` validation probe, and Windows file APIs all accept.
func normalizeWindowsPath(p string) string {
	switch p {
	case "", "/", "\\":
		return `%SystemDrive%\`
	}
	// "/C:/Users/foo" -> "C:/Users/foo": SFTP-style absolute Windows path.
	if len(p) >= 4 && p[0] == '/' && isASCIILetter(p[1]) && p[2] == ':' && (p[3] == '/' || p[3] == '\\') {
		p = p[1:]
	}
	return strings.ReplaceAll(p, "/", `\`)
}

// batchSetValue escapes a value for use inside `set "VAR=value"`. The quoted
// form already neutralizes most shell metacharacters; the only thing we need
// to handle is `%` (variable expansion) and a stray `"` would terminate the
// quoted region.
func batchSetValue(v string) string {
	v = strings.ReplaceAll(v, "%", "%%")
	// A literal `"` in the value would close the quoted region; replace with `""`.
	v = strings.ReplaceAll(v, `"`, `""`)
	return v
}

// cmdQuoteArg quotes a single argument so cmd.exe + the standard Microsoft C
// runtime parser deliver the original string to the target program. The rules
// (per https://learn.microsoft.com/cpp/cpp/main-function-command-line-args):
//
//   - wrap in "..."
//   - inside the wrap, escape `"` as `\"`
//   - any backslashes immediately before a `"` (or the closing quote) must be
//     doubled
//
// On top of that, we double `%` so cmd.exe won't expand env-var references
// inside the argument when running from a batch file.
func cmdQuoteArg(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	backslashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			backslashes++
			sb.WriteByte('\\')
		case '"':
			for j := 0; j < backslashes; j++ {
				sb.WriteByte('\\')
			}
			sb.WriteString(`\"`)
			backslashes = 0
		default:
			backslashes = 0
			sb.WriteByte(c)
		}
	}
	for j := 0; j < backslashes; j++ {
		sb.WriteByte('\\')
	}
	sb.WriteByte('"')
	return strings.ReplaceAll(sb.String(), "%", "%%")
}

// uploadWindowsLauncher writes the launcher script to the user's home
// directory on the remote Windows host via SFTP and returns:
//   - the SFTP-style path (used to write/delete the file via SFTP)
//   - the cmd.exe-style path (used to invoke it from the SSH exec channel)
//
// Windows OpenSSH SFTP returns absolute paths in POSIX form like
// "/C:/Users/adm/foo.cmd". cmd.exe doesn't parse that — it needs
// "C:\Users\adm\foo.cmd". We hand the SFTP form back so removeRemoteFile
// can use the same client; the cmd form is what actually executes.
func uploadWindowsLauncher(client *ssh.Client, content string) (sftpPath, cmdPath string, err error) {
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return "", "", fmt.Errorf("sftp connect: %w", err)
	}
	defer sftpClient.Close()

	suffix, err := randomHex(6)
	if err != nil {
		return "", "", err
	}
	name := fmt.Sprintf("openpoet-launch-%s.cmd", suffix)

	abs, err := sftpClient.RealPath(name)
	if err != nil {
		// RealPath can fail on some Windows OpenSSH builds; fall back to relative.
		abs = name
	}

	f, err := sftpClient.Create(abs)
	if err != nil {
		return "", "", fmt.Errorf("sftp create %s: %w", abs, err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		f.Close()
		return "", "", fmt.Errorf("sftp write: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", "", fmt.Errorf("sftp close: %w", err)
	}
	return abs, sftpToWindowsPath(abs), nil
}

// sftpToWindowsPath converts the POSIX-style absolute path returned by
// Windows OpenSSH SFTP ("/C:/Users/adm/foo.cmd") into the native cmd.exe
// form ("C:\Users\adm\foo.cmd"). Paths that don't match the Windows-drive
// pattern are returned with `/` swapped for `\` only.
func sftpToWindowsPath(p string) string {
	if len(p) >= 4 && p[0] == '/' && isASCIILetter(p[1]) && p[2] == ':' && (p[3] == '/' || p[3] == '\\') {
		p = p[1:]
	}
	return strings.ReplaceAll(p, "/", `\`)
}

func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// removeRemoteFile is a best-effort cleanup of a file uploaded with sftp.
func removeRemoteFile(client *ssh.Client, path string) {
	if client == nil || path == "" {
		return
	}
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return
	}
	defer sftpClient.Close()
	_ = sftpClient.Remove(path)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
