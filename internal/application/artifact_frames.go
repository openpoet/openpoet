package application

import (
	"fmt"
	"regexp"
	"strings"
)

// An artifact is one document. Content placed in a nested frame (an <iframe>,
// srcdoc, <object>, <embed>) or in a shadow root is out of reach of the
// annotation bridge, which runs in the document OpenPoet serves: the user sees
// it but cannot comment on it. A nested frame is also a fresh JavaScript realm,
// with its own copy of the WebRTC constructors the render guard removes. So the
// viewer blocks nested frames at run time (artifact_guard.js), and a publish
// that builds them is refused here with a message that tells the agent what
// to write instead.
//
// This check reads the document as text. It is there for honest pages, so the
// agent learns about the rule when it publishes rather than from a page with
// holes in it; the run-time block is what a hostile page meets.

// artifactFrameFinding is one place in a document that builds a nested frame,
// a shadow root or writes into the parser.
type artifactFrameFinding struct {
	Line int
	What string
}

const maxArtifactFrameFindings = 8

var (
	// Elements that open a nested browsing context.
	artifactFrameTagPattern = regexp.MustCompile(`(?i)<(iframe|frameset|frame|object|embed|fencedframe|portal)[\s/>]`)
	// Any start tag, to look at its attributes.
	artifactStartTagPattern   = regexp.MustCompile(`(?is)<[a-z][a-z0-9-]*\b[^>]*>`)
	artifactSrcdocAttrPattern = regexp.MustCompile(`(?i)[\s"'/]srcdoc\s*=`)
	artifactShadowAttrPattern = regexp.MustCompile(`(?i)[\s"'/]shadowroot(?:mode)?\s*=`)
	// The value of an event-handler attribute (onclick="...") is script.
	artifactEventHandlerPattern = regexp.MustCompile(`(?is)[\s"'/]on[a-z]+\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	// Raw-text elements: their content is not markup, so an "<iframe" inside
	// <textarea> or <title> is text. Scripts are read separately.
	artifactRawTextOpenPattern = regexp.MustCompile(`(?i)<(script|style|textarea|title|xmp|noembed|noframes|noscript)\b[^>]*>`)
	artifactHTMLCommentPattern = regexp.MustCompile(`(?s)<!--.*?-->`)

	// In script (and in event-handler attributes).
	artifactScriptFramePatterns = []struct {
		pattern *regexp.Regexp
		what    string
	}{
		{regexp.MustCompile(`(?i)createElement(?:NS)?\s*\(\s*(?:[^,()]*,\s*)?["'` + "`" + `]\s*(iframe|frameset|frame|object|embed|fencedframe|portal)\s*["'` + "`" + `]`), "createElement('%s')"},
		{regexp.MustCompile(`(?i)(?:<|\\x3c|\\u003c)(iframe|frameset|frame|object|embed|fencedframe|portal)(?:[\s/>]|\\x3e|\\u003e)`), "<%s> markup built in script"},
		{regexp.MustCompile(`\.srcdoc\s*=[^=]`), "srcdoc assigned in script"},
		{regexp.MustCompile(`(?i)["'` + "`" + `]srcdoc["'` + "`" + `]`), "srcdoc set in script"},
		{regexp.MustCompile(`\battachShadow\s*\(`), "attachShadow()"},
		{regexp.MustCompile(`\bdocument\s*\.\s*write(?:ln)?\s*\(`), "document.write()"},
	}
)

// artifactFrameFindings lists where a document builds nested frames, shadow
// roots or calls document.write, with the line each one is on.
func artifactFrameFindings(content string) []artifactFrameFinding {
	// Comments are blanked (keeping line breaks, so lines still count right).
	markup := []byte(artifactHTMLCommentPattern.ReplaceAllStringFunc(content, blankKeepingLines))

	type scriptText struct {
		offset int
		text   string
	}
	var scripts []scriptText
	for pos := 0; pos < len(markup); {
		loc := artifactRawTextOpenPattern.FindSubmatchIndex(markup[pos:])
		if loc == nil {
			break
		}
		openStart, openEnd := pos+loc[0], pos+loc[1]
		name := strings.ToLower(string(markup[pos+loc[2] : pos+loc[3]]))
		bodyEnd := indexFold(markup[openEnd:], "</"+name)
		if bodyEnd < 0 {
			bodyEnd = len(markup) - openEnd
		}
		bodyEnd += openEnd
		if name == "script" && !artifactQuestionsIDPattern.Match(markup[openStart:openEnd]) {
			scripts = append(scripts, scriptText{offset: openEnd, text: string(markup[openEnd:bodyEnd])})
		}
		for i := openEnd; i < bodyEnd; i++ {
			if markup[i] != '\n' {
				markup[i] = ' '
			}
		}
		pos = bodyEnd
	}

	var findings []artifactFrameFinding
	seen := map[string]bool{}
	add := func(offset int, what string) {
		line := 1 + strings.Count(content[:offset], "\n")
		key := fmt.Sprintf("%d|%s", line, what)
		if seen[key] {
			return
		}
		seen[key] = true
		findings = append(findings, artifactFrameFinding{Line: line, What: what})
	}
	scanScript := func(offset int, text string) {
		for _, p := range artifactScriptFramePatterns {
			for _, m := range p.pattern.FindAllStringSubmatchIndex(text, -1) {
				what := p.what
				if strings.Contains(what, "%s") {
					what = fmt.Sprintf(what, strings.ToLower(text[m[2]:m[3]]))
				}
				add(offset+m[0], what)
			}
		}
	}

	text := string(markup)
	for _, m := range artifactFrameTagPattern.FindAllStringSubmatchIndex(text, -1) {
		add(m[0], "<"+strings.ToLower(text[m[2]:m[3]])+"> element")
	}
	for _, m := range artifactStartTagPattern.FindAllStringIndex(text, -1) {
		tag := text[m[0]:m[1]]
		if artifactSrcdocAttrPattern.MatchString(tag) {
			add(m[0], "srcdoc attribute")
		}
		if artifactShadowAttrPattern.MatchString(tag) {
			add(m[0], "declarative shadow root (shadowrootmode)")
		}
		for _, h := range artifactEventHandlerPattern.FindAllStringSubmatchIndex(tag, -1) {
			scanScript(m[0]+h[2], tag[h[2]:h[3]])
		}
	}
	for _, s := range scripts {
		scanScript(s.offset, s.text)
	}

	// In document order.
	for i := 1; i < len(findings); i++ {
		for j := i; j > 0 && findings[j].Line < findings[j-1].Line; j-- {
			findings[j], findings[j-1] = findings[j-1], findings[j]
		}
	}
	return findings
}

// artifactFramesError is the publish error for a document with findings: what
// was found, and how to write the page so it works in the viewer.
func artifactFramesError(findings []artifactFrameFinding) error {
	listed := findings
	if len(listed) > maxArtifactFrameFindings {
		listed = listed[:maxArtifactFrameFindings]
	}
	parts := make([]string, 0, len(listed))
	for _, f := range listed {
		parts = append(parts, fmt.Sprintf("line %d: %s", f.Line, f.What))
	}
	found := strings.Join(parts, "; ")
	if more := len(findings) - len(listed); more > 0 {
		found += fmt.Sprintf("; and %d more", more)
	}
	return validationError("artifact_nested_frames",
		"Artifact HTML was not published: it builds nested frames, shadow DOM or writes with document.write, "+
			"which the viewer blocks (content there is not shown and cannot be annotated). Found: "+found+". "+
			"Write it as one document: put every page, tab and panel in this HTML as a <section> (or <div>) with its own id, "+
			"and make tabs only show and hide those sections (the hidden attribute); do not keep pages as HTML strings in JSON or script. "+
			"Isolate a page's styles by scoping its CSS under its section id (#page-2 h1 {...}), not with a frame. "+
			"Put images in place as <img src=\"data:image/...;base64,...\">, not as placeholders filled in by script. "+
			"Never use <iframe>, srcdoc, <object>, <embed>, attachShadow or document.write.")
}

// blankKeepingLines replaces every byte but line breaks with a space, so
// offsets and line numbers in the result match the original.
func blankKeepingLines(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] != '\n' {
			b[i] = ' '
		}
	}
	return string(b)
}

// indexFold is the index of needle (lower-case ASCII) in haystack, ignoring
// ASCII case, or -1.
func indexFold(haystack []byte, needle string) int {
	n := len(needle)
	for i := 0; i+n <= len(haystack); i++ {
		j := 0
		for ; j < n; j++ {
			c := haystack[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needle[j] {
				break
			}
		}
		if j == n {
			return i
		}
	}
	return -1
}
