package handlers

import (
	_ "embed"
	"html"
	"sync"

	"openpoet/internal/application"
	"openpoet/web"
)

// A markdown version of an artifact is stored as markdown and turned into a
// page here, when it is served to the sandboxed frame: the rendered markdown
// inside a small document with its own stylesheet (both themes, phone width,
// print). The page is built on every request and never stored.

// artifactMarkdownStyle is the stylesheet of that page.
//
//go:embed artifact_markdown.css
var artifactMarkdownStyle string

// artifactMermaidScript draws the mermaid blocks of a markdown artifact. It
// runs after the mermaid library, inside the sandbox.
//
//go:embed artifact_mermaid.js
var artifactMermaidScript string

// artifactPrintScript opens the print dialog of an artifact rendered to be
// printed. It takes the place of the annotation bridge in that response.
//
//go:embed artifact_print.js
var artifactPrintScript string

const artifactMermaidLibraryPath = "static/vendor/mermaid.min.js"

var (
	artifactMermaidLibraryOnce  sync.Once
	artifactMermaidLibraryBlock string
)

// artifactMermaidLibrary returns the vendored mermaid library as an inline
// script block. The artifact frame has no network (script-src allows inline
// scripts only), so the library a diagram needs travels inside the page; it is
// only added to a document that has a mermaid block. The block is built once.
func artifactMermaidLibrary() string {
	artifactMermaidLibraryOnce.Do(func() {
		source, err := web.FS.ReadFile(artifactMermaidLibraryPath)
		if err != nil {
			return // without the library the blocks stay as code
		}
		artifactMermaidLibraryBlock = artifactScriptBlock("data-openpoet-mermaid-library", string(source))
	})
	return artifactMermaidLibraryBlock
}

// artifactMarkdownDocument returns the HTML document a markdown version is
// shown as. The markdown may carry raw HTML and scripts: like any artifact,
// the result is only ever served by RenderArtifact, under its sandbox and
// content security policy.
func artifactMarkdownDocument(title, source string, print bool) string {
	body, hasMermaid := application.RenderArtifactMarkdown(source)
	if title == "" {
		title = "Document"
	}
	root := `<html lang="en">`
	if print {
		root = `<html lang="en" data-openpoet-print>`
	}
	document := "<!doctype html>\n" + root + "\n<head>\n<meta charset=\"utf-8\">\n" +
		"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n" +
		"<title>" + html.EscapeString(title) + "</title>\n" +
		"<style data-openpoet-markdown>\n" + artifactMarkdownStyle + "</style>\n</head>\n<body>\n" +
		"<main id=\"openpoet-markdown\">\n" + body + "</main>\n"
	if hasMermaid {
		if library := artifactMermaidLibrary(); library != "" {
			document += library + "\n" + artifactScriptBlock("data-openpoet-mermaid", artifactMermaidScript) + "\n"
		}
	}
	return document + "</body>\n</html>\n"
}
