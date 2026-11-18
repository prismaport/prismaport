// Package checkoutweb embeds the built hosted checkout (web/checkout/dist, produced by
// `task web:build`). Without a build only a placeholder is embedded and the pages answer 503.
package checkoutweb

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
