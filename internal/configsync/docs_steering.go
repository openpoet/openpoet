package configsync

import (
	"os"
	"strings"
)

// Every harness is steered to publish documentation into OpenPoet instead of
// harness-native artifacts or loose files, so output lands in one durable,
// user-visible place. Plans, reports and documentation go out as interactive
// HTML artifacts (openpoet_create_artifact), which the user can annotate and
// the agent revises; a short plain-text answer goes out through the same tool,
// as a markdown artifact. Artifacts are the one format of documents: the Docs
// tools are retired.

const docsSteeringBegin = "<!-- OPENPOET:BEGIN docs-steering -->"
const docsSteeringEnd = "<!-- OPENPOET:END docs-steering -->"

// OpenPoetDocsInstructionBlock is the harness-agnostic steering text. It is
// embedded verbatim in every per-backend instruction surface (copilot
// instructions, ACP instructions, the managed .claude/CLAUDE.md, and the
// codex/opencode AGENTS.md managed section). The markers stay the same across
// releases, so a sync replaces the previous wording in place.
func OpenPoetDocsInstructionBlock() string {
	return docsSteeringBegin + `
## Plans and documentation go to OpenPoet, as interactive HTML artifacts

When you write a plan, a report, documentation or any deliverable the user will
read or review, and whenever the user asks you to document something, publish it
as an HTML artifact with the ` + "`openpoet_create_artifact`" + ` tool: one self-contained
HTML page (inline CSS and JavaScript, no external resources) built like a small
site, with navigation between sections, visual components, inline SVG or canvas
charts and restrained motion. Do not leave a ` + "`.md`" + ` or ` + "`.html`" + ` file in the
project for this, and do not use harness-native artifacts. The
` + "`html-artifact`" + ` skill has the design guide.

- Do not type the page into the tool call: write it to an ` + "`.html`" + ` file in a
  temporary or scratch directory and pass its path as ` + "`html_path`" + `. Change a
  published page with ` + "`openpoet_patch_artifact`" + ` (only the replacements are
  sent), and get its HTML back into a file with ` + "`openpoet_read_artifact`" + ` and
  ` + "`output_path`" + `.
- The user annotates artifacts. Before changing one, read the open annotations
  with ` + "`openpoet_list_artifact_annotations`" + `; publish the new version with
  ` + "`openpoet_patch_artifact`" + ` or ` + "`openpoet_update_artifact`" + `; then resolve the
  ones you addressed, in one call, with ` + "`openpoet_resolve_artifact_annotation`" + `.
- When the user has to decide or answer something, declare questions in the
  artifact (the ` + "`html-artifact`" + ` skill shows how). The user answers them in the
  viewer and the answers arrive with the annotations.
- When the user asks for a link to an artifact, a session or a task, get it
  with ` + "`openpoet_get_link`" + `. Never build a URL yourself.
- For a short plain-text answer that only needs to leave the chat (a list, a
  snippet, a quick explanation), publish a markdown artifact instead: the same
  ` + "`openpoet_create_artifact`" + ` tool, with the text in ` + "`markdown`" + ` (or a ` + "`.md`" + `
  file in ` + "`markdown_path`" + `) in place of ` + "`html_path`" + `. OpenPoet renders it,
  with ` + "```mermaid" + ` blocks drawn as diagrams. There is no separate tool for
  documents: ` + "`openpoet_read_artifact`" + ` reads every document, and the ids a
  task or a session lists.
- When MCP is unavailable, call the same tools through the CLI, for example
  ` + "`$OPENPOET_BIN cli call openpoet_create_artifact '{\"title\":\"Plan\",\"html_file\":\"/tmp/plan.html\"}'`" + `
  or, for markdown, ` + "`... '{\"title\":\"Notes\",\"markdown_file\":\"/tmp/notes.md\"}'`" + `.
` + docsSteeringEnd + "\n"
}

// upsertDocsSteeringFile creates or updates the managed steering section in the
// file at path. Files fully owned by OpenPoet are simply created; files the user
// also edits keep their content — only the marker-delimited section is replaced
// (or appended when absent). Idempotent: syncing twice never duplicates.
func upsertDocsSteeringFile(path string) error {
	block := OpenPoetDocsInstructionBlock()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return os.WriteFile(path, []byte(block), 0644)
		}
		return err
	}
	content := string(data)
	updated := upsertDocsSteeringContent(content, block)
	if updated == content {
		return nil
	}
	return os.WriteFile(path, []byte(updated), 0644)
}

// upsertDocsSteeringContent replaces the existing managed section or appends
// one, returning the updated content.
func upsertDocsSteeringContent(content, block string) string {
	begin := strings.Index(content, docsSteeringBegin)
	end := strings.Index(content, docsSteeringEnd)
	if begin >= 0 && end > begin {
		return content[:begin] + strings.TrimSuffix(block, "\n") + content[end+len(docsSteeringEnd):]
	}
	if !strings.HasSuffix(content, "\n") && content != "" {
		content += "\n"
	}
	return content + "\n" + block
}
