package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// linkToolView is the JSON of GET /api/links.
type linkToolView struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Title    string `json:"title"`
	Path     string `json:"path"`
	URL      string `json:"url"`
	LocalURL string `json:"local_url"`
	Public   bool   `json:"public"`
}

// linkKindLabels says, for each kind, what the link opens.
var linkKindLabels = map[string]string{
	"artifact": "the artifact viewer",
	"document": "the document viewer",
	"session":  "the session's terminal",
	"task":     "the task",
	"project":  "the project",
}

// executeLinkTool runs openpoet_get_link: it asks the server for the link of
// an artifact, a Doc, a session or a task. The public address of the instance
// is known only to the server, which is why an agent must not build links.
func executeLinkTool(client *APIClient, params map[string]interface{}, sessionID string) (string, error) {
	kind := strings.ToLower(stringParam(params, "kind"))
	if kind == "" {
		return "", fmt.Errorf("kind is required: artifact, document, session or task")
	}
	id := stringParam(params, "id")
	if id == "" && kind == "session" {
		id = sessionID // "this session"
	}
	if id == "" {
		return "", fmt.Errorf("id is required")
	}
	query := url.Values{"kind": {kind}, "id": {id}}
	if version := stringParam(params, "version"); version != "" {
		query.Set("version", version)
	}
	body, err := client.Get("/api/links?" + query.Encode())
	if err != nil {
		return "", artifactAPIError(err)
	}
	return FormatLink(body), nil
}

// FormatLink turns the JSON of a link view into the text the link tool
// returns. The MCP tool and the chat assistant's tool share it.
func FormatLink(body []byte) string {
	var link linkToolView
	if err := json.Unmarshal(body, &link); err != nil || link.Path == "" {
		return string(body)
	}
	what := link.Kind + " " + link.ID
	if title := oneLine(link.Title, 200); title != "" {
		what += fmt.Sprintf(" %q", title)
	}
	opens := linkKindLabels[link.Kind]
	if opens == "" {
		opens = "it"
	}
	if link.Public && link.URL != "" {
		return fmt.Sprintf("Link to %s: %s\n"+
			"It opens %s directly. It works for whoever can reach this OpenPoet instance (its network or VPN, signed in when the instance asks for it). "+
			"Pass on this exact link; do not build another one.", what, link.URL, opens)
	}
	var sb strings.Builder
	sb.WriteString("No public address is configured for this OpenPoet instance, so there is no link to " + what +
		" that works outside the machine it runs on. Tell the user to set \"Public base URL\" in Config → Settings, then ask again.\n")
	if link.LocalURL != "" {
		sb.WriteString("Local link (only on that machine): " + link.LocalURL + "\n")
	}
	sb.WriteString("In-app path: " + link.Path + "\nDo not present either as a link the user can open from another device.")
	return sb.String()
}
