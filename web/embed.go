// Package web holds the embedded web UI assets served by the core.
//
// The React application is built to webapp/dist and synced into web/dist
// via `scripts/sync-webui.sh` (invoked by `make webui`). The placeholder
// in web/dist/index.html is replaced by the built SPA on sync.
package web

import "embed"

//go:embed dist
var FS embed.FS