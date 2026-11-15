package llm

import (
	"strings"
	"testing"
)

// Every place an agent learns how to document steers it the same way: an HTML
// artifact for plans, reports and documentation; a markdown artifact, through
// the same tool, for any other long answer. The Docs tools are retired: no
// prompt and no tool list names them.
func TestPromptsAndToolDescriptionsPreferArtifactsForDocumentation(t *testing.T) {
	for name, prompt := range map[string]string{
		"direct API prompt": ChatSystemPrompt(nil, nil, nil, nil),
		"MCP prompt":        ChatSystemPrompt(nil, nil, nil, nil, true),
	} {
		rule := ""
		for _, line := range strings.Split(prompt, "\n") {
			if strings.Contains(line, "create_artifact") && strings.Contains(line, "markdown") && strings.Contains(line, "plan") {
				rule = line
				break
			}
		}
		if rule == "" || !strings.Contains(rule, "documentation") || !strings.Contains(rule, ">5 lines") {
			t.Fatalf("%s: no rule that sends plans and documentation to an HTML artifact and long answers to a markdown one: %q", name, rule)
		}
		for _, retired := range []string{"create_document", "read_document", "a Doc"} {
			if strings.Contains(prompt, retired) {
				t.Fatalf("%s still names %q", name, retired)
			}
		}
	}
	if prompt := ChatSystemPrompt(nil, nil, nil, nil); !strings.Contains(prompt, "**Artifacts**: create_artifact, update_artifact, patch_artifact, read_artifact, list_artifacts, list_artifact_annotations, resolve_artifact_annotation") {
		t.Fatal("the direct API prompt does not list the artifact tools")
	}

	descriptions := map[string]string{}
	for _, tool := range MCPTools("session") {
		descriptions[tool.Name] = tool.Description
	}
	for _, tool := range ChatTools() {
		descriptions[tool.Name] = tool.Description
	}
	// The Docs tools are not listed on any surface, and no description names them.
	for _, retired := range []string{"openpoet_create_document", "create_document", "openpoet_read_document", "read_document"} {
		if _, listed := descriptions[retired]; listed {
			t.Fatalf("%s is still listed", retired)
		}
	}
	for name, description := range descriptions {
		if strings.Contains(description, "create_document") || strings.Contains(description, "read_document") {
			t.Fatalf("%s still names a Docs tool: %s", name, description)
		}
	}
	for _, name := range []string{"openpoet_create_artifact", "create_artifact"} {
		for _, phrase := range []string{"plans, reports, documentation", "navigation", "charts", "motion", "markdown", "more than about 5 lines"} {
			if !strings.Contains(descriptions[name], phrase) {
				t.Fatalf("%s description lacks %q: %s", name, phrase, descriptions[name])
			}
		}
	}
	// Updating an artifact starts from the user's annotations.
	for _, name := range []string{"openpoet_update_artifact", "update_artifact"} {
		if !strings.Contains(descriptions[name], "list_artifact_annotations") || !strings.Contains(descriptions[name], "resolve_artifact_annotation") {
			t.Fatalf("%s description does not describe the review loop: %s", name, descriptions[name])
		}
	}
}
