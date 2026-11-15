package subtitletranslator

import (
	"embed"
	"io/fs"
)

//go:embed all:web/dist
var frontendFiles embed.FS

//go:embed locales/*.json
var localeFiles embed.FS

// Frontend contains the Vite production build. Build web/ before the server.
func Frontend() fs.FS {
	sub, _ := fs.Sub(frontendFiles, "web/dist")
	return sub
}

// Locales contains the bundled interface catalogs.
func Locales() fs.FS {
	sub, _ := fs.Sub(localeFiles, "locales")
	return sub
}
