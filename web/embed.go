package web

import "embed"

//go:embed templates static sw.js manifest.json
var FS embed.FS

// Note: static/hooks/bridge.sh is embedded as part of the "static" directory
// It can be served at /static/hooks/bridge.sh or extracted for config sync
