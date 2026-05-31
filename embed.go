package symphony

import (
	"embed"
	"io/fs"
)

//go:embed static/**
var embeddedStaticFiles embed.FS

func StaticFS() fs.FS {
	staticFS, err := fs.Sub(embeddedStaticFiles, "static")
	if err != nil {
		panic(err)
	}
	return staticFS
}
