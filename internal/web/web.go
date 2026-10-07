// Package web embeds the investigation console (static HTML, CSS, JS; no
// build step).
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// FS returns the console's files rooted at the static directory.
func FS() fs.FS {
	sub, _ := fs.Sub(files, "static")
	return sub
}
