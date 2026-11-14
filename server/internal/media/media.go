package media

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type Handler struct {
	rootDir string
}

func NewHandler(mediaDir string) *Handler {
	return &Handler{rootDir: filepath.Clean(mediaDir)}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	relPath := strings.TrimPrefix(r.URL.Path, "/media/")
	relPath = strings.TrimPrefix(relPath, "/")
	if relPath == "" || strings.Contains(relPath, "..") {
		http.Error(w, "forbidden path", http.StatusForbidden)
		return
	}

	fullPath := filepath.Clean(filepath.Join(h.rootDir, filepath.FromSlash(relPath)))
	if !strings.HasPrefix(fullPath, h.rootDir+string(filepath.Separator)) {
		http.Error(w, "path traversal blocked", http.StatusForbidden)
		return
	}

	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	etag := fmt.Sprintf(`W/"%x-%x"`, info.ModTime().Unix(), info.Size())
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Accept-Ranges", "bytes")

	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	f, err := os.Open(fullPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()

	// http.ServeContent natively handles HTTP Range requests (206 Partial Content)
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
