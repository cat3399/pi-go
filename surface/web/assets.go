//go:build pi_go_webui

package web

import (
	"embed"
	"io/fs"
)

//go:embed all:_frontend/out
var embedded embed.FS

// EmbeddedAssets returns the production browser export linked into pi-go.
func EmbeddedAssets() (fs.FS, error) {
	return fs.Sub(embedded, "_frontend/out")
}
