// Package assets embeds static assets (HTML templates) for pkgmirror.
package assets

import (
	"embed"
	"io/fs"
)

//go:embed templates/*.html
var embedded embed.FS

// Templates returns a filesystem rooted at the templates directory.
func Templates() fs.FS {
	sub, err := fs.Sub(embedded, "templates")
	if err != nil {
		// fs.Sub on an embed.FS with a literal valid prefix cannot fail in
		// practice. Panic to surface a programmer error if it ever does.
		panic("assets: Sub(\"templates\"): " + err.Error())
	}
	return sub
}
