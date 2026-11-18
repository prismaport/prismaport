// Package landingweb embeds the public landing page (web/landing/site): plain HTML, CSS and a
// small script with no build step, served at the site root.
package landingweb

import (
	"embed"
	"io/fs"
)

//go:embed all:site
var site embed.FS

// Site is the landing page's files rooted at their top directory.
func Site() fs.FS {
	sub, err := fs.Sub(site, "site")
	if err != nil {
		panic(err)
	}
	return sub
}
