package application

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type uiActionManifest struct {
	SchemaVersion            int                     `json:"schema_version"`
	Routes                   []uiActionRoute         `json:"routes"`
	BusinessUIActions        []string                `json:"business_ui_actions"`
	PresentationOnly         []string                `json:"presentation_only"`
	BusinessEventActions     []string                `json:"business_event_actions"`
	PresentationEventActions []string                `json:"presentation_event_actions"`
	Allowlist                []uiActionAllowlistItem `json:"allowlist"`
}

type uiActionRoute struct {
	Route              string   `json:"route"`
	Method             string   `json:"method"`
	Capability         string   `json:"capability"`
	Domain             string   `json:"domain"`
	Risk               string   `json:"risk"`
	Scopes             []string `json:"scopes"`
	ApplicationService string   `json:"application_service"`
	UIBinding          string   `json:"ui_binding"`
	Status             string   `json:"status"`
}

type uiActionAllowlistItem struct {
	Route  string `json:"route"`
	Method string `json:"method"`
	Reason string `json:"reason"`
}

func TestUIActionManifestCoversBackendAndFrontendMutations(t *testing.T) {
	root := repositoryRoot(t)
	manifest := loadUIActionManifest(t, root)
	routes, allowlist := validateUIActionManifest(t, manifest)

	backend := extractBackendMutationRoutes(t, root)
	for key := range backend {
		if _, covered := routes[key]; covered {
			continue
		}
		if _, allowed := allowlist[key]; allowed {
			continue
		}
		t.Errorf("backend mutation missing from manifest: %s", key)
	}
	for key := range routes {
		if _, exists := backend[key]; !exists {
			t.Errorf("manifest route no longer exists in backend: %s", key)
		}
	}

	frontend := extractFrontendMutationRoutes(t, root)
	for key, locations := range frontend {
		if routeSetContainsShape(routes, key) || routeSetContainsShape(allowlist, key) {
			continue
		}
		t.Errorf("frontend mutation missing from manifest: %s (%s)", key, strings.Join(locations, ", "))
	}

	uiActions := extractInlineUIActions(t, root)
	coveredActions := make(map[string]struct{}, len(manifest.BusinessUIActions)+len(manifest.PresentationOnly))
	for _, action := range manifest.BusinessUIActions {
		coveredActions[action] = struct{}{}
	}
	for _, action := range manifest.PresentationOnly {
		if _, duplicate := coveredActions[action]; duplicate {
			t.Errorf("UI action classified twice: %s", action)
		}
		coveredActions[action] = struct{}{}
	}
	for action, locations := range uiActions {
		if _, ok := coveredActions[action]; !ok {
			t.Errorf("inline UI action missing from business_ui_actions/presentation_only: %s (%s)", action, strings.Join(locations, ", "))
		}
	}

	eventActions := extractStaticEventActions(t, root)
	coveredEvents := make(map[string]struct{}, len(manifest.BusinessEventActions)+len(manifest.PresentationEventActions))
	for _, action := range manifest.BusinessEventActions {
		coveredEvents[action] = struct{}{}
	}
	for _, action := range manifest.PresentationEventActions {
		if _, duplicate := coveredEvents[action]; duplicate {
			t.Errorf("event action classified twice: %s", action)
		}
		coveredEvents[action] = struct{}{}
	}
	for action, locations := range eventActions {
		if _, ok := coveredEvents[action]; !ok {
			t.Errorf("static click/submit listener missing from manifest: %s (%s)", action, strings.Join(locations, ", "))
		}
	}
}

func validateUIActionManifest(t *testing.T, manifest uiActionManifest) (map[string]struct{}, map[string]struct{}) {
	t.Helper()
	if manifest.SchemaVersion != 1 {
		t.Fatalf("schema_version=%d, want 1", manifest.SchemaVersion)
	}
	routes := make(map[string]struct{}, len(manifest.Routes))
	implemented, internal := 0, 0
	for _, item := range manifest.Routes {
		key := routeKey(item.Method, item.Route)
		if _, duplicate := routes[key]; duplicate {
			t.Errorf("duplicate manifest route: %s", key)
		}
		routes[key] = struct{}{}
		if item.Capability == "" || item.Domain == "" || item.ApplicationService == "" || len(item.Scopes) == 0 {
			t.Errorf("incomplete manifest metadata: %s", key)
		}
		if !regexp.MustCompile(`^R[0-4]$`).MatchString(item.Risk) {
			t.Errorf("invalid risk %q: %s", item.Risk, key)
		}
		switch item.Status {
		case "implemented":
			implemented++
			if item.UIBinding != "shared_application_service" || item.ApplicationService == "internal_only" {
				t.Errorf("implemented route lacks a shared service binding: %s", key)
			}
		case "internal_only":
			internal++
			if item.UIBinding != "internal_transport" || item.ApplicationService != "internal_only" {
				t.Errorf("internal route has an invalid binding: %s", key)
			}
		default:
			t.Errorf("route is not fully converged (status=%q): %s", item.Status, key)
		}
		if item.Method == "DELETE" && item.Risk != "R3" && item.Risk != "R4" {
			t.Errorf("DELETE must be R3/R4: %s", key)
		}
		if (item.Domain == "tunnel" || item.Domain == "update" || contains(item.Scopes, "credentials:write")) && item.Risk != "R4" {
			t.Errorf("credential/tunnel/update mutation must be R4: %s", key)
		}
	}
	if implemented != 122 || internal != 2 {
		t.Errorf("manifest convergence = %d implemented/%d internal, want 122/2", implemented, internal)
	}
	allowlist := make(map[string]struct{}, len(manifest.Allowlist))
	for _, item := range manifest.Allowlist {
		if item.Reason == "" {
			t.Errorf("allowlist reason missing: %s %s", item.Method, item.Route)
		}
		allowlist[routeKey(item.Method, item.Route)] = struct{}{}
	}
	return routes, allowlist
}

func extractBackendMutationRoutes(t *testing.T, root string) map[string]struct{} {
	t.Helper()
	source := readText(t, filepath.Join(root, "cmd/openpoet/main.go"))
	apiStart := strings.Index(source, `r.Route("/api"`)
	apiEnd := strings.Index(source[apiStart:], "// Test-only endpoints")
	if apiStart < 0 || apiEnd < 0 {
		t.Fatal("could not isolate /api route block")
	}
	apiEnd += apiStart
	re := regexp.MustCompile(`r\.(Post|Put|Patch|Delete)\("([^"]+)"`)
	result := make(map[string]struct{})
	for _, match := range re.FindAllStringSubmatchIndex(source, -1) {
		method := strings.ToUpper(source[match[2]:match[3]])
		route := source[match[4]:match[5]]
		if match[0] >= apiStart && match[0] < apiEnd {
			route = "/api" + route
		}
		result[routeKey(method, route)] = struct{}{}
	}
	return result
}

func extractFrontendMutationRoutes(t *testing.T, root string) map[string][]string {
	t.Helper()
	result := make(map[string][]string)
	files, err := filepath.Glob(filepath.Join(root, "web/static/js/*.js"))
	if err != nil {
		t.Fatal(err)
	}
	apiCall := regexp.MustCompile(`this\.api\(\s*['"](POST|PUT|PATCH|DELETE)['"]\s*,\s*([` + "`" + `'\"])([^` + "`" + `'\"]+)`)
	fetchCall := regexp.MustCompile(`fetch\(\s*([` + "`" + `'\"])(/api[^` + "`" + `'\"]+)[` + "`" + `'\"]\s*,\s*\{(?s:.{0,500}?)method:\s*['"](POST|PUT|PATCH|DELETE)['"]`)
	beacon := regexp.MustCompile(`sendBeacon\?\.\(\s*['"]([^'"]+)['"]`)
	for _, file := range files {
		source := readText(t, file)
		for _, match := range apiCall.FindAllStringSubmatch(source, -1) {
			addFrontendRoute(result, match[1], "/api"+match[3], filepath.Base(file))
		}
		for _, match := range fetchCall.FindAllStringSubmatch(source, -1) {
			addFrontendRoute(result, match[3], match[2], filepath.Base(file))
		}
		for _, match := range beacon.FindAllStringSubmatch(source, -1) {
			addFrontendRoute(result, "POST", match[1], filepath.Base(file))
		}
	}
	return result
}

func extractInlineUIActions(t *testing.T, root string) map[string][]string {
	t.Helper()
	result := make(map[string][]string)
	files, err := filepath.Glob(filepath.Join(root, "web/static/js/*.js"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`onclick=["'][^"']*?(app|window\.openPoetTheme)\.([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)
	for _, file := range files {
		source := readText(t, file)
		for _, match := range re.FindAllStringSubmatch(source, -1) {
			prefix := "app"
			if match[1] == "window.openPoetTheme" {
				prefix = "theme"
			}
			action := prefix + "." + match[2]
			result[action] = appendUnique(result[action], filepath.Base(file))
		}
	}
	return result
}

func extractStaticEventActions(t *testing.T, root string) map[string][]string {
	t.Helper()
	result := make(map[string][]string)
	files, err := filepath.Glob(filepath.Join(root, "web/static/js/*.js"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?:getElementById\(["']([^"']+)["']\)|querySelector\(["']#([^"']+)["']\))\?*\.addEventListener\(["'](?:click|submit)["']`)
	for _, file := range files {
		source := readText(t, file)
		for _, match := range re.FindAllStringSubmatch(source, -1) {
			id := match[1]
			if id == "" {
				id = match[2]
			}
			action := "element#" + id
			result[action] = appendUnique(result[action], filepath.Base(file))
		}
	}
	return result
}

func addFrontendRoute(result map[string][]string, method, route, file string) {
	route = regexp.MustCompile(`\$\{[^}]+\}`).ReplaceAllString(route, "{param}")
	if query := strings.IndexByte(route, '?'); query >= 0 {
		route = route[:query]
	}
	key := routeKey(method, route)
	result[key] = appendUnique(result[key], file)
}

func routeSetContainsShape[T any](set map[string]T, key string) bool {
	shape := routeShape(key)
	for candidate := range set {
		if routeShape(candidate) == shape {
			return true
		}
	}
	return false
}

func routeShape(key string) string {
	return regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(key, "{}")
}

func routeKey(method, route string) string {
	return strings.ToUpper(strings.TrimSpace(method)) + " " + strings.TrimSpace(route)
}

func appendUnique(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func loadUIActionManifest(t *testing.T, root string) uiActionManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "docs/automation/ui-action-manifest.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest uiActionManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
