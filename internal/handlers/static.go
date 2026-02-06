package handlers

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

type StaticHandler struct {
	fs      http.FileSystem
	handler http.Handler
}

func NewStaticHandler(embeddedFS embed.FS, subdir string) (*StaticHandler, error) {
	subFS, err := fs.Sub(embeddedFS, subdir)
	if err != nil {
		return nil, err
	}

	httpFS := http.FS(subFS)
	fileServer := http.FileServer(httpFS)

	return &StaticHandler{
		fs:      httpFS,
		handler: fileServer,
	}, nil
}

func (h *StaticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Set cache headers for static assets
	path := r.URL.Path
	if strings.HasPrefix(path, "/static/") {
		if strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".css") {
			w.Header().Set("Cache-Control", "public, max-age=31536000") // 1 year
		} else if strings.HasSuffix(path, ".woff2") || strings.HasSuffix(path, ".woff") {
			w.Header().Set("Cache-Control", "public, max-age=31536000")
		}
	}

	h.handler.ServeHTTP(w, r)
}

// ServeSPA serves a single-page application, returning index.html for unknown routes
func (h *StaticHandler) ServeSPA(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Try to serve the file directly
	if strings.HasPrefix(path, "/static/") || path == "/sw.js" || path == "/manifest.json" {
		h.handler.ServeHTTP(w, r)
		return
	}

	// Check if file exists
	f, err := h.fs.Open(path)
	if err == nil {
		f.Close()
		h.handler.ServeHTTP(w, r)
		return
	}

	// Serve index.html for SPA routes
	r.URL.Path = "/"
	h.handler.ServeHTTP(w, r)
}
