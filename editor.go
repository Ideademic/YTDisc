package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"ytdisc/bundled"
)

// ---------------------------------------------------------------------------
// Edit capability detection
// ---------------------------------------------------------------------------

// EditCapability reports whether the user can use the library editing
// features (add/rename/delete videos and channels). Editing requires
// yt-dlp (which ships embedded) and an internet connection — the
// latter so downloads aren't tried offline only to fail noisily.
//
// Rename/delete by themselves don't require yt-dlp or internet, but
// for UX simplicity we gate the entire edit-mode toggle on both. The
// frontend can choose to allow rename/delete even when offline if it
// wants — the backend doesn't enforce that.
//
// As of v1.1.0 ffmpeg is no longer required: yt-dlp's separate
// video+audio streams are muxed by the pure-Go MP4 muxer in
// muxer.go. The Ffmpeg* fields are kept in the JSON DTO for
// frontend compatibility but always report false / empty.
type EditCapability struct {
	Enabled         bool   `json:"enabled"`
	YtDlpAvailable  bool   `json:"ytDlpAvailable"`
	YtDlpVersion    string `json:"ytDlpVersion,omitempty"`
	YtDlpPath       string `json:"ytDlpPath,omitempty"`
	FfmpegAvailable bool   `json:"ffmpegAvailable"` // always false now; retained for DTO stability
	FfmpegPath      string `json:"ffmpegPath,omitempty"`
	Online          bool   `json:"online"`
	Reason          string `json:"reason,omitempty"`
}

type editCapCache struct {
	mu      sync.Mutex
	value   EditCapability
	fetched time.Time
}

const editCapTTL = 8 * time.Second

func (c *editCapCache) get() (EditCapability, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetched.IsZero() && time.Since(c.fetched) < editCapTTL {
		return c.value, true
	}
	return EditCapability{}, false
}

func (c *editCapCache) set(v EditCapability) {
	c.mu.Lock()
	c.value = v
	c.fetched = time.Now()
	c.mu.Unlock()
}

func (c *editCapCache) invalidate() {
	c.mu.Lock()
	c.fetched = time.Time{}
	c.mu.Unlock()
}

// GetEditCapability returns the current edit-mode capability state.
// Cached for editCapTTL so the status bar can poll without hammering
// PATH lookups and DNS dials.
func (a *App) GetEditCapability() EditCapability {
	if v, ok := a.editCap.get(); ok {
		return v
	}
	cap := a.computeEditCapability()
	a.editCap.set(cap)
	return cap
}

// RefreshEditCapability invalidates the cache and recomputes. The
// frontend calls this when the user clicks the (possibly disabled)
// edit badge — gives them an immediate way to retry after fixing
// network or installing ffmpeg.
func (a *App) RefreshEditCapability() EditCapability {
	a.editCap.invalidate()
	return a.GetEditCapability()
}

// computeEditCapability resolves yt-dlp and probes the network.
// yt-dlp is preferred-resolved from the binary embedded in this app
// (extracted asynchronously by extractEmbeddedYtdlpAsync at startup)
// and falls back to PATH lookup on developer builds without the
// embed. ffmpeg is no longer probed: separate video+audio streams
// from yt-dlp are muxed by the pure-Go muxer in muxer.go.
//
// **Fast path.** This method must return promptly because it's the
// first JS call after app boot — historically a slow capability
// check left the status bar stuck at "Loading…". Specifically: we
// do NOT exec yt-dlp here (that's where the slowness came from —
// PyInstaller standalone bundles take ~30 s to start up cold), and
// we do NOT extract the embedded binary on this path; extraction
// runs once on a startup goroutine. Until the goroutine finishes,
// we report a transient "Preparing yt-dlp…" reason.
func (a *App) computeEditCapability() EditCapability {
	cap := EditCapability{}

	cap.YtDlpPath = a.resolveYtdlpPath()
	if cap.YtDlpPath != "" {
		cap.YtDlpAvailable = true
	}

	cap.Online = checkOnline()
	cap.Enabled = cap.YtDlpAvailable && cap.Online

	// Single-line reason for the disabled badge — most actionable
	// problem first. The "Preparing yt-dlp…" branch only fires on
	// the very first launch after install (or first launch after a
	// version that bumps the embedded yt-dlp size).
	switch {
	case !cap.YtDlpAvailable && a.ytdlpExtracting.Load():
		cap.Reason = "Preparing yt-dlp…"
	case !cap.YtDlpAvailable:
		cap.Reason = "yt-dlp not installed"
	case !cap.Online:
		cap.Reason = "No internet connection"
	}
	return cap
}

// resolveYtdlpPath returns an absolute path to a yt-dlp executable
// without doing any blocking work. Just reads the atomic pointer set
// by extractEmbeddedYtdlpAsync (which runs once on startup). If the
// background extraction hasn't finished yet, returns "" (and the
// capability check reports "Preparing yt-dlp…"). For developer
// builds where the embed is the in-repo placeholder, the atomic
// stays nil forever and we fall through to PATH-based discovery.
func (a *App) resolveYtdlpPath() string {
	if p := a.ytdlpExtractedPath.Load(); p != nil {
		return *p
	}
	if bundled.HasEmbeddedYtdlp() {
		// Real embed but extraction goroutine hasn't finished. Don't
		// fall through to PATH — we're about to be ready, and if PATH
		// finds an outdated system yt-dlp we'd silently use the wrong
		// one.
		return ""
	}
	return findCommand("yt-dlp")
}

// findCommand looks up an executable by name. Tries PATH first, then
// falls back to common Homebrew / MacPorts / user install locations.
// The fallback exists in case pathfix.go's PATH augmentation didn't
// cover the user's setup, or in case a child process resets PATH —
// either way returning an absolute path is bulletproof.
func findCommand(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	candidates := []string{
		"/opt/homebrew/bin/" + name, // Apple Silicon Homebrew
		"/usr/local/bin/" + name,    // Intel Homebrew
		"/opt/local/bin/" + name,    // MacPorts
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin", name),
			filepath.Join(home, "bin", name),
		)
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return c
		}
	}
	return ""
}

// checkOnline does a quick TCP dial to a well-known DNS provider on
// port 53. Faster than HTTP and doesn't depend on any specific website
// being reachable. 2 s timeout keeps the UI responsive when offline.
func checkOnline() bool {
	conn, err := net.DialTimeout("tcp", "1.1.1.1:53", 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ---------------------------------------------------------------------------
// Channel CRUD
// ---------------------------------------------------------------------------

// CreateChannel creates a new (empty) channel folder under Videos/.
func (a *App) CreateChannel(name string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if err := validateChannelName(name); err != nil {
		return err
	}

	a.mu.RLock()
	dir := a.videosDir
	a.mu.RUnlock()
	if dir == "" {
		return errors.New("library not loaded")
	}

	chDir := filepath.Join(dir, name)
	if _, err := os.Stat(chDir); err == nil {
		return fmt.Errorf("channel %q already exists", name)
	}
	if err := os.Mkdir(chDir, 0o755); err != nil {
		return err
	}
	a.Rescan()
	return nil
}

// RenameChannel renames a channel folder and migrates the thumbnail
// cache (which is keyed by absolute path).
func (a *App) RenameChannel(oldName, newName string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	newName = strings.TrimSpace(newName)
	if err := validateChannelName(newName); err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}

	a.mu.RLock()
	dir := a.videosDir
	lib := a.library
	a.mu.RUnlock()
	if dir == "" {
		return errors.New("library not loaded")
	}

	oldDir := filepath.Join(dir, oldName)
	newDir := filepath.Join(dir, newName)
	if _, err := os.Stat(newDir); err == nil {
		return fmt.Errorf("channel %q already exists", newName)
	}

	// Snapshot the current absolute paths so we can migrate cache
	// entries after the folder rename.
	var oldPaths []string
	if lib != nil {
		if ch := lib.ChannelByName(oldName); ch != nil {
			for _, v := range ch.Videos {
				oldPaths = append(oldPaths, v.AbsPath)
			}
		}
	}

	if err := os.Rename(oldDir, newDir); err != nil {
		return err
	}
	a.migrateCacheForChannelRename(oldPaths, oldDir, newDir)
	a.subs.renameChannel(oldName, newName)
	a.Rescan()
	return nil
}

// DeleteChannel moves the channel folder to .trash/ within Videos/.
func (a *App) DeleteChannel(name string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	// Drop subscriptions to this channel from every loaded account.
	defer a.subs.removeChannel(name)
	a.mu.RLock()
	dir := a.videosDir
	a.mu.RUnlock()
	if dir == "" {
		return errors.New("library not loaded")
	}
	src := filepath.Join(dir, name)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("channel %q not found", name)
	}
	if err := moveToTrash(dir, src); err != nil {
		return err
	}
	a.Rescan()
	return nil
}

// requireEditor returns errEditorOnly if the current account is not
// the Editor singleton. Every library-mutating method calls this
// first so non-editor accounts get a uniform error rather than
// silently corrupting state. errEditorOnly is defined in app.go.
func (a *App) requireEditor() error {
	if a.accounts == nil || !a.accounts.isCurrentEditor() {
		return errEditorOnly
	}
	return nil
}

// ---------------------------------------------------------------------------
// Folder CRUD (folders live one level inside a channel)
// ---------------------------------------------------------------------------

// CreateFolder creates an (empty) folder inside the named channel.
func (a *App) CreateFolder(channelName, folderName string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	folderName = strings.TrimSpace(folderName)
	if err := validateChannelName(folderName); err != nil {
		return err
	}

	a.mu.RLock()
	dir := a.videosDir
	a.mu.RUnlock()
	if dir == "" {
		return errors.New("library not loaded")
	}

	chDir := filepath.Join(dir, channelName)
	if info, err := os.Stat(chDir); err != nil || !info.IsDir() {
		return fmt.Errorf("channel %q not found", channelName)
	}
	fDir := filepath.Join(chDir, folderName)
	if _, err := os.Stat(fDir); err == nil {
		return fmt.Errorf("folder %q already exists in %q", folderName, channelName)
	}
	if err := os.Mkdir(fDir, 0o755); err != nil {
		return err
	}
	a.Rescan()
	return nil
}

// RenameFolder renames a folder inside the named channel and migrates
// the thumbnail cache for every video inside it.
func (a *App) RenameFolder(channelName, oldName, newName string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	newName = strings.TrimSpace(newName)
	if err := validateChannelName(newName); err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}

	a.mu.RLock()
	dir := a.videosDir
	lib := a.library
	a.mu.RUnlock()
	if dir == "" {
		return errors.New("library not loaded")
	}

	oldDir := filepath.Join(dir, channelName, oldName)
	newDir := filepath.Join(dir, channelName, newName)
	if _, err := os.Stat(newDir); err == nil {
		return fmt.Errorf("folder %q already exists", newName)
	}

	var oldPaths []string
	if lib != nil {
		if ch := lib.ChannelByName(channelName); ch != nil {
			if f := ch.FolderByName(oldName); f != nil {
				for _, v := range f.Videos {
					oldPaths = append(oldPaths, v.AbsPath)
				}
			}
		}
	}

	if err := os.Rename(oldDir, newDir); err != nil {
		return err
	}
	a.migrateCacheForChannelRename(oldPaths, oldDir, newDir)
	a.Rescan()
	return nil
}

// DeleteFolder moves the folder + everything inside it to .trash.
func (a *App) DeleteFolder(channelName, folderName string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	a.mu.RLock()
	dir := a.videosDir
	a.mu.RUnlock()
	if dir == "" {
		return errors.New("library not loaded")
	}
	src := filepath.Join(dir, channelName, folderName)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("folder %q not found in %q", folderName, channelName)
	}
	if err := moveToTrash(dir, src); err != nil {
		return err
	}
	a.Rescan()
	return nil
}

// EmptyFolder moves all videos out of the folder into the channel root,
// then removes the (now empty) folder. Sidecar images and thumbnail
// cache entries are migrated alongside each video. If a name collision
// would occur in the channel root, that one video is skipped (the user
// gets a sensible error and can resolve the collision manually).
func (a *App) EmptyFolder(channelName, folderName string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	a.mu.RLock()
	dir := a.videosDir
	lib := a.library
	a.mu.RUnlock()
	if dir == "" {
		return errors.New("library not loaded")
	}

	chDir := filepath.Join(dir, channelName)
	fDir := filepath.Join(chDir, folderName)
	if _, err := os.Stat(fDir); err != nil {
		return fmt.Errorf("folder %q not found in %q", folderName, channelName)
	}

	var videos []*Video
	if lib != nil {
		if ch := lib.ChannelByName(channelName); ch != nil {
			if f := ch.FolderByName(folderName); f != nil {
				videos = append(videos, f.Videos...)
			}
		}
	}

	for _, v := range videos {
		fname := filepath.Base(v.AbsPath)
		dst := filepath.Join(chDir, fname)
		if _, err := os.Stat(dst); err == nil {
			return fmt.Errorf("can't move %q out of %q: a file with that name already exists at the channel root", fname, folderName)
		}
		if err := os.Rename(v.AbsPath, dst); err != nil {
			return fmt.Errorf("moving %q: %w", fname, err)
		}
		// Sidecar images travel with the video.
		ext := filepath.Ext(v.AbsPath)
		oldStem := strings.TrimSuffix(v.AbsPath, ext)
		newStem := strings.TrimSuffix(dst, ext)
		for _, sidExt := range sidecarExts {
			oldSide := oldStem + sidExt
			if _, err := os.Stat(oldSide); err == nil {
				_ = os.Rename(oldSide, newStem+sidExt)
			}
		}
		a.migrateThumbCache(v.AbsPath, dst)
	}

	// Folder is now empty (modulo hidden files like ._* AppleDouble
	// forks). os.Remove fails on non-empty dirs, so try it first; if
	// it fails because of dotfile residue, fall back to removing the
	// dir tree (we know we just emptied the user-visible content).
	if err := os.Remove(fDir); err != nil {
		_ = os.RemoveAll(fDir)
	}

	a.Rescan()
	return nil
}

// MoveVideo moves a video (and its sidecar image, if any) to a
// different folder inside the SAME channel. destFolder == "" moves the
// video to the channel root. Cross-channel moves aren't supported here —
// keep it simple; the user can rename the channel separately if needed.
func (a *App) MoveVideo(relPath, destFolder string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	abs, err := a.absVideoPath(relPath)
	if err != nil {
		return err
	}

	a.mu.RLock()
	dir := a.videosDir
	a.mu.RUnlock()
	if dir == "" {
		return errors.New("library not loaded")
	}

	parts := strings.Split(filepath.ToSlash(relPath), "/")
	if len(parts) < 2 {
		return errors.New("invalid video path")
	}
	channel := parts[0]
	fname := parts[len(parts)-1]

	destDir := filepath.Join(dir, channel)
	if destFolder != "" {
		if err := validateChannelName(destFolder); err != nil {
			return err
		}
		destDir = filepath.Join(dir, channel, destFolder)
		if info, err := os.Stat(destDir); err != nil || !info.IsDir() {
			return fmt.Errorf("destination folder %q not found in %q", destFolder, channel)
		}
	}

	newAbs := filepath.Join(destDir, fname)
	if newAbs == abs {
		return nil
	}
	if _, err := os.Stat(newAbs); err == nil {
		return errors.New("a file with that name already exists at the destination")
	}

	if err := os.Rename(abs, newAbs); err != nil {
		return err
	}

	ext := filepath.Ext(abs)
	oldStem := strings.TrimSuffix(abs, ext)
	newStem := strings.TrimSuffix(newAbs, ext)
	for _, sidExt := range sidecarExts {
		oldSide := oldStem + sidExt
		if _, err := os.Stat(oldSide); err == nil {
			_ = os.Rename(oldSide, newStem+sidExt)
		}
	}
	a.migrateThumbCache(abs, newAbs)
	a.Rescan()
	return nil
}

// ---------------------------------------------------------------------------
// Video CRUD
// ---------------------------------------------------------------------------

// RenameVideo renames a video file. newTitle should NOT include the
// extension; the existing extension is preserved. Sidecar images and
// thumbnail cache are migrated too.
func (a *App) RenameVideo(relPath, newTitle string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	newTitle = strings.TrimSpace(newTitle)
	if err := validateFileName(newTitle); err != nil {
		return err
	}

	abs, err := a.absVideoPath(relPath)
	if err != nil {
		return err
	}

	parent := filepath.Dir(abs)
	ext := filepath.Ext(abs)
	newAbs := filepath.Join(parent, newTitle+ext)
	if newAbs == abs {
		return nil
	}
	if _, err := os.Stat(newAbs); err == nil {
		return errors.New("a file with that name already exists")
	}

	if err := os.Rename(abs, newAbs); err != nil {
		return err
	}

	// Move sidecar images alongside.
	oldStem := strings.TrimSuffix(abs, ext)
	newStem := strings.TrimSuffix(newAbs, ext)
	for _, sidExt := range sidecarExts {
		oldSide := oldStem + sidExt
		if _, err := os.Stat(oldSide); err == nil {
			_ = os.Rename(oldSide, newStem+sidExt)
		}
	}

	a.migrateThumbCache(abs, newAbs)
	a.Rescan()
	return nil
}

// DeleteVideo moves a video and its sidecars to .trash/, and clears
// the thumbnail cache entry.
func (a *App) DeleteVideo(relPath string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	abs, err := a.absVideoPath(relPath)
	if err != nil {
		return err
	}

	a.mu.RLock()
	dir := a.videosDir
	a.mu.RUnlock()
	if dir == "" {
		return errors.New("library not loaded")
	}

	if err := moveToTrash(dir, abs); err != nil {
		return err
	}

	ext := filepath.Ext(abs)
	stem := strings.TrimSuffix(abs, ext)
	for _, sidExt := range sidecarExts {
		side := stem + sidExt
		if _, err := os.Stat(side); err == nil {
			_ = moveToTrash(dir, side)
		}
	}

	key := thumbKey(abs)
	a.mu.Lock()
	delete(a.memThumbs, key)
	a.mu.Unlock()
	_ = os.Remove(filepath.Join(dir, ".thumbs", key+".jpg"))
	if a.positions != nil {
		// Drop the bookmark from every loaded account so a deleted
		// file doesn't linger in anyone's resume map.
		a.positions.clearAcrossAccounts(filepath.ToSlash(relPath))
	}

	a.Rescan()
	return nil
}

// ---------------------------------------------------------------------------
// yt-dlp download
// ---------------------------------------------------------------------------

// AddVideos downloads each URL into the given (existing) channel using
// yt-dlp. quality selects the format ceiling — "fhd" (default, no
// ceiling), "hd" (720p), or "sd" (480p). If destFolder is non-empty,
// downloads land inside that folder under the channel.
//
// Each URL that looks like a playlist (contains a list= parameter)
// triggers a separate code path: we ask yt-dlp for the playlist's
// title, create a folder under the channel named after it (avoiding
// collisions), and run yt-dlp with playlist mode enabled into that
// folder. Single-video URLs remain a flat download into destFolder.
//
// Progress is streamed to the frontend over the "ytdlp-progress"
// Wails event. Returns when the entire batch completes (or on first
// error).
func (a *App) AddVideos(channelName, destFolder string, urls []string, quality string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	cap := a.GetEditCapability()
	if !cap.Enabled {
		return errors.New(cap.Reason)
	}

	a.mu.RLock()
	dir := a.videosDir
	ctx := a.ctx
	a.mu.RUnlock()
	if dir == "" {
		return errors.New("library not loaded")
	}

	chDir := filepath.Join(dir, channelName)
	info, err := os.Stat(chDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("channel %q not found", channelName)
	}

	baseDir := chDir
	if destFolder != "" {
		baseDir = filepath.Join(chDir, destFolder)
		if info, err := os.Stat(baseDir); err != nil || !info.IsDir() {
			return fmt.Errorf("folder %q not found in %q", destFolder, channelName)
		}
	}

	// Filter out empty strings before counting "total" so the progress
	// indicator is accurate.
	clean := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u != "" {
			clean = append(clean, u)
		}
	}
	if len(clean) == 0 {
		return errors.New("no URLs provided")
	}

	total := len(clean)
	for i, url := range clean {
		runtime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
			"current": i + 1,
			"total":   total,
			"phase":   "starting",
			"url":     url,
		})

		// Decide where this URL's downloads land. Playlists always
		// create a new folder under the channel root (named after the
		// playlist); when destFolder is set, we still place the
		// playlist folder at the channel root rather than nesting,
		// since the model only allows one level of folder depth.
		downloadDir := baseDir
		isPlaylist := looksLikePlaylistURL(url)
		if isPlaylist {
			title, err := fetchPlaylistTitle(ctx, cap.YtDlpPath, url)
			if err != nil || title == "" {
				title = "Playlist"
			}
			folder, err := uniqueFolderName(chDir, sanitizeFolderName(title))
			if err != nil {
				runtime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
					"current": i + 1, "total": total, "phase": "error",
					"url": url, "error": err.Error(),
				})
				a.Rescan()
				return err
			}
			downloadDir = filepath.Join(chDir, folder)
			if err := os.Mkdir(downloadDir, 0o755); err != nil {
				runtime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
					"current": i + 1, "total": total, "phase": "error",
					"url": url, "error": err.Error(),
				})
				a.Rescan()
				return err
			}
			runtime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
				"phase": "log", "url": url,
				"line": fmt.Sprintf("[ytdisc] playlist → folder %q", folder),
			})
		}

		if err := runYtdlp(ctx, cap.YtDlpPath, downloadDir, url, quality, isPlaylist); err != nil {
			runtime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
				"current": i + 1,
				"total":   total,
				"phase":   "error",
				"url":     url,
				"error":   err.Error(),
			})
			a.Rescan() // pick up any partial successes from earlier URLs
			return err
		}

		// yt-dlp wanted to merge separate video+audio streams via
		// ffmpeg but we don't ship ffmpeg, so the per-stream temp
		// files (Title.f137.mp4 + Title.f140.m4a) are still on disk.
		// Mux them into a single Title.mp4 ourselves with the pure-
		// Go muxer in muxer.go.
		if err := mergeOrphanStreams(ctx, downloadDir); err != nil {
			runtime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
				"phase": "log",
				"url":   url,
				"line":  fmt.Sprintf("[ytdisc] mux warning: %v", err),
			})
			// Don't fail — leave streams in place, user can retry.
		}

		runtime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
			"current": i + 1,
			"total":   total,
			"phase":   "done",
			"url":     url,
		})
	}

	a.Rescan()
	runtime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
		"phase": "all-done",
		"total": total,
	})
	return nil
}

// formatSelector returns the yt-dlp -f argument for a given quality
// label. We always force H.264 video (avc1) so WKWebView on macOS can
// play the result — see runYtdlp's notes for context. Heights are an
// inclusive ceiling; "fhd" has no ceiling so 1080p+ is allowed where
// available.
func formatSelector(quality string) string {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "sd":
		return "bv*[vcodec^=avc1][height<=480]+ba[ext=m4a]/b[ext=mp4][height<=480]/b[height<=480]"
	case "hd":
		return "bv*[vcodec^=avc1][height<=720]+ba[ext=m4a]/b[ext=mp4][height<=720]/b[height<=720]"
	default: // "fhd" or anything unknown
		return "bv*[vcodec^=avc1]+ba[ext=m4a]/b[ext=mp4]/b"
	}
}

// looksLikePlaylistURL is a cheap text check that mirrors yt-dlp's own
// playlist detection: a YouTube URL with a list= query parameter (or
// the dedicated playlist endpoint). Single-video URLs that incidentally
// link out to a playlist (?v=…&list=…) are also treated as playlists,
// matching yt-dlp's default when --no-playlist is omitted.
func looksLikePlaylistURL(url string) bool {
	lower := strings.ToLower(url)
	if !strings.Contains(lower, "youtube.com") && !strings.Contains(lower, "youtu.be") {
		return false
	}
	return strings.Contains(lower, "list=") || strings.Contains(lower, "/playlist")
}

// fetchPlaylistTitle asks yt-dlp for just the playlist's title, using
// --flat-playlist + --print so no videos are downloaded. Returns the
// raw title (already trimmed). 10 s timeout — playlist title queries
// are quick; if it stalls, we fall back to a generic name.
func fetchPlaylistTitle(ctx context.Context, ytdlp, url string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// `--` separates flags from positional URL — see runYtdlp for why.
	out, err := exec.CommandContext(c, ytdlp,
		"--flat-playlist",
		"--playlist-items", "1",
		"--print", "%(playlist_title)s",
		"--", url,
	).Output()
	if err != nil {
		return "", err
	}
	// yt-dlp prints "NA" when there's no playlist title; treat that
	// like an empty result so the fallback name kicks in.
	title := strings.TrimSpace(string(out))
	if title == "" || title == "NA" {
		return "", nil
	}
	return title, nil
}

// sanitizeFolderName strips characters our cross-platform validation
// would reject (`/ \ : * ? " < > |`) and trims the result so it never
// starts with a dot or exceeds the channel-name length cap.
func sanitizeFolderName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if strings.ContainsRune(invalidNameChars, r) {
			b.WriteRune('-')
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimLeft(out, ".")
	if out == "" {
		out = "Playlist"
	}
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

// uniqueFolderName returns base if no folder of that name exists in
// chDir, otherwise base + " (2)", " (3)", … Stops trying at 100 and
// returns an error rather than looping forever.
func uniqueFolderName(chDir, base string) (string, error) {
	candidate := base
	for i := 2; i < 100; i++ {
		if _, err := os.Stat(filepath.Join(chDir, candidate)); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s (%d)", base, i)
	}
	return "", fmt.Errorf("couldn't pick a unique folder name based on %q", base)
}

// runYtdlp downloads one URL into outDir. Streams stdout/stderr lines
// as "ytdlp-progress" events with phase="log" so the frontend can
// show live status. quality picks the format-selector ceiling; if
// playlist is true, --no-playlist is omitted so yt-dlp downloads
// every entry in the playlist.
//
// We deliberately do NOT pass --ffmpeg-location: yt-dlp's internal
// merger fails when it can't find ffmpeg, leaving the per-stream
// `Title.f137.mp4` + `Title.f140.m4a` files on disk, which our own
// pure-Go muxer (mergeOrphanStreams → muxAVtoMP4) picks up. yt-dlp
// is otherwise self-contained — no other postprocessor in our
// invocation needs ffmpeg (we use --write-thumbnail to keep
// thumbnails as sidecars rather than embedding them).
func runYtdlp(ctx context.Context, ytdlp, outDir, url, quality string, playlist bool) error {
	outTemplate := filepath.Join(outDir, "%(title)s.%(ext)s")
	if playlist {
		// Add a numeric prefix to the filename so playlist videos sort
		// in the order yt-dlp downloaded them (which matches the
		// playlist order). Without this they'd sort A-Z by title in
		// the UI, which loses the playlist's intended sequence.
		outTemplate = filepath.Join(outDir, "%(playlist_index)03d - %(title)s.%(ext)s")
	}

	args := []string{
		"-o", outTemplate,
		// Force H.264 (avc1) video so the file plays in WKWebView's
		// native <video> element on macOS — AV1 (format 399) and VP9
		// don't reliably play there. Selector preference per quality
		// tier; see formatSelector.
		"-f", formatSelector(quality),
		// Write the thumbnail as a sidecar file (Title.webp or .jpg)
		// rather than embedding it. Our scanner already discovers
		// sidecar images alongside videos, so the UX is identical —
		// and embedding would require ffmpeg via the
		// ThumbnailsConvertor postprocessor.
		"--write-thumbnail",
		"--merge-output-format", "mp4",
		"--newline",  // emits one line per progress update (vs \r-overwrite)
		"--progress", // ensure progress output even when not a TTY
	}
	if !playlist {
		// Default behavior: when a single-video URL also has list=…
		// (because the user copy-pasted from YouTube while in a
		// playlist), don't pull the entire playlist. Playlist URLs
		// take the other branch, which omits this flag.
		args = append(args, "--no-playlist")
	} else {
		args = append(args, "--yes-playlist")
	}
	// `--` is a hard end-of-flags marker — without it, a URL that
	// starts with "-" (e.g. a malicious "--config-location=/tmp/x")
	// would be parsed by yt-dlp as a flag, not a positional URL.
	// yt-dlp has flags that load arbitrary files / run commands
	// (--exec, --config-location, --load-info-json), so flag injection
	// here would be a code-execution primitive. The "--" makes that
	// impossible regardless of URL contents.
	args = append(args, "--", url)

	// Log the full invocation so any future "didn't work" report shows
	// exactly what was run. Sent through the same event stream the
	// frontend already displays in the Add Videos modal log panel.
	runtime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
		"phase": "log",
		"url":   url,
		"line":  fmt.Sprintf("[ytdisc] exec: %s %s", ytdlp, strings.Join(args, " ")),
	})

	cmd := exec.CommandContext(ctx, ytdlp, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout // merge stderr into stdout for unified streaming

	if err := cmd.Start(); err != nil {
		return err
	}

	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		streamLines(ctx, stdout, url)
	}()

	waitErr := cmd.Wait()
	<-streamDone
	return waitErr
}

// streamLines reads from r line-by-line (handling both \n and \r as
// line separators since yt-dlp progress uses \r in some modes) and
// emits each line as a progress event.
func streamLines(ctx context.Context, r io.Reader, url string) {
	defer func() {
		// closeable readers benefit; non-closeable ignore.
		if c, ok := r.(io.Closer); ok {
			c.Close()
		}
	}()

	buf := make([]byte, 4096)
	var leftover []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			leftover = append(leftover, buf[:n]...)
			for {
				idx := indexLineBreak(leftover)
				if idx < 0 {
					break
				}
				line := strings.TrimSpace(string(leftover[:idx]))
				leftover = leftover[idx+1:]
				if line != "" {
					runtime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
						"phase": "log",
						"url":   url,
						"line":  line,
					})
				}
			}
		}
		if err != nil {
			if len(leftover) > 0 {
				line := strings.TrimSpace(string(leftover))
				if line != "" {
					runtime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
						"phase": "log",
						"url":   url,
						"line":  line,
					})
				}
			}
			return
		}
	}
}

func indexLineBreak(b []byte) int {
	for i, c := range b {
		if c == '\n' || c == '\r' {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Defensive merge: yt-dlp leaves orphans, we clean up
// ---------------------------------------------------------------------------

// orphanStreamRe matches yt-dlp's per-stream temp filenames that get
// left behind when its merger silently fails:
//   "Title.f137.mp4" → ["Title", "137", "mp4"]
//   "Title.f140.m4a" → ["Title", "140", "m4a"]
var orphanStreamRe = regexp.MustCompile(`^(.*)\.f\d+\.(mp4|m4a)$`)

// mergeOrphanStreams scans chDir for unmerged yt-dlp stream files and
// muxes each video+audio pair into a single Title.mp4 using the
// pure-Go muxer in muxer.go. Stream-copy only (no transcode), so
// AVC + AAC pairs mux in seconds. Source files are removed on
// success.
//
// yt-dlp leaves these orphan pairs whenever it would normally merge
// them via ffmpeg but can't find ffmpeg on PATH (which is now the
// expected case — we deliberately don't ship ffmpeg). Doing the mux
// here means edit mode works without any external dependencies.
func mergeOrphanStreams(ctx context.Context, chDir string) error {
	entries, err := os.ReadDir(chDir)
	if err != nil {
		return nil
	}

	type pair struct{ video, audio string }
	pairs := make(map[string]*pair)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip dotfiles, including macOS AppleDouble resource forks
		// ("._Title.f137.mp4") that appear on exFAT/FAT32 USB drives.
		// Without this, we'd try to "merge" the metadata forks and
		// inevitably fail.
		if strings.HasPrefix(name, ".") {
			continue
		}
		m := orphanStreamRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		base, ext := m[1], m[2]
		if pairs[base] == nil {
			pairs[base] = &pair{}
		}
		full := filepath.Join(chDir, name)
		switch ext {
		case "mp4":
			pairs[base].video = full
		case "m4a":
			pairs[base].audio = full
		}
	}

	for base, p := range pairs {
		if p.video == "" || p.audio == "" {
			continue
		}
		out := filepath.Join(chDir, base+".mp4")
		// Skip if a final merged file already exists.
		if _, err := os.Stat(out); err == nil {
			continue
		}

		runtime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
			"phase": "log",
			"url":   base,
			"line":  fmt.Sprintf("[ytdisc] muxing %s.{f*.mp4,f*.m4a} → %s.mp4", base, base),
		})

		// Pure-Go remux. Returns on first error so the user sees
		// what went wrong (rather than silently leaving streams
		// behind). The orphan files are kept on failure so the
		// user can manually inspect/recover.
		if err := muxAVtoMP4(p.video, p.audio, out); err != nil {
			return fmt.Errorf("muxing %q failed: %w", base, err)
		}

		// Clean up the per-stream temp files now that the mux succeeded.
		_ = os.Remove(p.video)
		_ = os.Remove(p.audio)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Cache migration helpers
// ---------------------------------------------------------------------------

// migrateThumbCache moves a single video's cache entry from the
// old absolute path to the new one, and migrates the resume-playback
// bookmark too (keyed by relative path under Videos/).
func (a *App) migrateThumbCache(oldAbs, newAbs string) {
	a.mu.RLock()
	dir := a.videosDir
	a.mu.RUnlock()
	if dir == "" {
		return
	}

	oldKey := thumbKey(oldAbs)
	newKey := thumbKey(newAbs)
	if oldKey != newKey {
		oldCache := filepath.Join(dir, ".thumbs", oldKey+".jpg")
		newCache := filepath.Join(dir, ".thumbs", newKey+".jpg")
		if _, err := os.Stat(oldCache); err == nil {
			_ = os.Rename(oldCache, newCache)
		}

		a.mu.Lock()
		if data, ok := a.memThumbs[oldKey]; ok {
			a.memThumbs[newKey] = data
			delete(a.memThumbs, oldKey)
		}
		a.mu.Unlock()
	}

	// Migrate resume bookmark if present. Apply across every loaded
	// account so each user's bookmark for the renamed file follows
	// the file. Cold accounts (never accessed this session) are
	// reconciled at next-load time.
	if a.positions != nil {
		oldRel, err1 := filepath.Rel(dir, oldAbs)
		newRel, err2 := filepath.Rel(dir, newAbs)
		if err1 == nil && err2 == nil {
			a.positions.renameAcrossAccounts(filepath.ToSlash(oldRel), filepath.ToSlash(newRel))
		}
	}
}

// migrateCacheForChannelRename migrates cache entries for every video
// inside a renamed channel. The list of old absolute paths is captured
// before the folder rename happens.
func (a *App) migrateCacheForChannelRename(oldPaths []string, oldDir, newDir string) {
	for _, oldAbs := range oldPaths {
		rel, err := filepath.Rel(oldDir, oldAbs)
		if err != nil {
			continue
		}
		newAbs := filepath.Join(newDir, rel)
		a.migrateThumbCache(oldAbs, newAbs)
	}
}

// ---------------------------------------------------------------------------
// Filesystem helpers
// ---------------------------------------------------------------------------

// moveToTrash moves src into <libDir>/.trash/<timestamp>_<name>.
// Recoverable until the user empties .trash manually. The .trash
// folder is hidden so the library scanner already skips it.
func moveToTrash(libDir, src string) error {
	trashDir := filepath.Join(libDir, ".trash")
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return err
	}
	stamp := time.Now().Format("20060102-150405")
	dst := filepath.Join(trashDir, stamp+"_"+filepath.Base(src))
	return os.Rename(src, dst)
}

// invalidNameChars: filesystem-illegal characters across macOS, Linux,
// Windows (we target portability since the library lives on a USB
// stick that may move between systems). Slash and backslash for path
// separators; the rest for Windows-FAT/NTFS compatibility.
const invalidNameChars = `/\:*?"<>|`

// windowsReservedNames are case-insensitive base names that Windows
// refuses to create as files OR directories, regardless of extension.
// Since the library is portable across macOS / Linux / Windows on the
// same USB stick, we reject these everywhere even though Unix would
// accept them.
var windowsReservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// isWindowsReservedName reports whether name (or its stem before the
// first dot) collides with a Windows reserved device name. Compared
// case-insensitively, with the extension stripped, matching Windows'
// own behavior.
func isWindowsReservedName(name string) bool {
	stem := strings.ToLower(name)
	if i := strings.Index(stem, "."); i >= 0 {
		stem = stem[:i]
	}
	return windowsReservedNames[stem]
}

func validateChannelName(name string) error {
	if name == "" {
		return errors.New("channel name cannot be empty")
	}
	if strings.HasPrefix(name, ".") {
		return errors.New("channel name cannot start with a dot")
	}
	if strings.ContainsAny(name, invalidNameChars) {
		return errors.New(`channel name cannot contain / \ : * ? " < > |`)
	}
	if isWindowsReservedName(name) {
		return errors.New("that name is reserved on Windows — pick another")
	}
	if len(name) > 200 {
		return errors.New("channel name too long")
	}
	return nil
}

func validateFileName(name string) error {
	if name == "" {
		return errors.New("name cannot be empty")
	}
	if strings.HasPrefix(name, ".") {
		return errors.New("name cannot start with a dot")
	}
	if strings.ContainsAny(name, invalidNameChars) {
		return errors.New(`name cannot contain / \ : * ? " < > |`)
	}
	if isWindowsReservedName(name) {
		return errors.New("that name is reserved on Windows — pick another")
	}
	if len(name) > 240 {
		return errors.New("name too long")
	}
	return nil
}