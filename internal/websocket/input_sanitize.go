package websocket

import "strings"

func sanitizeTerminalClientInput(input string) string {
	if input == "" {
		return input
	}
	var out strings.Builder
	out.Grow(len(input))
	for i := 0; i < len(input); {
		if n := terminalResponseSequenceLen(input[i:]); n > 0 {
			i += n
			continue
		}
		out.WriteByte(input[i])
		i++
	}
	if out.Len() == len(input) {
		return input
	}
	return out.String()
}

func terminalResponseSequenceLen(s string) int {
	if len(s) == 0 {
		return 0
	}
	if s[0] == 0x1b {
		return escapedTerminalResponseLen(s)
	}
	return orphanTerminalResponseLen(s)
}

func escapedTerminalResponseLen(s string) int {
	if len(s) < 2 || s[0] != 0x1b {
		return 0
	}
	switch s[1] {
	case '[':
		if n := csiTerminalResponseLen(s[2:]); n > 0 {
			return 2 + n
		}
	case ']':
		if n := oscTerminalResponseLen(s[2:]); n > 0 {
			return 2 + n
		}
	}
	return 0
}

func orphanTerminalResponseLen(s string) int {
	if strings.HasPrefix(s, "[O") {
		if n := orphanTerminalResponseLen(s[2:]); n > 0 {
			return 2 + n
		}
	}
	switch {
	case strings.HasPrefix(s, "["):
		if n := csiTerminalResponseLen(s[1:]); n > 0 {
			return 1 + n
		}
	case strings.HasPrefix(s, "]"):
		if n := oscTerminalResponseLen(s[1:]); n > 0 {
			return 1 + n
		}
	}
	return 0
}

func csiTerminalResponseLen(s string) int {
	if len(s) == 0 {
		return 0
	}
	i := 0
	for i < len(s) && isCSIParamByte(s[i]) {
		i++
	}
	if i >= len(s) {
		return 0
	}
	final := s[i]
	if final == 'R' || final == 'c' || (final == 'u' && strings.HasPrefix(s[:i], "<")) {
		return i + 1
	}
	return 0
}

func oscTerminalResponseLen(s string) int {
	if !(strings.HasPrefix(s, "10;") || strings.HasPrefix(s, "11;")) {
		return 0
	}
	for i := 3; i < len(s); i++ {
		if s[i] == '\a' {
			return i + 1
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
			return i + 2
		}
		if s[i] == '\\' {
			return i + 1
		}
		if s[i] == '\r' || s[i] == '\n' {
			return 0
		}
	}
	return 0
}

func isCSIParamByte(b byte) bool {
	return (b >= '0' && b <= '9') || b == ';' || b == '?' || b == '>' || b == '<'
}
