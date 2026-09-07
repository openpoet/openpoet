package configsync

import (
	"os"
	"strings"
)

// Phase 7.1 (Maestro): every harness is steered to publish documentation as
// OpenPoet Docs (openpoet_create_document) instead of harness-native artifacts,
// so output lands in one durable, user-visible place.

const docsSteeringBegin = "<!-- OPENPOET:BEGIN docs-steering -->"
const docsSteeringEnd = "<!-- OPENPOET:END docs-steering -->"

// OpenPoetDocsInstructionBlock is the harness-agnostic steering text. It is
// embedded verbatim in every per-backend instruction surface (copilot
// instructions, ACP instructions, the managed .claude/CLAUDE.md, and the
// codex/opencode AGENTS.md managed section).
func OpenPoetDocsInstructionBlock() string {
	return docsSteeringBegin + `
## Documentation goes to OpenPoet Docs

When you produce documentation, reports, plans, or any long-form deliverable,
create it as an OpenPoet Doc with the ` + "`openpoet_create_document`" + ` tool
(or ` + "`$OPENPOET_BIN cli call openpoet_create_document ...`" + ` when MCP is
unavailable) instead of harness-native artifacts or loose files.
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
