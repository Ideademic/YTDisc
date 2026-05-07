package main

// MusicLibrary is the in-memory model of `Music/<Artist>/<Album>/`.
// Each leaf is an .m4a song; it can have these sidecars:
//
//   - {stem}.{jpg|jpeg|png|webp}   — album art / song cover
//   - {stem}.mp4                   — attached music video
//
// One artist can have many albums; one album many songs. Like videos,
// folder nesting is exactly two levels deep (Artist → Album → Song);
// scanFolder doesn't recurse further.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Eyevinn/mp4ff/mp4"
)

var musicAudioExts = map[string]bool{
	".m4a": true,
	".mp3": true, // accepted for sideloaded files, even though we don't download mp3
}

// MusicLibrary mirrors Library but for the Music/ tree.
type MusicLibrary struct {
	Root    string
	Artists []*Artist
}

type Artist struct {
	Name   string
	Path   string // absolute
	Albums []*Album
}

type Album struct {
	Name       string
	Path       string // absolute
	ArtistName string
	Songs      []*Song
}

type Song struct {
	Title       string
	Artist      string
	Album       string
	AbsPath     string
	RelPath     string  // forward-slash, "Artist/Album/Song.m4a"
	DurationSec float64
	SizeBytes   int64
	HasMV       bool // a sibling Title.mp4 file exists
}

func ScanMusicLibrary(root string) *MusicLibrary {
	lib := &MusicLibrary{Root: root}
	entries, err := os.ReadDir(root)
	if err != nil {
		return lib
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		lib.Artists = append(lib.Artists, scanArtist(root, e.Name()))
	}
	sort.Slice(lib.Artists, func(i, j int) bool {
		return strings.ToLower(lib.Artists[i].Name) < strings.ToLower(lib.Artists[j].Name)
	})
	return lib
}

func scanArtist(root, name string) *Artist {
	dir := filepath.Join(root, name)
	a := &Artist{Name: name, Path: dir}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return a
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		a.Albums = append(a.Albums, scanAlbum(dir, name, e.Name()))
	}
	sort.Slice(a.Albums, func(i, j int) bool {
		return strings.ToLower(a.Albums[i].Name) < strings.ToLower(a.Albums[j].Name)
	})
	return a
}

func scanAlbum(artistDir, artistName, albumName string) *Album {
	dir := filepath.Join(artistDir, albumName)
	al := &Album{Name: albumName, Path: dir, ArtistName: artistName}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return al
	}
	// Build a set of stems with .mp4 sidecars so HasMV can be set
	// without a second os.Stat per song.
	mvStems := make(map[string]bool)
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if strings.ToLower(filepath.Ext(e.Name())) == ".mp4" {
			mvStems[strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))] = true
		}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fname := e.Name()
		if strings.HasPrefix(fname, ".") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(fname))
		if !musicAudioExts[ext] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		abs := filepath.Join(dir, fname)
		stem := strings.TrimSuffix(fname, filepath.Ext(fname))
		s := &Song{
			Title:     stem,
			Artist:    artistName,
			Album:     albumName,
			AbsPath:   abs,
			RelPath:   filepath.ToSlash(filepath.Join(artistName, albumName, fname)),
			SizeBytes: info.Size(),
			HasMV:     mvStems[stem],
		}
		// Best-effort duration read. m4a is an MP4 container so the
		// same mp4ff path that handles videos works here. mp3 is
		// outside our happy path (no parser); duration stays 0.
		if ext == ".m4a" {
			if dur, _, _, err := readMP4Meta(abs); err == nil {
				s.DurationSec = dur
			}
		}
		al.Songs = append(al.Songs, s)
	}
	sort.Slice(al.Songs, func(i, j int) bool {
		return strings.ToLower(al.Songs[i].Title) < strings.ToLower(al.Songs[j].Title)
	})
	return al
}

// allSongs returns every song in the library; used for aggregation.
func (l *MusicLibrary) allSongs() []*Song {
	if l == nil {
		return nil
	}
	out := make([]*Song, 0)
	for _, a := range l.Artists {
		for _, al := range a.Albums {
			out = append(out, al.Songs...)
		}
	}
	return out
}

func (l *MusicLibrary) ArtistByName(name string) *Artist {
	for _, a := range l.Artists {
		if a.Name == name {
			return a
		}
	}
	return nil
}

func (a *Artist) AlbumByName(name string) *Album {
	for _, al := range a.Albums {
		if al.Name == name {
			return al
		}
	}
	return nil
}

// ---- DTOs (frontend) -----------------------------------------------------

type ArtistInfo struct {
	Name       string       `json:"name"`
	AlbumCount int          `json:"albumCount"`
	SongCount  int          `json:"songCount"`
	Albums     []AlbumInfo  `json:"albums"`
}

type AlbumInfo struct {
	Name       string  `json:"name"`
	Artist     string  `json:"artist"`
	SongCount  int     `json:"songCount"`
	TotalSecs  float64 `json:"totalSecs"`
}

type SongInfo struct {
	Title       string  `json:"title"`
	Artist      string  `json:"artist"`
	Album       string  `json:"album"`
	RelPath     string  `json:"relPath"`
	DurationSec float64 `json:"durationSec"`
	SizeBytes   int64   `json:"sizeBytes"`
	HasMV       bool    `json:"hasMV"`
}

func toArtistInfo(a *Artist) ArtistInfo {
	ai := ArtistInfo{Name: a.Name}
	for _, al := range a.Albums {
		ai.AlbumCount++
		ai.SongCount += len(al.Songs)
		ai.Albums = append(ai.Albums, AlbumInfo{
			Name:      al.Name,
			Artist:    al.ArtistName,
			SongCount: len(al.Songs),
			TotalSecs: albumDuration(al),
		})
	}
	return ai
}

func toSongInfo(s *Song) SongInfo {
	return SongInfo{
		Title:       s.Title,
		Artist:      s.Artist,
		Album:       s.Album,
		RelPath:     s.RelPath,
		DurationSec: s.DurationSec,
		SizeBytes:   s.SizeBytes,
		HasMV:       s.HasMV,
	}
}

func albumDuration(al *Album) float64 {
	var n float64
	for _, s := range al.Songs {
		n += s.DurationSec
	}
	return n
}

// totalSize / totalDuration / totalCounts for the stats view.
func (l *MusicLibrary) Stats() (artists, albums, songs int, dur float64, bytes int64) {
	if l == nil {
		return
	}
	artists = len(l.Artists)
	for _, a := range l.Artists {
		albums += len(a.Albums)
		for _, al := range a.Albums {
			for _, s := range al.Songs {
				songs++
				dur += s.DurationSec
				bytes += s.SizeBytes
			}
		}
	}
	return
}

// readM4ADuration is a helper exposed for tests / future use.
// Currently scanAlbum calls readMP4Meta directly.
func readM4ADuration(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	parsed, err := mp4.DecodeFile(f, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		return 0, err
	}
	if parsed.Moov == nil || parsed.Moov.Mvhd == nil || parsed.Moov.Mvhd.Timescale == 0 {
		return 0, fmt.Errorf("no moov/mvhd in %s", path)
	}
	return float64(parsed.Moov.Mvhd.Duration) / float64(parsed.Moov.Mvhd.Timescale), nil
}
