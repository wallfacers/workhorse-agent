// Package webui embeds the reference Web UI assets so the server can serve
// them at /ui without an external static-files dependency.
package webui

import "embed"

//go:embed index.html app.js
var FS embed.FS
