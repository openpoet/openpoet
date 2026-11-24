package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// devboxToolNames are the tools that publish ports of the session's devbox.
var devboxToolNames = map[string]bool{
	"openpoet_devbox_publish_port":   true,
	"openpoet_devbox_list_ports":     true,
	"openpoet_devbox_unpublish_port": true,
}

type devboxPortView struct {
	App           string `json:"app"`
	URL           string `json:"url"`
	ContainerPort int    `json:"container_port"`
	Mode          string `json:"mode"`
	Container     string `json:"container"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
}

// executeDevboxTool runs the devbox port tools. The server derives the
// container from the session token the client carries; no argument names it.
func executeDevboxTool(client *APIClient, name string, params map[string]interface{}) (string, error) {
	switch name {
	case "openpoet_devbox_publish_port":
		app := strings.TrimSpace(stringParam(params, "app"))
		if app == "" {
			return "", fmt.Errorf("app is required")
		}
		port := toolNumber(params["port"])
		if port <= 0 {
			return "", fmt.Errorf("port is required (the port the app listens on inside the container)")
		}
		payload, _ := json.Marshal(map[string]any{"app": app, "port": port, "mode": stringParam(params, "mode")})
		body, err := client.Post("/api/devbox/ports", string(payload))
		if err != nil {
			return "", artifactAPIError(err)
		}
		var v devboxPortView
		if err := json.Unmarshal(body, &v); err != nil {
			return string(body), nil
		}
		what := map[string]string{
			"created":   "Published",
			"unchanged": "Already published",
			"repaired":  "Already published (its relay or route was missing and was recreated)",
		}[v.Status]
		if what == "" {
			what = "Published"
		}
		return fmt.Sprintf("%s: %s\n-> container %s, port %d (mode %s). Reachable only from the private network (VPN). "+
			"Remove it with devbox_unpublish_port when it is no longer needed.", what, v.URL, v.Container, v.ContainerPort, v.Mode), nil

	case "openpoet_devbox_list_ports":
		body, err := client.Get("/api/devbox/ports")
		if err != nil {
			return "", artifactAPIError(err)
		}
		var resp struct {
			Ports []devboxPortView `json:"ports"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return string(body), nil
		}
		if len(resp.Ports) == 0 {
			return "No ports are published from this devbox.", nil
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("%d port(s) published from this devbox:\n", len(resp.Ports)))
		for _, v := range resp.Ports {
			sb.WriteString(fmt.Sprintf("- %s: %s -> port %d (mode %s), since %s\n", v.App, v.URL, v.ContainerPort, v.Mode, v.CreatedAt))
		}
		return sb.String(), nil

	case "openpoet_devbox_unpublish_port":
		app := strings.TrimSpace(stringParam(params, "app"))
		if app == "" {
			return "", fmt.Errorf("app is required")
		}
		body, err := client.Delete("/api/devbox/ports/" + url.PathEscape(app))
		if err != nil {
			return "", artifactAPIError(err)
		}
		var v devboxPortView
		if err := json.Unmarshal(body, &v); err != nil {
			return string(body), nil
		}
		return fmt.Sprintf("Removed: %s no longer answers.", v.URL), nil
	}
	return "", fmt.Errorf("unknown devbox tool %s", name)
}
