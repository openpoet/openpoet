package database

import "runtime"

// TargetsWindows reports whether the sessions of this project run on Windows.
//
// Remote projects are judged by the shape of their stored path: the SSH/SFTP
// browser records either the native form ("C:\Users\dev\...") or the form
// Windows OpenSSH SFTP returns ("/C:/Users/dev/..."). Both are unambiguous,
// and reading them costs no SSH round-trip — the banner check in the session
// runner is the authority once a connection exists, this is for the config that
// has to be written before one does.
func (p *Project) TargetsWindows() bool {
	if p == nil {
		return false
	}
	if p.Type != "remote" {
		return runtime.GOOS == "windows"
	}
	return looksLikeWindowsPath(p.Path)
}

func looksLikeWindowsPath(path string) bool {
	if len(path) >= 3 && path[0] == '/' {
		path = path[1:]
	}
	return len(path) >= 2 && isASCIILetter(path[0]) && path[1] == ':'
}

func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// PartsFor returns the command, args and env to inject for a target operating
// system. A global MCP server reaches every project, so the single absolute
// path it stores can only satisfy one OS; the Windows variant covers the rest
// of a mixed fleet under the same server name.
//
// An empty CommandWindows means the server has no variant and the primary
// command serves every host — the behavior of every server that predates the
// variants. Args and env fall back to the primary ones individually, so a
// variant that only differs in its binary path need not restate them.
func (m MCPServer) PartsFor(windows bool) (command, args, env string) {
	if !windows || m.CommandWindows == "" {
		return m.Command, m.Args, m.Env
	}
	args, env = m.ArgsWindows, m.EnvWindows
	if args == "" {
		args = m.Args
	}
	if env == "" {
		env = m.Env
	}
	return m.CommandWindows, args, env
}
