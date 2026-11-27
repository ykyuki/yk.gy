// Package web embeds the single-page front-end into the Wasm binary.
package web

import (
	"embed"

	"github.com/ykyuki/yk.gy/internal/handler"
)

//go:embed index.html privacy.html style.css app.js favicon.svg robots.txt
var files embed.FS

var contentTypes = map[string]string{
	"index.html":   "text/html; charset=utf-8",
	"privacy.html": "text/html; charset=utf-8",
	"style.css":    "text/css; charset=utf-8",
	"app.js":       "text/javascript; charset=utf-8",
	"favicon.svg":  "image/svg+xml",
	"robots.txt":   "text/plain; charset=utf-8",
}

// Assets returns the embedded files keyed by name.
func Assets() map[string]handler.Asset {
	out := make(map[string]handler.Asset, len(contentTypes))
	for name, ct := range contentTypes {
		b, err := files.ReadFile(name)
		if err != nil {
			continue
		}
		out[name] = handler.Asset{ContentType: ct, Body: b}
	}
	return out
}
