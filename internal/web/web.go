// Package web contains the embedded, dependency-free local Web UI.
package web

import (
	"embed"
	"net/http"
)

//go:embed index.html
var files embed.FS

// Handler returns a handler backed entirely by the embedded files.
func Handler() http.Handler {
	return http.FileServer(http.FS(files))
}
