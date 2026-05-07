package main

// JS-callable methods for the Music tab + the playlist subsystem +
// the music-video attach flow. Lives in its own file to keep
// editor.go focused on the Videos tab — Wails binds methods by
// receiver type, not by file, so this is purely organizational.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ---- Music library queries (JS-callable) ---------------------------------

// MusicArtists returns every artist in Music/, with their album list
// inline. Cheap because it's just the in-memory tree — call it
// freely.
func (a *App) MusicArtists() []ArtistInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.music == nil {
		return []ArtistInfo{}
	}
	out := make([]ArtistInfo, 0, len(a.music.Artists))
	for _, ar := range a.music.Artists {
		out = append(out, toArtistInfo(ar))
	}
	return out
}

// AlbumSongs returns the songs in a specific album for a specific
// artist. Returns the songs in their on-disk filename order so a
// numeric track-number prefix (the default yt-dlp playlist template)
// produces correct album playback order.
func (a *App) AlbumSongs(artistName, albumName string) []SongInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.music == nil {
		return []SongInfo{}
	}
	ar := a.music.ArtistByName(artistName)
	if ar == nil {
		return []SongInfo{}
	}
	al := ar.AlbumByName(albumName)
	if al == nil {
		return []SongInfo{}
	}
	out := make([]SongInfo, 0, len(al.Songs))
	for _, s := range al.Songs {
		out = append(out, toSongInfo(s))
	}
	return out
}

// HasMusicArt reports whether art is available for a song. Looks for
// a sibling jpg/png/webp in the same album dir, OR a pre-cached art
// in Music/.arts/. Used by the frontend to decide whether to render
// /art/<rel> or the placeholder.
func (a *App) HasMusicArt(songRelPath string) bool {
	abs, err := a.absMusicPath(songRelPath)
	if err != nil {
		return false
	}
	return musicArtFor(abs) != "" || a.musicArtCacheExists(abs)
}

// musicArtFor returns the path of a sidecar art file next to the
// song, or "" if none. Mirrors findSidecarThumb but for music.
func musicArtFor(songAbs string) string {
	stem := strings.TrimSuffix(songAbs, filepath.Ext(songAbs))
	for _, ext := range sidecarExts {
		p := stem + ext
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	// Fall back to a shared "cover.jpg" / "folder.jpg" in the album
	// directory — common for sideloaded music libraries.
	dir := filepath.Dir(songAbs)
	for _, name := range []string{"cover.jpg", "cover.jpeg", "cover.png", "folder.jpg", "folder.png"} {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func (a *App) musicArtCacheExists(songAbs string) bool {
	a.mu.RLock()
	root := a.musicDir
	a.mu.RUnlock()
	if root == "" {
		return false
	}
	return fileExists(filepath.Join(root, ".arts", thumbKey(songAbs)+".jpg"))
}

// MusicLibraryStats is a thin wrapper around (*MusicLibrary).Stats so
// the Accounts-tab stats panel can refresh without going through
// Status() (which does a lot more work).
type MusicLibraryStats struct {
	Artists    int     `json:"artists"`
	Albums     int     `json:"albums"`
	Songs      int     `json:"songs"`
	TotalSecs  float64 `json:"totalSecs"`
	TotalBytes int64   `json:"totalBytes"`
}

func (a *App) GetMusicStats() MusicLibraryStats {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.music == nil {
		return MusicLibraryStats{}
	}
	artists, albums, songs, dur, bytes := a.music.Stats()
	return MusicLibraryStats{
		Artists:    artists,
		Albums:     albums,
		Songs:      songs,
		TotalSecs:  dur,
		TotalBytes: bytes,
	}
}

// ---- Music CRUD (Editor only) --------------------------------------------

// DeleteSong moves a song's m4a + its sidecars (art + music video)
// to Music/.trash/.
func (a *App) DeleteSong(relPath string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	abs, err := a.absMusicPath(relPath)
	if err != nil {
		return err
	}
	a.mu.RLock()
	root := a.musicDir
	a.mu.RUnlock()
	if root == "" {
		return errors.New("music library not loaded")
	}
	if err := moveToTrash(root, abs); err != nil {
		return err
	}
	stem := strings.TrimSuffix(abs, filepath.Ext(abs))
	for _, ext := range append(sidecarExts, ".mp4") {
		side := stem + ext
		if _, err := os.Stat(side); err == nil {
			_ = moveToTrash(root, side)
		}
	}
	a.Rescan()
	return nil
}

// DeleteAlbum moves an entire album directory to .trash. The artist
// directory is left even if it becomes empty — the user might just
// want to add another album to that artist later.
func (a *App) DeleteAlbum(artist, album string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	a.mu.RLock()
	root := a.musicDir
	a.mu.RUnlock()
	if root == "" {
		return errors.New("music library not loaded")
	}
	src := filepath.Join(root, artist, album)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("album %q / %q not found", artist, album)
	}
	if err := moveToTrash(root, src); err != nil {
		return err
	}
	a.Rescan()
	return nil
}

// ---- Music download (yt-dlp from YouTube Music) --------------------------

// AddMusic downloads songs OR albums from YouTube / YouTube Music
// URLs into the Music/ tree. Single song URLs land in
// Music/<Channel>/Singles/Title.m4a (Singles is the synthetic album
// for non-album tracks). Album URLs (playlist links from YouTube
// Music) create Music/<Artist>/<Album>/##\ -\ Track.m4a.
//
// Audio is fetched as m4a (format 140) so no conversion is needed —
// matching the project's "no ffmpeg" stance. Album art is written
// as a sidecar via --write-thumbnail.
func (a *App) AddMusic(urls []string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	cap := a.GetEditCapability()
	if !cap.Enabled {
		return errors.New(cap.Reason)
	}
	a.mu.RLock()
	root := a.musicDir
	ctx := a.ctx
	a.mu.RUnlock()
	if root == "" {
		return errors.New("music library directory missing — create it next to Videos/")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}

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
	var firstErr error
	for i, url := range clean {
		wruntime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
			"current": i + 1, "total": total,
			"phase": "starting", "url": url,
		})

		isAlbum := looksLikePlaylistURL(url)
		artist, container, err := fetchMusicMeta(ctx, cap.YtDlpPath, url, isAlbum)
		if err != nil || artist == "" {
			artist = "Unknown Artist"
		}
		if container == "" {
			if isAlbum {
				container = "Untitled Album"
			} else {
				container = "Singles"
			}
		}
		albumDir := filepath.Join(root,
			sanitizeFolderName(artist),
			sanitizeFolderName(container))
		if err := os.MkdirAll(albumDir, 0o755); err != nil {
			wruntime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
				"current": i + 1, "total": total,
				"phase": "error", "url": url, "error": err.Error(),
			})
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		if err := runYtdlpAudio(ctx, cap.YtDlpPath, albumDir, url, isAlbum); err != nil {
			// Log the error per-URL but keep going so one bad URL
			// doesn't kill the rest of the batch (a common case is
			// one removed/region-locked song in an otherwise-fine
			// album of 20 tracks).
			wruntime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
				"current": i + 1, "total": total,
				"phase": "error", "url": url, "error": err.Error(),
			})
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		wruntime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
			"current": i + 1, "total": total,
			"phase": "done", "url": url,
		})
	}

	a.Rescan()
	wruntime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
		"phase": "all-done", "total": total,
	})
	// Return the first error AFTER rescanning + emitting all-done so
	// the UI sees the partial-success state. The frontend treats
	// non-nil error as "show this message" but the per-URL phase
	// events already showed which items failed.
	return firstErr
}

// fetchMusicMeta uses yt-dlp's --print to pull the YouTube Music
// metadata fields we need to place the file. For albums we pull the
// playlist title; for singles we pull the artist + track name.
// All errors fall back to the caller's defaults.
func fetchMusicMeta(ctx context.Context, ytdlp, url string, isAlbum bool) (artist, container string, err error) {
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := []string{
		"--quiet",
		"--no-warnings",
	}
	if isAlbum {
		args = append(args,
			"--flat-playlist",
			"--playlist-items", "1",
			"--print", "%(playlist_uploader,uploader,artist)s",
			"--print", "%(playlist_title,album)s",
			"--", url,
		)
	} else {
		args = append(args,
			"--no-playlist",
			"--print", "%(artist,uploader,creator)s",
			"--print", "%(album)s",
			"--", url,
		)
	}
	out, err := exec.CommandContext(c, ytdlp, args...).Output()
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) >= 1 && lines[0] != "" && lines[0] != "NA" {
		artist = lines[0]
	}
	if len(lines) >= 2 && lines[1] != "" && lines[1] != "NA" {
		container = lines[1]
	}
	return artist, container, nil
}

// runYtdlpAudio is the audio-only counterpart to runYtdlp. Forces
// format 140 (AAC m4a) so the output is a single self-contained file
// — no muxing needed, no ffmpeg postprocessing. Album art lands as a
// sidecar via --write-thumbnail.
func runYtdlpAudio(ctx context.Context, ytdlp, outDir, url string, playlist bool) error {
	template := filepath.Join(outDir, "%(track,title)s.%(ext)s")
	if playlist {
		template = filepath.Join(outDir, "%(playlist_index)02d - %(track,title)s.%(ext)s")
	}
	args := []string{
		"-o", template,
		// 140 = m4a AAC 128k. Fall back to bestaudio[ext=m4a] then
		// any best audio if 140 isn't offered (rare).
		"-f", "140/bestaudio[ext=m4a]/bestaudio",
		"--write-thumbnail",
		"--newline",
		"--progress",
		"--ignore-errors",
	}
	if playlist {
		args = append(args, "--yes-playlist")
	} else {
		args = append(args, "--no-playlist")
	}
	args = append(args, "--", url)

	wruntime.EventsEmit(ctx, "ytdlp-progress", map[string]any{
		"phase": "log", "url": url,
		"line": fmt.Sprintf("[ytdisc] exec: %s %s", ytdlp, strings.Join(args, " ")),
	})

	cmd := exec.CommandContext(ctx, ytdlp, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
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

// AddMusicVideo attaches a music video to an existing song. The
// video file is downloaded with the same H.264 + AAC mux pipeline
// as regular videos (separate streams + pure-Go muxer) and saved as
// a sidecar `<song-stem>.mp4` next to the song's m4a so the scanner
// detects HasMV automatically.
func (a *App) AddMusicVideo(songRelPath, url, quality string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	cap := a.GetEditCapability()
	if !cap.Enabled {
		return errors.New(cap.Reason)
	}
	songAbs, err := a.absMusicPath(songRelPath)
	if err != nil {
		return err
	}
	if !fileExists(songAbs) {
		return errors.New("song not found")
	}
	a.mu.RLock()
	ctx := a.ctx
	a.mu.RUnlock()

	dir := filepath.Dir(songAbs)
	stem := strings.TrimSuffix(filepath.Base(songAbs), filepath.Ext(songAbs))

	// Download into a tmp subdir so yt-dlp's title-derived filename
	// doesn't collide with the song's m4a or with future additions.
	// We rename the resulting .mp4 to <stem>.mp4 once it's on disk.
	tmp, err := os.MkdirTemp(dir, ".mvtmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	if err := runYtdlp(ctx, cap.YtDlpPath, tmp, url, quality, false); err != nil {
		return err
	}
	if err := mergeOrphanStreams(ctx, tmp); err != nil {
		return err
	}

	// Find the resulting .mp4 in tmp/ and move it to <stem>.mp4.
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	moved := false
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if strings.ToLower(filepath.Ext(e.Name())) != ".mp4" {
			continue
		}
		src := filepath.Join(tmp, e.Name())
		dst := filepath.Join(dir, stem+".mp4")
		if _, err := os.Stat(dst); err == nil {
			_ = os.Remove(dst) // overwrite existing music video
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
		moved = true
		break
	}
	if !moved {
		return errors.New("yt-dlp didn't produce an mp4")
	}
	a.Rescan()
	return nil
}

// RemoveMusicVideo deletes a song's attached music video (the .mp4
// sidecar). The song itself is untouched.
func (a *App) RemoveMusicVideo(songRelPath string) error {
	if err := a.requireEditor(); err != nil {
		return err
	}
	songAbs, err := a.absMusicPath(songRelPath)
	if err != nil {
		return err
	}
	a.mu.RLock()
	root := a.musicDir
	a.mu.RUnlock()
	stem := strings.TrimSuffix(songAbs, filepath.Ext(songAbs))
	mv := stem + ".mp4"
	if _, err := os.Stat(mv); err != nil {
		return errors.New("no music video attached")
	}
	if err := moveToTrash(root, mv); err != nil {
		return err
	}
	a.Rescan()
	return nil
}

// ---- Playlists (JS-callable) ---------------------------------------------

func (a *App) GetPlaylists() ([]Playlist, error) {
	id := a.accounts.currentID()
	if id == "" {
		return nil, errors.New("no active account")
	}
	return a.playlists.visibleTo(id)
}

func (a *App) CreatePlaylist(name, visibility string) (Playlist, error) {
	id := a.accounts.currentID()
	if id == "" {
		return Playlist{}, errors.New("no active account")
	}
	return a.playlists.create(id, name, PlaylistVisibility(visibility))
}

func (a *App) DeletePlaylist(id string) error {
	owner := a.accounts.currentID()
	if owner == "" {
		return errors.New("no active account")
	}
	return a.playlists.delete(owner, id)
}

func (a *App) RenamePlaylist(id, newName string) error {
	owner := a.accounts.currentID()
	if owner == "" {
		return errors.New("no active account")
	}
	return a.playlists.rename(owner, id, newName)
}

func (a *App) SetPlaylistVisibility(id, visibility string) error {
	owner := a.accounts.currentID()
	if owner == "" {
		return errors.New("no active account")
	}
	return a.playlists.setVisibility(owner, id, PlaylistVisibility(visibility))
}

func (a *App) AddSongToPlaylist(playlistID, songRelPath string) error {
	actor := a.accounts.currentID()
	if actor == "" {
		return errors.New("no active account")
	}
	return a.playlists.addSong(actor, playlistID, songRelPath)
}

func (a *App) RemoveSongFromPlaylist(playlistID, songRelPath string) error {
	actor := a.accounts.currentID()
	if actor == "" {
		return errors.New("no active account")
	}
	return a.playlists.removeSong(actor, playlistID, songRelPath)
}

func (a *App) ReorderPlaylistSongs(playlistID string, songs []string) error {
	actor := a.accounts.currentID()
	if actor == "" {
		return errors.New("no active account")
	}
	return a.playlists.reorderSongs(actor, playlistID, songs)
}

// ---- Pre-fetch download manifest (visual progress UI) --------------------

// FetchDownloadManifest does a `--flat-playlist --print` pass over a
// list of URLs to produce per-item titles BEFORE the real download
// starts, so the frontend can render a checkbox-style list with one
// row per item and update progress bars in place. Returns one
// ManifestEntry per URL — for playlist URLs the entry's Items[]
// holds every track in the playlist.
//
// This is best-effort; if metadata fetch fails for a URL we still
// return an entry with that URL as a single Item so the download
// loop has something to render.
func (a *App) FetchDownloadManifest(urls []string) []ManifestEntry {
	if err := a.requireEditor(); err != nil {
		return []ManifestEntry{}
	}
	cap := a.GetEditCapability()
	if !cap.YtDlpAvailable {
		return []ManifestEntry{}
	}
	out := make([]ManifestEntry, 0, len(urls))
	for _, url := range urls {
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		out = append(out, fetchManifestForURL(a.ctx, cap.YtDlpPath, url))
	}
	return out
}

type ManifestEntry struct {
	URL      string         `json:"url"`
	Kind     string         `json:"kind"` // "single" | "playlist" | "album"
	Title    string         `json:"title"`
	Items    []ManifestItem `json:"items"`
}

type ManifestItem struct {
	Title    string `json:"title"`
	Duration int    `json:"duration,omitempty"`
}

func fetchManifestForURL(ctx context.Context, ytdlp, url string) ManifestEntry {
	entry := ManifestEntry{URL: url, Kind: "single", Title: url}
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	if looksLikePlaylistURL(url) {
		entry.Kind = "playlist"
		// Get the playlist title + every entry's title.
		out, err := exec.CommandContext(c, ytdlp,
			"--quiet", "--no-warnings",
			"--flat-playlist",
			"--print", "playlist:%(playlist_title)s",
			"--print", "%(title)s",
			"--", url,
		).Output()
		if err != nil {
			return entry
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || line == "NA" {
				continue
			}
			if strings.HasPrefix(line, "playlist:") {
				entry.Title = strings.TrimPrefix(line, "playlist:")
				continue
			}
			entry.Items = append(entry.Items, ManifestItem{Title: line})
		}
		if entry.Title == "" {
			entry.Title = "Playlist"
		}
		return entry
	}

	out, err := exec.CommandContext(c, ytdlp,
		"--quiet", "--no-warnings",
		"--no-playlist",
		"--print", "%(title)s",
		"--", url,
	).Output()
	if err == nil {
		title := strings.TrimSpace(string(out))
		if title != "" && title != "NA" {
			entry.Title = title
		}
	}
	entry.Items = []ManifestItem{{Title: entry.Title}}
	return entry
}
