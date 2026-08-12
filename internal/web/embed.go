package web

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
)

//go:embed all:dist
var distFS embed.FS

// Handler returns an http.Handler that serves the embedded frontend.
// Falls back to index.html for SPA routing.
// If dist/ doesn't exist (dev mode), returns nil.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil
	}

	// Check if dist has content
	entries, _ := fs.ReadDir(sub, ".")
	if len(entries) == 0 {
		return nil
	}

	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try to serve the file directly
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}
		if _, err := fs.Stat(sub, path[1:]); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA fallback: serve index.html
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}

// DevDistExists checks if a local dist/ directory exists (for dev builds).
func DevDistExists() bool {
	info, err := os.Stat("dist")
	return err == nil && info.IsDir()
}
