// Package docsweb embeds the built developer portal (web/docs/dist, produced by
// `task web:docs:build`). Without a build only a placeholder is embedded and the page answers 503.
package docsweb

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist is the build output rooted at its top directory.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
