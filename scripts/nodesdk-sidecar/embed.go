package sidecar

import "embed"

//go:embed index.js tools.js package.json package-lock.json
var FS embed.FS
