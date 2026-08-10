//go:build !pi_go_webui

package web

import "io/fs"

// EmbeddedAssets keeps the Web command buildable during normal Go development.
// Production builds select assets.go with the pi_go_webui tag.
func EmbeddedAssets() (fs.FS, error) {
	return nil, ErrEmbeddedAssetsUnavailable
}
