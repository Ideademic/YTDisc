package main

import (
	"net/http"
	"path/filepath"
	"strings"
)

// MediaHandler handles four URL prefixes the frontend uses:
//
//   /video/<rel-path>   stream a Videos/<rel-path> file with HTTP
//                       Range support so <video> can seek
//   /thumb/<rel-path>   serve a cached video thumbnail (or 404 if
//                       not yet generated; frontend regenerates on miss)
//   /audio/<rel-path>   stream a Music/<rel-path> file (m4a) with
//                       Range so the <audio> element can seek
//   /art/<rel-path>     serve song / album art — looks for a sidecar
//                       image next to the song first, falls back to
//                       a cached one in Music/.arts/
//
// Anything else falls through to the default Wails asset server,
// which serves the embedded frontend bundle (index.html, JS, CSS).
type MediaHandler struct {
	app *App
}

func NewMediaHandler(app *App) http.Handler {
	return &MediaHandler{app: app}
}

func (h *MediaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/video/"):
		h.serveVideo(w, r, strings.TrimPrefix(r.URL.Path, "/video/"))
	case strings.HasPrefix(r.URL.Path, "/thumb/"):
		h.serveThumb(w, r, strings.TrimPrefix(r.URL.Path, "/thumb/"))
	case strings.HasPrefix(r.URL.Path, "/audio/"):
		h.serveAudio(w, r, strings.TrimPrefix(r.URL.Path, "/audio/"))
	case strings.HasPrefix(r.URL.Path, "/art/"):
		h.serveArt(w, r, strings.TrimPrefix(r.URL.Path, "/art/"))
	default:
		http.NotFound(w, r)
	}
}

func (h *MediaHandler) serveVideo(w http.ResponseWriter, r *http.Request, relPath string) {
	h.app.mu.RLock()
	root := h.app.videosDir
	h.app.mu.RUnlock()
	if root == "" {
		http.Error(w, "library not loaded", http.StatusServiceUnavailable)
		return
	}

	abs, err := safeJoin(root, relPath)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	// http.ServeFile handles Range requests, ETag, conditional GETs.
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeFile(w, r, abs)
}

func (h *MediaHandler) serveThumb(w http.ResponseWriter, r *http.Request, relPath string) {
	h.app.mu.RLock()
	root := h.app.videosDir
	h.app.mu.RUnlock()
	if root == "" {
		http.Error(w, "library not loaded", http.StatusServiceUnavailable)
		return
	}

	abs, err := safeJoin(root, relPath)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	key := thumbKey(abs)

	// Disk cache first.
	thumbPath := filepath.Join(root, ".thumbs", key+".jpg")
	if fileExists(thumbPath) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		http.ServeFile(w, r, thumbPath)
		return
	}

	// Memory cache fallback.
	h.app.mu.RLock()
	jpeg, ok := h.app.memThumbs[key]
	h.app.mu.RUnlock()
	if ok {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		_, _ = w.Write(jpeg)
		return
	}

	http.NotFound(w, r)
}

func (h *MediaHandler) serveAudio(w http.ResponseWriter, r *http.Request, relPath string) {
	h.app.mu.RLock()
	root := h.app.musicDir
	h.app.mu.RUnlock()
	if root == "" {
		http.Error(w, "music library not loaded", http.StatusServiceUnavailable)
		return
	}
	abs, err := safeJoin(root, relPath)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeFile(w, r, abs)
}

// serveArt resolves /art/<song-rel-path>. We don't key art by hash
// like video thumbnails — songs already have a stable file location
// next to them, so a sidecar (or shared cover.jpg in the album dir)
// is the canonical source. If a sidecar isn't present we look for a
// cached image at Music/.arts/<sha1>.jpg (where sha1 is the song's
// absolute path), which is where future yt-dlp downloads will write.
func (h *MediaHandler) serveArt(w http.ResponseWriter, r *http.Request, relPath string) {
	h.app.mu.RLock()
	root := h.app.musicDir
	h.app.mu.RUnlock()
	if root == "" {
		http.NotFound(w, r)
		return
	}
	abs, err := safeJoin(root, relPath)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if sidecar := musicArtFor(abs); sidecar != "" {
		w.Header().Set("Cache-Control", "private, max-age=86400")
		http.ServeFile(w, r, sidecar)
		return
	}
	cached := filepath.Join(root, ".arts", thumbKey(abs)+".jpg")
	if fileExists(cached) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		http.ServeFile(w, r, cached)
		return
	}
	http.NotFound(w, r)
}

// safeJoin resolves rel against root and rejects any result that escapes
// root (defense against ../ in the URL OR a symlink inside Videos/
// pointing outside it). Symlinks are resolved before the prefix check
// so a malicious or sloppy symlink can't be used as a file-read
// primitive against arbitrary paths on disk.
func safeJoin(root, rel string) (string, error) {
	rel = filepath.FromSlash(rel)
	abs := filepath.Join(root, rel)

	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	// Resolve any symlinks in the root (e.g. /Volumes/USB might be a
	// symlink target). We use the resolved root as the prefix so a
	// child resolved via EvalSymlinks compares apples to apples.
	if resolvedRoot, err := filepath.EvalSymlinks(cleanRoot); err == nil {
		cleanRoot = resolvedRoot
	}

	cleanAbs, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	// EvalSymlinks fails if the file doesn't exist; that's actually a
	// useful signal (the URL points at a non-existent file), but we
	// still want a meaningful error for the caller. Fall back to the
	// non-resolved path in that case — http.ServeFile will then 404.
	if resolved, err := filepath.EvalSymlinks(cleanAbs); err == nil {
		cleanAbs = resolved
	}

	if !strings.HasPrefix(cleanAbs+string(filepath.Separator), cleanRoot+string(filepath.Separator)) &&
		cleanAbs != cleanRoot {
		return "", &pathErr{}
	}
	return cleanAbs, nil
}

type pathErr struct{}

func (e *pathErr) Error() string { return "path escapes root" }
