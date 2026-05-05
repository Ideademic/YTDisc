package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// App is the singleton bound to the JS frontend. Wails turns its public
// methods into async functions on `window.go.main.App.*`.
type App struct {
	ctx context.Context

	mu        sync.RWMutex
	videosDir string
	library   *Library

	// In-memory thumbnail fallback when disk cache isn't writable
	// (e.g. the USB stick is mounted read-only). Keyed by sha1 of the
	// absolute video path.
	memThumbs map[string][]byte

	// Cached result of edit-capability detection (yt-dlp present +
	// internet reachable). See editor.go.
	editCap editCapCache

	// Resume-playback bookmarks, persisted to Videos/.state.json.
	positions *positionStore

	// Monotonically-incrementing tag for thumbnail-discovery walks.
	// Each Rescan bumps this, and any in-flight walk bails as soon as
	// it notices its tag is stale. Without this, churn (rapid add/
	// delete) stacks goroutines that all walk the library in parallel
	// and race to write the same .thumbs/<key>.jpg files.
	thumbWalkTag uint64
}

func NewApp() *App {
	return &App{
		memThumbs: make(map[string][]byte),
		positions: newPositionStore(),
	}
}

// Called by Wails after the webview is ready.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	dir, err := findVideosDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: %v\n", err)
		return
	}

	a.mu.Lock()
	a.videosDir = dir
	a.library = ScanLibrary(dir)
	a.mu.Unlock()

	// Load persisted resume-playback bookmarks. Cheap (small JSON
	// file) so do it synchronously here.
	a.positions.setLibDir(dir)

	// Discover thumbnails (embedded MP4 cover art + sidecar files) for
	// every video and pre-populate the cache. This runs in the
	// background so the UI can render immediately.
	a.mu.Lock()
	a.thumbWalkTag++
	tag := a.thumbWalkTag
	a.mu.Unlock()
	go a.discoverThumbnails(tag)
}

// discoverThumbnails walks the library and tries, for each video, to
// populate the cache from embedded cover art or a sidecar image file.
// `tag` is the generation this walk was started for; if Rescan bumps
// the tag while we're running, we bail so a fresh walk can take over
// without us doing duplicate work or racing on the same .thumbs files.
func (a *App) discoverThumbnails(tag uint64) {
	a.mu.RLock()
	lib := a.library
	current := a.thumbWalkTag
	a.mu.RUnlock()
	if lib == nil || current != tag {
		return
	}

	for _, ch := range lib.Channels {
		for _, v := range ch.allVideos() {
			// Cheap check between videos so we exit promptly when
			// the user is rapidly creating/deleting things.
			a.mu.RLock()
			stale := a.thumbWalkTag != tag
			a.mu.RUnlock()
			if stale {
				return
			}
			if a.thumbCached(v.AbsPath) {
				continue
			}
			// 1. Embedded cover art.
			if data, err := extractEmbeddedThumb(v.AbsPath); err == nil {
				_ = a.writeThumb(v.AbsPath, data)
				continue
			}
			// 2. Sidecar image file.
			if data, _, err := readSidecarThumb(v.AbsPath); err == nil {
				_ = a.writeThumb(v.AbsPath, data)
				continue
			}
			// 3. Nothing — leave empty. User can manually fetch
			//    via FetchThumbnailFromYouTube or ImportThumbnail.
		}
	}
}

// ---- JS-callable methods --------------------------------------------------

// Status returns whether the library was located and basic counts.
func (a *App) Status() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.library == nil {
		return map[string]any{
			"ok":      false,
			"message": "Couldn't find a Videos/ folder next to the app.",
		}
	}
	return map[string]any{
		"ok":         true,
		"videosDir":  a.videosDir,
		"channels":   len(a.library.Channels),
		"videos":     a.library.TotalVideos(),
		"totalBytes": a.library.TotalBytes(),
		"totalSecs":  a.library.TotalSeconds(),
	}
}

// Channels returns the channel list (folders directly under Videos/).
func (a *App) Channels() []ChannelInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.library == nil {
		return []ChannelInfo{}
	}
	out := make([]ChannelInfo, 0, len(a.library.Channels))
	for _, c := range a.library.Channels {
		out = append(out, ChannelInfo{
			Name:       c.Name,
			VideoCount: c.totalVideoCount(),
			TotalSecs:  c.TotalSeconds(),
		})
	}
	return out
}

// Items returns the contents of a channel (when folderName == "") or
// of a specific folder inside a channel. Folders and videos are
// returned in a single A-Z sorted list keyed by their display name —
// folders interleave with videos rather than grouping at the top, so
// the column behaves like a Finder listing with no folder grouping.
//
// When folderName is non-empty, the result contains only videos
// (folders never nest).
func (a *App) Items(channelName, folderName string) []Item {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.library == nil {
		return []Item{}
	}
	c := a.library.ChannelByName(channelName)
	if c == nil {
		return []Item{}
	}

	if folderName != "" {
		f := c.FolderByName(folderName)
		if f == nil {
			return []Item{}
		}
		out := make([]Item, 0, len(f.Videos))
		for _, v := range f.Videos {
			out = append(out, videoToItem(v))
		}
		// Already sorted at scan time, but the rule for the column is
		// "always A-Z by display name" — re-sort defensively.
		sortItems(out)
		return out
	}

	out := make([]Item, 0, len(c.Videos)+len(c.Folders))
	for _, f := range c.Folders {
		out = append(out, Item{
			Kind:       "folder",
			Name:       f.Name,
			VideoCount: len(f.Videos),
			TotalSecs:  f.TotalSeconds(),
		})
	}
	for _, v := range c.Videos {
		out = append(out, videoToItem(v))
	}
	sortItems(out)
	return out
}

// Rescan rebuilds the library from disk. The scan itself runs OUTSIDE
// the write lock so concurrent /video/ and /thumb/ requests aren't
// blocked while we walk the filesystem and decode every MP4 moov box —
// only the swap of a.library is locked, which is constant-time.
func (a *App) Rescan() {
	a.mu.RLock()
	dir := a.videosDir
	a.mu.RUnlock()
	if dir == "" {
		return
	}
	lib := ScanLibrary(dir)

	a.mu.Lock()
	a.library = lib
	a.thumbWalkTag++
	tag := a.thumbWalkTag
	a.mu.Unlock()
	go a.discoverThumbnails(tag)
}

// HasThumbnail reports whether a thumbnail exists for the given video.
func (a *App) HasThumbnail(relPath string) bool {
	abs, err := a.absVideoPath(relPath)
	if err != nil {
		return false
	}
	return a.thumbCached(abs)
}

// FetchThumbnailFromYouTube downloads the YouTube thumbnail for the
// given URL or bare video ID and saves it for `relPath`.
//
// Accepts: full YouTube URLs (youtube.com/watch?v=..., youtu.be/...,
// shorts), bare 11-char IDs, or filename-style "[ID].mp4" patterns.
func (a *App) FetchThumbnailFromYouTube(relPath string, urlOrID string) error {
	abs, err := a.absVideoPath(relPath)
	if err != nil {
		return err
	}
	id := extractYouTubeID(urlOrID)
	if id == "" {
		// Last try: maybe the user pasted nothing useful but the
		// filename itself contains the ID in brackets.
		id = extractYouTubeID(filepath.Base(relPath))
	}
	if id == "" {
		return errors.New("couldn't find a YouTube video ID in that input")
	}
	data, err := fetchYouTubeThumb(id)
	if err != nil {
		return err
	}
	return a.writeThumb(abs, data)
}

// ImportThumbnailFromFile copies an image file from disk into the
// thumb cache for `relPath`. Used by the "Choose file..." UI button.
func (a *App) ImportThumbnailFromFile(relPath string, filePath string) error {
	abs, err := a.absVideoPath(relPath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("empty image file")
	}
	return a.writeThumb(abs, data)
}

// ClearThumbnail removes the cached thumbnail so the placeholder
// shows again. Useful for "remove thumbnail" UI.
func (a *App) ClearThumbnail(relPath string) error {
	abs, err := a.absVideoPath(relPath)
	if err != nil {
		return err
	}
	key := thumbKey(abs)

	a.mu.Lock()
	delete(a.memThumbs, key)
	dir := a.videosDir
	a.mu.Unlock()

	if dir != "" {
		_ = os.Remove(filepath.Join(dir, ".thumbs", key+".jpg"))
	}
	return nil
}

// ---- internal helpers -----------------------------------------------------

// absVideoPath resolves a slash-separated relative path under Videos/
// to an absolute path on disk. Rejects paths that escape Videos/ via
// "../" segments or via symlinks pointing outside the library — every
// CRUD path on the App goes through this, so it's the chokepoint for
// path-traversal defense on the JS-bindings side (the asset server's
// safeJoin is the equivalent for HTTP-side paths).
func (a *App) absVideoPath(relPath string) (string, error) {
	a.mu.RLock()
	dir := a.videosDir
	a.mu.RUnlock()
	if dir == "" {
		return "", errors.New("library not loaded")
	}
	return safeJoin(dir, relPath)
}

// thumbCached reports whether a thumbnail is cached on disk or in
// memory for the given absolute video path.
func (a *App) thumbCached(absPath string) bool {
	key := thumbKey(absPath)

	a.mu.RLock()
	dir := a.videosDir
	_, inMem := a.memThumbs[key]
	a.mu.RUnlock()

	if inMem {
		return true
	}
	if dir != "" {
		if fileExists(filepath.Join(dir, ".thumbs", key+".jpg")) {
			return true
		}
	}
	return false
}

// writeThumb persists thumbnail bytes for the given absolute video
// path. Tries Videos/.thumbs/<key>.jpg first, falls back to in-memory
// cache if the directory isn't writable (read-only USB).
func (a *App) writeThumb(absPath string, data []byte) error {
	a.mu.RLock()
	dir := a.videosDir
	a.mu.RUnlock()
	if dir == "" {
		return errors.New("library not loaded")
	}

	key := thumbKey(absPath)

	thumbsDir := filepath.Join(dir, ".thumbs")
	if err := os.MkdirAll(thumbsDir, 0o755); err == nil {
		path := filepath.Join(thumbsDir, key+".jpg")
		if err := os.WriteFile(path, data, 0o644); err == nil {
			return nil
		}
	}

	a.mu.Lock()
	a.memThumbs[key] = data
	a.mu.Unlock()
	return nil
}

func thumbKey(absPath string) string {
	sum := sha1.Sum([]byte(absPath))
	return hex.EncodeToString(sum[:])
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// ---- DTOs (these become the TS/JS types Wails generates) ------------------

type ChannelInfo struct {
	Name       string  `json:"name"`
	VideoCount int     `json:"videoCount"`
	TotalSecs  float64 `json:"totalSecs"`
}

// Item is the discriminated row type returned by Items(). The frontend
// switches on Kind ("folder" | "video") to decide how to render and
// what action to take on click.
type Item struct {
	Kind string `json:"kind"` // "folder" or "video"
	Name string `json:"name"` // folder name OR video title

	// folder fields
	VideoCount int     `json:"videoCount,omitempty"`
	TotalSecs  float64 `json:"totalSecs,omitempty"`

	// video fields (TotalSecs above doubles as the video duration)
	Channel   string `json:"channel,omitempty"`
	Folder    string `json:"folder,omitempty"`
	RelPath   string `json:"relPath,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

func videoToItem(v *Video) Item {
	return Item{
		Kind:      "video",
		Name:      v.Title,
		Channel:   v.Channel,
		Folder:    v.Folder,
		RelPath:   filepath.ToSlash(v.RelPath),
		TotalSecs: v.DurationSec,
		Width:     v.Width,
		Height:    v.Height,
		SizeBytes: v.SizeBytes,
	}
}

// sortItems sorts an Item slice case-insensitively by display name, so
// folders and videos interleave alphabetically.
func sortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
}
