package handlers

import "unicode/utf8"

// cp1252HighBytes maps CP1252 (Windows-1252) bytes 0x80–0x9F to their Unicode
// codepoints. Bytes 0xA0–0xFF in CP1252 are identical to Latin-1 (the codepoint
// equals the byte value), so we don't need a table for them. Five slots in the
// 0x80–0x9F range are unassigned in CP1252 (0x81, 0x8D, 0x8F, 0x90, 0x9D);
// they're left at 0xFFFD to preserve the corruption signal rather than guess.
var cp1252HighBytes = [32]rune{
	0x20AC, 0xFFFD, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021, // 80–87
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0xFFFD, 0x017D, 0xFFFD, // 88–8F
	0xFFFD, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, // 90–97
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0xFFFD, 0x017E, 0x0178, // 98–9F
}

// normalizeUTF8Body returns body unchanged when it's already valid UTF-8.
// Otherwise it transcodes the bytes from CP1252 to UTF-8.
//
// Windows claude.exe pipes hook input JSON to bridge.sh using the host's ANSI
// codepage (typically CP1252 on Western installs) instead of UTF-8, so a single
// `é` arrives as 0xE9 — which Go's JSON decoder then replaces with U+FFFD,
// breaking AskUserQuestion answer lookup (the answer key never matches the
// re-rendered question text). Transcoding here recovers the original chars so
// downstream code sees valid UTF-8 regardless of the remote codepage.
func normalizeUTF8Body(body []byte) []byte {
	if utf8.Valid(body) {
		return body
	}
	out := make([]byte, 0, len(body)+len(body)/8)
	for _, b := range body {
		switch {
		case b < 0x80:
			out = append(out, b)
		case b < 0xA0:
			out = utf8.AppendRune(out, cp1252HighBytes[b-0x80])
		default:
			out = utf8.AppendRune(out, rune(b))
		}
	}
	return out
}
