package configsync

import (
	"os"
	"strings"
)

// Every harness is steered to publish documentation into OpenPoet instead of
// harness-native artifacts or loose files, so output lands in one durable,
// user-visible place. Plans, reports and documentation go out as interactive
// HTML artifacts (openpoet_create_artifact), which the user can annotate and
// the agent revises; short plain-text answers stay as OpenPoet Docs
// (openpoet_create_document).

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
charts and restrained motion. Do not write a ` + "`.md`" + ` or ` + "`.html`" + ` file into the
project for this, and do not use harness-native artifacts. The
` + "`html-artifact`" + ` skill has the design guide.

- The user annotates artifacts. Before changing one, read the open annotations
  with ` + "`openpoet_list_artifact_annotations`" + `; publish the new version with
  ` + "`openpoet_update_artifact`" + `; then resolve each annotation you addressed with
  ` + "`openpoet_resolve_artifact_annotation`" + `.
- When the user has to decide or answer something, declare questions in the
  artifact (the ` + "`html-artifact`" + ` skill shows how). The user answers them in the
  viewer and the answers arrive with the annotations.
- When the user asks for a link to an artifact, a Doc, a session or a task, get
  it with ` + "`openpoet_get_link`" + `. Never build a URL yourself.
- For a short plain-text answer that only needs to leave the chat (a list, a
  snippet, a quick explanation), create an OpenPoet Doc with
  ` + "`openpoet_create_document`" + ` instead. Use it too when the artifact tools are
  not available in this session.
- When MCP is unavailable, call the same tools through the CLI, for example
  ` + "`$OPENPOET_BIN cli call openpoet_create_artifact '{\"title\":\"Plan\",\"html_file\":\"/tmp/plan.html\"}'`" + `
  or ` + "`$OPENPOET_BIN cli call openpoet_create_document ...`" + `.
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
