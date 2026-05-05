package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Eyevinn/mp4ff/mp4"
)

var videoExts = map[string]bool{
	".mp4": true,
	".m4v": true,
	".mov": true,
}

// Library is the in-memory model of the Videos/ folder.
type Library struct {
	Root     string
	Channels []*Channel
}

// Channel represents one folder directly under Videos/. Videos is the
// list of videos at the channel root (not inside any subfolder); Folders
// are the subfolders, each holding their own videos. Folder nesting is
// only one level deep — folders never contain folders.
type Channel struct {
	Name    string
	Path    string // absolute
	Videos  []*Video
	Folders []*Folder
}

// Folder is one level of grouping inside a channel. Folder.Videos hold
// videos whose RelPath is "Channel/Folder/file.ext".
type Folder struct {
	Name   string
	Path   string // absolute
	Videos []*Video
}

type Video struct {
	Title       string // filename without extension
	Channel     string
	Folder      string  // empty if at channel root
	AbsPath     string
	RelPath     string  // path relative to Library.Root, native separator
	DurationSec float64
	Width       int
	Height      int
	SizeBytes   int64
}

// ---- find Videos/ ---------------------------------------------------------

// findVideosDir locates the Videos/ folder relative to the running binary,
// handling the Mac .app bundle layout and Linux AppImage. Search order:
//
//  1. $APPIMAGE's parent directory          (Linux AppImage)
//  2. The directory containing the binary   (and walking up to 5 levels)
//  3. The current working directory         (last resort)
func findVideosDir() (string, error) {
	check := func(dir string) (string, bool) {
		if dir == "" {
			return "", false
		}
		candidate := filepath.Join(dir, "Videos")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, true
		}
		return "", false
	}

	if appimage := os.Getenv("APPIMAGE"); appimage != "" {
		if p, ok := check(filepath.Dir(appimage)); ok {
			return p, nil
		}
	}

	exe, err := os.Executable()
	if err == nil {
		if resolved, err2 := filepath.EvalSymlinks(exe); err2 == nil {
			exe = resolved
		}
		dir := filepath.Dir(exe)
		for i := 0; i < 5; i++ {
			if p, ok := check(dir); ok {
				return p, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	if cwd, err := os.Getwd(); err == nil {
		if p, ok := check(cwd); ok {
			return p, nil
		}
	}

	return "", errors.New("no Videos/ folder found near the binary")
}

// ---- scan -----------------------------------------------------------------

// ScanLibrary walks `root`, expecting `root/<Channel>/<Video>.mp4` plus
// optionally `root/<Channel>/<Folder>/<Video>.mp4` (one level of folder
// nesting only — folders never contain folders). Channels, folders, and
// videos are all sorted alphabetically; folders interleave with videos
// in the channel listing rather than grouping at the top.
func ScanLibrary(root string) *Library {
	lib := &Library{Root: root}

	entries, err := os.ReadDir(root)
	if err != nil {
		return lib
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip hidden / cache folders.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		ch := scanChannel(root, e.Name())
		// Include empty channels too — otherwise a freshly-created
		// channel folder is invisible until it has at least one video,
		// which makes "create channel, then add videos to it" impossible.
		lib.Channels = append(lib.Channels, ch)
	}
	sort.Slice(lib.Channels, func(i, j int) bool {
		return strings.ToLower(lib.Channels[i].Name) < strings.ToLower(lib.Channels[j].Name)
	})
	return lib
}

func scanChannel(root, name string) *Channel {
	chDir := filepath.Join(root, name)
	ch := &Channel{Name: name, Path: chDir}

	entries, err := os.ReadDir(chDir)
	if err != nil {
		return ch
	}
	for _, e := range entries {
		fname := e.Name()
		// Skip hidden everywhere — macOS resource forks, .DS_Store,
		// Thumbs.db, our own .thumbs/.trash/.state, etc.
		if strings.HasPrefix(fname, ".") {
			continue
		}
		if e.IsDir() {
			ch.Folders = append(ch.Folders, scanFolder(chDir, name, fname))
			continue
		}
		if v := readVideoEntry(chDir, name, "", fname, e); v != nil {
			ch.Videos = append(ch.Videos, v)
		}
	}
	sort.Slice(ch.Videos, func(i, j int) bool {
		return strings.ToLower(ch.Videos[i].Title) < strings.ToLower(ch.Videos[j].Title)
	})
	sort.Slice(ch.Folders, func(i, j int) bool {
		return strings.ToLower(ch.Folders[i].Name) < strings.ToLower(ch.Folders[j].Name)
	})
	return ch
}

func scanFolder(chDir, channel, folder string) *Folder {
	fDir := filepath.Join(chDir, folder)
	f := &Folder{Name: folder, Path: fDir}

	entries, err := os.ReadDir(fDir)
	if err != nil {
		return f
	}
	for _, e := range entries {
		fname := e.Name()
		if strings.HasPrefix(fname, ".") {
			continue
		}
		// Folders inside folders are not part of the model — skip them
		// silently rather than recursing. The user-facing rule is "one
		// level of folders" and we enforce it here at scan time.
		if e.IsDir() {
			continue
		}
		if v := readVideoEntry(fDir, channel, folder, fname, e); v != nil {
			f.Videos = append(f.Videos, v)
		}
	}
	sort.Slice(f.Videos, func(i, j int) bool {
		return strings.ToLower(f.Videos[i].Title) < strings.ToLower(f.Videos[j].Title)
	})
	return f
}

// readVideoEntry constructs a Video from a directory entry, or returns
// nil if the entry isn't a recognized video file. dir is the absolute
// directory containing fname; channel is the channel folder name;
// folder is the subfolder name (empty string if at channel root).
func readVideoEntry(dir, channel, folder, fname string, e os.DirEntry) *Video {
	ext := strings.ToLower(filepath.Ext(fname))
	if !videoExts[ext] {
		return nil
	}
	info, err := e.Info()
	if err != nil {
		return nil
	}
	abs := filepath.Join(dir, fname)
	rel := filepath.Join(channel, fname)
	if folder != "" {
		rel = filepath.Join(channel, folder, fname)
	}
	v := &Video{
		Title:     strings.TrimSuffix(fname, filepath.Ext(fname)),
		Channel:   channel,
		Folder:    folder,
		AbsPath:   abs,
		RelPath:   rel,
		SizeBytes: info.Size(),
	}
	if dur, w, h, err := readMP4Meta(abs); err == nil {
		v.DurationSec = dur
		v.Width = w
		v.Height = h
	}
	return v
}

func (l *Library) ChannelByName(name string) *Channel {
	for _, c := range l.Channels {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (c *Channel) FolderByName(name string) *Folder {
	for _, f := range c.Folders {
		if f.Name == name {
			return f
		}
	}
	return nil
}

func (l *Library) TotalVideos() int {
	n := 0
	for _, c := range l.Channels {
		n += c.totalVideoCount()
	}
	return n
}

func (l *Library) TotalBytes() int64 {
	var n int64
	for _, c := range l.Channels {
		for _, v := range c.allVideos() {
			n += v.SizeBytes
		}
	}
	return n
}

func (l *Library) TotalSeconds() float64 {
	var n float64
	for _, c := range l.Channels {
		n += c.TotalSeconds()
	}
	return n
}

// allVideos returns every video in the channel, root-level and inside
// folders. Order is undefined — used for aggregations only.
func (c *Channel) allVideos() []*Video {
	out := make([]*Video, 0, len(c.Videos))
	out = append(out, c.Videos...)
	for _, f := range c.Folders {
		out = append(out, f.Videos...)
	}
	return out
}

func (c *Channel) totalVideoCount() int {
	n := len(c.Videos)
	for _, f := range c.Folders {
		n += len(f.Videos)
	}
	return n
}

func (c *Channel) TotalSeconds() float64 {
	var n float64
	for _, v := range c.allVideos() {
		n += v.DurationSec
	}
	return n
}

func (f *Folder) TotalSeconds() float64 {
	var n float64
	for _, v := range f.Videos {
		n += v.DurationSec
	}
	return n
}

// ---- MP4 metadata ---------------------------------------------------------

// readMP4Meta extracts duration and video dimensions from an MP4 file by
// reading just the moov atom (no codec decoding). Cheap and pure-Go.
func readMP4Meta(path string) (durationSec float64, width, height int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()

	// Lazy mdat decoding skips reading the actual sample data — we only
	// want the moov box, which is small and contains all metadata.
	parsed, err := mp4.DecodeFile(f, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		return 0, 0, 0, err
	}

	moov := parsed.Moov
	if moov == nil || moov.Mvhd == nil {
		return 0, 0, 0, fmt.Errorf("no moov/mvhd in %s", path)
	}

	if moov.Mvhd.Timescale != 0 {
		durationSec = float64(moov.Mvhd.Duration) / float64(moov.Mvhd.Timescale)
	}

	for _, trak := range moov.Traks {
		if trak.Mdia == nil || trak.Mdia.Hdlr == nil {
			continue
		}
		if trak.Mdia.Hdlr.HandlerType != "vide" {
			continue
		}
		// Tkhd width/height are 16.16 fixed-point.
		if trak.Tkhd != nil {
			width = int(trak.Tkhd.Width >> 16)
			height = int(trak.Tkhd.Height >> 16)
		}
		break
	}

	return durationSec, width, height, nil
}