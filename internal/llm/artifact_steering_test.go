package llm

import (
	"strings"
	"testing"
)

// Every place an agent learns how to document steers it the same way: an HTML
// artifact for plans, reports and documentation; a Doc for short plain text.
func TestPromptsAndToolDescriptionsPreferArtifactsForDocumentation(t *testing.T) {
	for name, prompt := range map[string]string{
		"direct API prompt": ChatSystemPrompt(nil, nil, nil, nil),
		"MCP prompt":        ChatSystemPrompt(nil, nil, nil, nil, true),
	} {
		rule := ""
		for _, line := range strings.Split(prompt, "\n") {
			if strings.Contains(line, "create_artifact") && strings.Contains(line, "create_document") {
				rule = line
				break
			}
		}
		if rule == "" || strings.Index(rule, "create_artifact") > strings.Index(rule, "create_document") ||
			!strings.Contains(rule, "plan") || !strings.Contains(rule, "documentation") {
			t.Fatalf("%s: no rule that sends plans and documentation to create_artifact first: %q", name, rule)
		}
	}
	if prompt := ChatSystemPrompt(nil, nil, nil, nil); !strings.Contains(prompt, "**Artifacts**: create_artifact, update_artifact, read_artifact, list_artifacts, list_artifact_annotations, resolve_artifact_annotation") {
		t.Fatal("the direct API prompt does not list the artifact tools")
	}

	descriptions := map[string]string{}
	for _, tool := range MCPTools("session") {
		descriptions[tool.Name] = tool.Description
	}
	for _, tool := range ChatTools() {
		descriptions[tool.Name] = tool.Description
	}
	// The Docs tool points at the artifact tool, in the naming of each surface.
	if !strings.Contains(descriptions["openpoet_create_document"], "prefer openpoet_create_artifact") {
		t.Fatalf("MCP create_document does not point at the artifact tool: %s", descriptions["openpoet_create_document"])
	}
	if !strings.Contains(descriptions["create_document"], "prefer create_artifact") {
		t.Fatalf("chat create_document does not point at the artifact tool: %s", descriptions["create_document"])
	}
	for _, name := range []string{"openpoet_create_artifact", "create_artifact"} {
		for _, phrase := range []string{"Prefer this over create_document", "plans, reports, documentation", "navigation", "charts", "motion"} {
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
