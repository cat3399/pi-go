//go:build pi_go_webui

// Package webassets contains the optional pi-go WebUI build output. It is only
// linked by the pi-go-web composition root; the default CLI does not import it.
package webassets

import (
	"embed"
	"io/fs"
)

//go:embed all:out
var embedded embed.FS

// FS returns the root of the exported browser application.
func FS() (fs.FS, error) {
	return fs.Sub(embedded, "out")
}
