package application

import (
	"bytes"
	"html"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
)

// A markdown version of an artifact is stored as its source and rendered here,
// on the server, every time it is needed: for the page the viewer shows and for
// telling whether the passage an annotation quotes is still in a version. Both
// read the same output, so an annotation is checked against the text the reader
// actually saw.
//
// The rendering matches what the Docs viewer did (GitHub-flavored markdown with
// a line break for every newline) and keeps raw HTML written in the markdown,
// which existing documents use (<details>, <br>, <img>). Raw HTML is only
// acceptable because the result is never put in an OpenPoet page: it leaves
// through the artifact render route, into the sandboxed frame, under the same
// policy as any HTML artifact. Do not return this HTML from another endpoint.
var artifactMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(goldmarkhtml.WithHardWraps(), goldmarkhtml.WithUnsafe()),
)

// artifactMermaidCodeClass is the class the renderer gives a ```mermaid block.
const artifactMermaidCodeClass = `class="language-mermaid"`

// RenderArtifactMarkdown renders the markdown source of an artifact version as
// an HTML fragment (the content of a page body, not a document). hasMermaid
// says whether it contains a mermaid block, which the page draws with a script.
func RenderArtifactMarkdown(source string) (body string, hasMermaid bool) {
	var out bytes.Buffer
	if err := artifactMarkdown.Convert([]byte(source), &out); err != nil {
		// The source is still shown, as text.
		return "<pre>" + html.EscapeString(source) + "</pre>", false
	}
	body = out.String()
	return body, strings.Contains(body, artifactMermaidCodeClass)
}
