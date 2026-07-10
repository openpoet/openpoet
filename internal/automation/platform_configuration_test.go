package automation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"openpoet/internal/application"
	"openpoet/internal/database"
)

func configurationPlatformTestServices() ConfigurationPlatformServices {
	return ConfigurationPlatformServices{
		Projects:          application.NewProjectService(nil, nil, nil),
		ProjectOperations: application.NewProjectOperationService(nil, nil, nil),
		Tags:              application.NewTagService(nil),
		Skills:            application.NewSkillService(nil, nil),
		Agents:            application.NewAIAgentService(nil, nil),
		AIConfigs:         application.NewAIConfigService(nil, nil, nil, nil),
		MCP:               application.NewMCPService(nil, nil, nil),
		CustomTools:       application.NewCustomToolService(nil, nil, nil),
		Configuration:     application.NewConfigurationService(nil, nil, nil, nil, nil),
	}
}

func configurationPlatformTestRegistry(t *testing.T) (*application.CapabilityRegistry, *PlatformCapabilityRegistry) {
	t.Helper()
	capabilities := application.NewCapabilityRegistry()
	registry, err := NewPlatformCapabilityRegistry(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if err = RegisterConfigurationPlatformCapabilities(registry, configurationPlatformTestServices()); err != nil {
		t.Fatal(err)
	}
	return capabilities, registry
}

func configurationPlatformDefinitionsForTest() []PlatformCapabilityDefinition {
	groups := [][]PlatformCapabilityDefinition{
		projectPlatformDefinitions(), projectOperationPlatformDefinitions(), tagPlatformDefinitions(),
		skillPlatformDefinitions(), agentPlatformDefinitions(), aiConfigPlatformDefinitions(),
		mcpPlatformDefinitions(), customToolPlatformDefinitions(), configurationPlatformDefinitions(),
	}
	var result []PlatformCapabilityDefinition
	for _, group := range groups {
		result = append(result, group...)
	}
	return result
}

func configurationPlatformActor(definitions []PlatformCapabilityDefinition) Actor {
	scopes := ScopeSet{}
	for _, definition := range definitions {
		for _, scope := range definition.Scopes {
			scopes[Scope(scope)] = struct{}{}
		}
	}
	return Actor{Type: "automation_client", ID: "helena", ClientID: "helena", Name: "Helena", Scopes: scopes}
}

func TestConfigurationPlatformRegistersCompleteUniqueSurface(t *testing.T) {
	definitions := configurationPlatformDefinitionsForTest()
	if len(definitions) != 63 {
		t.Fatalf("configuration surface has %d capabilities, want 63", len(definitions))
	}

	seen := make(map[application.CapabilityName]struct{}, len(definitions))
	for _, definition := range definitions {
		if _, exists := seen[definition.Name]; exists {
			t.Fatalf("duplicate configuration capability %q", definition.Name)
		}
		seen[definition.Name] = struct{}{}
	}
	capabilities, registry := configurationPlatformTestRegistry(t)
	if got := len(capabilities.List()); got != len(definitions) {
		t.Fatalf("application registry has %d configuration capabilities, want %d", got, len(definitions))
	}
	if got := len(registry.ListForActor(configurationPlatformActor(definitions))); got != len(definitions) {
		t.Fatalf("platform discovery has %d configuration capabilities, want %d", got, len(definitions))
	}
}

func TestConfigurationPlatformRegistrationRequiresEveryServiceAtomically(t *testing.T) {
	capabilities := application.NewCapabilityRegistry()
	registry, err := NewPlatformCapabilityRegistry(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	services := configurationPlatformTestServices()
	services.CustomTools = nil
	if err = RegisterConfigurationPlatformCapabilities(registry, services); err == nil {
		t.Fatal("registration accepted a missing configuration service")
	}
	if got := len(capabilities.List()); got != 0 {
		t.Fatalf("failed registration partially mutated application registry with %d capabilities", got)
	}
}

type configurationManifest struct {
	Routes []struct {
		Capability         application.CapabilityName `json:"capability"`
		Risk               string                     `json:"risk"`
		Scopes             []string                   `json:"scopes"`
		ApplicationService string                     `json:"application_service"`
	} `json:"routes"`
}

func TestConfigurationPlatformMutationMetadataMatchesManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "automation", "ui-action-manifest.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest configurationManifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	definitions := make(map[application.CapabilityName]PlatformCapabilityDefinition)
	for _, definition := range configurationPlatformDefinitionsForTest() {
		definitions[definition.Name] = definition
	}
	wantedServices := map[string]struct{}{
		"ProjectService": {}, "ProjectOperationService": {}, "TagService": {}, "SkillService": {},
		"AIAgentService": {}, "AIConfigService": {}, "MCPService": {}, "CustomToolService": {},
		"ConfigurationService": {},
	}
	riskMetadata := map[string]struct {
		risk     application.CapabilityRisk
		approval application.ApprovalPolicy
	}{
		"R1": {application.CapabilityRiskRead, application.ApprovalNone},
		"R2": {application.CapabilityRiskWrite, application.ApprovalByPolicy},
		"R3": {application.CapabilityRiskDestructive, application.ApprovalExplicit},
		"R4": {application.CapabilityRiskUnsafe, application.ApprovalExplicit},
	}

	checked := 0
	for _, route := range manifest.Routes {
		if _, wanted := wantedServices[route.ApplicationService]; !wanted {
			continue
		}
		definition, exists := definitions[route.Capability]
		if !exists {
			t.Errorf("manifest mutation %q has no typed configuration adapter", route.Capability)
			continue
		}
		expected, validRisk := riskMetadata[route.Risk]
		if !validRisk {
			t.Errorf("manifest mutation %q has unknown risk %q", route.Capability, route.Risk)
			continue
		}
		gotScopes := make([]string, len(definition.Scopes))
		for i, scope := range definition.Scopes {
			gotScopes[i] = string(scope)
		}
		if !reflect.DeepEqual(gotScopes, route.Scopes) || definition.Risk != expected.risk || definition.Approval != expected.approval {
			t.Errorf("metadata mismatch for %q: scopes=%v risk=%s approval=%s; want scopes=%v risk=%s approval=%s",
				route.Capability, gotScopes, definition.Risk, definition.Approval, route.Scopes, expected.risk, expected.approval)
		}
		if !definition.Mutation {
			t.Errorf("manifest mutation %q is not marked mutation", route.Capability)
		}
		if definition.Handler != application.CapabilityHandler(route.Capability) {
			t.Errorf("handler mismatch for %q: %q", route.Capability, definition.Handler)
		}
		checked++
	}
	if checked != 44 {
		t.Fatalf("checked %d manifest mutations, want 44", checked)
	}
}

func TestConfigurationPlatformReadSurfaceIsExplicit(t *testing.T) {
	want := []string{
		"agents.list", "ai_configs.list", "ai_configs.list_assignments", "mcp.list", "mcp.list_project",
		"mcp_api_key.status", "projects.get", "projects.get_shares", "projects.list", "settings.get",
		"skills.list", "skills.list_project", "skills.list_project_config", "skills.list_versions", "tags.list",
		"tags.list_project", "tools.get_policies", "tools.get_project_policy", "tools.list_project",
	}
	var got []string
	for _, definition := range configurationPlatformDefinitionsForTest() {
		if definition.Risk == application.CapabilityRiskRead && definition.Approval == application.ApprovalNone && !definition.Mutation {
			got = append(got, string(definition.Name))
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("read surface mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestConfigurationPlatformDiscoveryRequiresAllScopes(t *testing.T) {
	_, registry := configurationPlatformTestRegistry(t)
	actor := Actor{Type: "automation_client", ID: "helena", Scopes: ScopeSet{
		"mcp:write": {}, "projects:write": {},
	}}
	find := func() PlatformCapabilityDescriptor {
		t.Helper()
		for _, descriptor := range registry.ListForActor(actor) {
			if descriptor.Name == "mcp.create_project" {
				return descriptor
			}
		}
		t.Fatal("mcp.create_project not discovered")
		return PlatformCapabilityDescriptor{}
	}
	descriptor := find()
	if descriptor.Allowed || !reflect.DeepEqual(descriptor.Scopes, []application.CapabilityScope{"mcp:write", "projects:write", "credentials:write"}) {
		t.Fatalf("incomplete scope set was allowed: %#v", descriptor)
	}
	actor.Scopes["credentials:write"] = struct{}{}
	if descriptor = find(); !descriptor.Allowed {
		t.Fatalf("complete scope set was rejected: %#v", descriptor)
	}
}

type configurationDryRunCase struct {
	name       string
	target     string
	payload    string
	secretText []string
}

func configurationDryRunCases() []configurationDryRunCase {
	return []configurationDryRunCase{
		{name: "projects.list", target: `{}`, payload: `{}`},
		{name: "projects.get", target: `{"id":1}`, payload: `{}`},
		{name: "projects.create", target: `{}`, payload: `{"name":"demo","path":"/tmp/demo","type":"local","ssh_credential":"project-secret"}`, secretText: []string{"project-secret"}},
		{name: "projects.update", target: `{"id":1}`, payload: `{"name":"demo","path":"/tmp/demo","type":"local","ssh_credential":"project-secret"}`, secretText: []string{"project-secret"}},
		{name: "projects.delete", target: `{"id":1}`, payload: `{}`},
		{name: "projects.duplicate", target: `{"id":1}`, payload: `{"name":"copy","path":"/tmp/copy","type":"local","ssh_credential":"project-secret"}`, secretText: []string{"project-secret"}},
		{name: "projects.validate", target: `{"id":1}`, payload: `{}`},
		{name: "projects.browse_remote", target: `{}`, payload: `{"ssh_host":"host","ssh_user":"user","ssh_credential":"browse-secret","path":"/srv"}`, secretText: []string{"browse-secret"}},
		{name: "tags.list", target: `{}`, payload: `{}`},
		{name: "tags.list_project", target: `{"project_id":1}`, payload: `{}`},
		{name: "tags.create", target: `{}`, payload: `{"name":"urgent","color":"red"}`},
		{name: "tags.update", target: `{"id":1}`, payload: `{"name":"important"}`},
		{name: "tags.delete", target: `{"id":1}`, payload: `{}`},
		{name: "tags.assign_project", target: `{"project_id":1}`, payload: `{"tag_ids":[1,2]}`},
		{name: "skills.list", target: `{}`, payload: `{}`},
		{name: "skills.list_versions", target: `{"id":1}`, payload: `{}`},
		{name: "skills.list_project_config", target: `{"project_id":1}`, payload: `{}`},
		{name: "skills.list_project", target: `{"project_id":1}`, payload: `{}`},
		{name: "skills.create", target: `{}`, payload: `{"name":"review","content":"instructions","enabled":true}`},
		{name: "skills.import", target: `{}`, payload: `{"skills":[{"name":"review","content":"instructions","enabled":true}]}`},
		{name: "skills.update", target: `{"id":1}`, payload: `{"name":"review-2"}`},
		{name: "skills.delete", target: `{"id":1}`, payload: `{}`},
		{name: "skills.duplicate", target: `{"id":1}`, payload: `{"name":"review-copy"}`},
		{name: "skills.restore_version", target: `{"id":1}`, payload: `{"version_id":2}`},
		{name: "skills.update_project_config", target: `{"project_id":1}`, payload: `{"configs":{"1":true}}`},
		{name: "skills.create_project", target: `{"project_id":1}`, payload: `{"name":"local","content":"instructions","enabled":true}`},
		{name: "skills.update_project", target: `{"id":2,"project_id":1}`, payload: `{"name":"local-2"}`},
		{name: "skills.delete_project", target: `{"id":2,"project_id":1}`, payload: `{}`},
		{name: "agents.list", target: `{}`, payload: `{}`},
		{name: "agents.create", target: `{}`, payload: `{"name":"assistant","system_prompt":"help","enabled":true}`},
		{name: "agents.update", target: `{"id":1}`, payload: `{"system_prompt":"help more"}`},
		{name: "agents.delete", target: `{"id":1}`, payload: `{}`},
		{name: "ai_configs.list", target: `{}`, payload: `{}`},
		{name: "ai_configs.list_assignments", target: `{}`, payload: `{}`},
		{name: "ai_configs.create", target: `{}`, payload: `{"name":"primary","provider_type":"openai","api_key":"ai-secret"}`, secretText: []string{"ai-secret"}},
		{name: "ai_configs.update", target: `{"id":1}`, payload: `{"api_key":"ai-secret"}`, secretText: []string{"ai-secret"}},
		{name: "ai_configs.delete", target: `{"id":1}`, payload: `{}`},
		{name: "ai_configs.assign", target: `{}`, payload: `{"assignments":{"ai_chat":1}}`},
		{name: "mcp.list", target: `{}`, payload: `{}`},
		{name: "mcp.list_project", target: `{"project_id":1}`, payload: `{}`},
		{name: "mcp.create", target: `{}`, payload: `{"name":"server","command":"bun","args":"[]","env":"{\"TOKEN\":\"mcp-secret\"}","enabled":true}`, secretText: []string{"mcp-secret"}},
		{name: "mcp.update", target: `{"id":1}`, payload: `{"env":"{\"TOKEN\":\"mcp-secret\"}"}`, secretText: []string{"mcp-secret"}},
		{name: "mcp.delete", target: `{"id":1}`, payload: `{}`},
		{name: "mcp.create_project", target: `{"project_id":1}`, payload: `{"name":"server","command":"bun","args":"[]","env":"{\"TOKEN\":\"mcp-secret\"}","enabled":true}`, secretText: []string{"mcp-secret"}},
		{name: "mcp.update_project", target: `{"id":2,"project_id":1}`, payload: `{"env":"{\"TOKEN\":\"mcp-secret\"}"}`, secretText: []string{"mcp-secret"}},
		{name: "mcp.delete_project", target: `{"id":2,"project_id":1}`, payload: `{}`},
		{name: "tools.list_project", target: `{"project_id":1}`, payload: `{}`},
		{name: "tools.create_project", target: `{"project_id":1}`, payload: `{"name":"build","command":"run tool-secret","parameters":"{}","enabled":true}`, secretText: []string{"tool-secret"}},
		{name: "tools.update_project", target: `{"id":2,"project_id":1}`, payload: `{"command":"run tool-secret"}`, secretText: []string{"tool-secret"}},
		{name: "tools.delete_project", target: `{"id":2,"project_id":1}`, payload: `{}`},
		{name: "settings.get", target: `{}`, payload: `{}`},
		{name: "tools.get_policies", target: `{}`, payload: `{}`},
		{name: "tools.get_project_policy", target: `{"project_id":1}`, payload: `{}`},
		{name: "projects.get_shares", target: `{"project_id":1}`, payload: `{}`},
		{name: "mcp_api_key.status", target: `{}`, payload: `{}`},
		{name: "settings.update", target: `{}`, payload: `{"settings":{"openai_api_key":"settings-secret"}}`, secretText: []string{"settings-secret"}},
		{name: "projects.sync_config", target: `{"project_id":1}`, payload: `{}`},
		{name: "config.sync_all", target: `{}`, payload: `{}`},
		{name: "mcp_api_key.generate", target: `{}`, payload: `{}`},
		{name: "mcp_api_key.revoke", target: `{}`, payload: `{}`},
		{name: "tools.update_policies", target: `{}`, payload: `{"policies":{"chat":"{\"mode\":\"allow\"}"}}`},
		{name: "tools.update_project_policy", target: `{"project_id":1}`, payload: `{"policy":"{\"mode\":\"allow\"}"}`},
		{name: "projects.update_shares", target: `{"project_id":1}`, payload: `{"shared_project_ids":[2,3]}`},
	}
}

func TestConfigurationPlatformEveryCapabilitySupportsSideEffectFreeDryRun(t *testing.T) {
	definitions := configurationPlatformDefinitionsForTest()
	_, registry := configurationPlatformTestRegistry(t)
	actor := configurationPlatformActor(definitions)
	cases := configurationDryRunCases()
	if len(cases) != len(definitions) {
		t.Fatalf("dry-run table has %d capabilities, want %d", len(cases), len(definitions))
	}
	seen := make(map[string]struct{}, len(cases))
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, duplicate := seen[testCase.name]; duplicate {
				t.Fatalf("duplicate dry-run case %q", testCase.name)
			}
			seen[testCase.name] = struct{}{}
			result, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
				Capability: application.CapabilityName(testCase.name), Actor: actor, DryRun: true,
				Target: json.RawMessage(testCase.target), Payload: json.RawMessage(testCase.payload),
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "dry_run" {
				t.Fatalf("status=%q, want dry_run", result.Status)
			}
			encoded, err := json.Marshal(result.Result)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range testCase.secretText {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("dry-run preview leaked secret %q: %s", secret, encoded)
				}
			}
		})
	}
	for _, definition := range definitions {
		if _, exists := seen[string(definition.Name)]; !exists {
			t.Errorf("capability %q has no dry-run contract test", definition.Name)
		}
	}
}

func TestConfigurationPlatformR3AndR4StopBeforeDomainExecutionWithoutApproval(t *testing.T) {
	definitions := configurationPlatformDefinitionsForTest()
	_, registry := configurationPlatformTestRegistry(t)
	actor := configurationPlatformActor(definitions)
	cases := make(map[string]configurationDryRunCase)
	for _, testCase := range configurationDryRunCases() {
		cases[testCase.name] = testCase
	}
	checked := 0
	for _, definition := range definitions {
		if definition.Risk != application.CapabilityRiskDestructive && definition.Risk != application.CapabilityRiskUnsafe {
			continue
		}
		testCase, exists := cases[string(definition.Name)]
		if !exists {
			t.Fatalf("risk %s capability %q has no approval-boundary case", definition.Risk, definition.Name)
		}
		t.Run(testCase.name, func(t *testing.T) {
			_, err := DispatchPlatformCapability(context.Background(), registry, PlatformDispatchRequest{
				Capability: application.CapabilityName(testCase.name), Actor: actor, Reason: "approved action",
				Target: json.RawMessage(testCase.target), Payload: json.RawMessage(testCase.payload),
			})
			var dispatchErr *PlatformDispatchError
			if err == nil || !errors.As(err, &dispatchErr) || dispatchErr.Code != "platform_approval_required" {
				t.Fatalf("unapproved %s did not stop at approval boundary: %T %v", testCase.name, err, err)
			}
		})
		checked++
	}
	if checked == 0 {
		t.Fatal("no R3/R4 configuration capabilities were checked")
	}
}

func TestConfigurationAutomationViewsNeverSerializeStoredSecrets(t *testing.T) {
	projectSecret := "ciphertext-project-marker"
	backendSecret := "backend-config-marker"
	project := projectAutomationView(database.Project{
		ID: 1, Name: "demo", Path: "/tmp/demo", Type: "remote",
		SSHCredentialEncrypted: sql.NullString{String: projectSecret, Valid: true},
		SSHCredentialIV:        sql.NullString{String: "project-iv-marker", Valid: true},
		BackendConfig:          `{"token":"` + backendSecret + `"}`,
	})
	aiSecret := "ciphertext-ai-marker"
	ai := aiConfigAutomationView(database.AIConfig{
		ID: 2, Name: "primary", ProviderType: "openai", APIKeyEncrypted: aiSecret,
		APIKeyIV: "ai-iv-marker", APIKeyPreview: "safe...",
	})
	encoded, err := json.Marshal(struct {
		Project ProjectAutomationView  `json:"project"`
		AI      AIConfigAutomationView `json:"ai"`
	}{Project: project, AI: ai})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{projectSecret, "project-iv-marker", backendSecret, aiSecret, "ai-iv-marker", `"backend_config":`, `"api_key_encrypted":`, `"api_key_iv":`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("automation view serialized forbidden secret material %q: %s", forbidden, encoded)
		}
	}
	if !project.HasCredential || !project.HasBackendConfig || !ai.HasAPIKey {
		t.Fatalf("redacted metadata was lost: project=%#v ai=%#v", project, ai)
	}
}
