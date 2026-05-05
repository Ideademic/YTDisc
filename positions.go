package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Resume-playback persistence. Saved positions are keyed by the video's
// forward-slash relative path (Channel/[Folder/]File.ext) so they
// survive moving the library between machines (unlike the thumbnail
// cache, which is keyed by absolute path because it predates this).
//
// Persisted to Videos/.state.json. The file is small (one entry per
// in-progress video) so we just rewrite it on every Save — no
// background flush, no rotating writes. Loss on crash / force-quit is
// acceptable; per the product spec the worst case is the user starts
// a video from the beginning.

const positionsFile = ".state.json"

// positionEntry is one saved bookmark. Updated is a Unix timestamp,
// kept around purely for future use (e.g. pruning entries older than
// some window so the file doesn't grow unbounded across years).
type positionEntry struct {
	Pos     float64 `json:"pos"`
	Updated int64   `json:"updated"`
}

type positionsFileFormat struct {
	Positions map[string]positionEntry `json:"positions"`
}

type positionStore struct {
	mu        sync.Mutex
	libDir    string
	positions map[string]positionEntry
	loaded    bool
}

func newPositionStore() *positionStore {
	return &positionStore{positions: make(map[string]positionEntry)}
}

// setLibDir reloads from disk under the given library root. Called
// after the App locates the Videos/ directory at startup.
func (s *positionStore) setLibDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.libDir = dir
	s.positions = make(map[string]positionEntry)
	s.loaded = false
	s.loadLocked()
}

func (s *positionStore) loadLocked() {
	if s.libDir == "" || s.loaded {
		return
	}
	data, err := os.ReadFile(filepath.Join(s.libDir, positionsFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// First run — no file yet. Mark loaded so we don't keep
			// retrying on every Get; future Saves will create it.
			s.loaded = true
		}
		// Any other error (USB hiccup, permission glitch) leaves
		// loaded=false so the next access tries again — otherwise
		// we'd silently overwrite the on-disk file with an empty
		// in-memory map on the next Save.
		return
	}
	var f positionsFileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		// Corrupt file — leave the in-memory map empty AND mark
		// loaded so we don't keep failing the same parse on every
		// access. The next Save will overwrite the bad file.
		s.loaded = true
		return
	}
	if f.Positions != nil {
		s.positions = f.Positions
	}
	s.loaded = true
}

func (s *positionStore) get(relPath string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return s.positions[relPath].Pos
}

func (s *positionStore) set(relPath string, sec float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.libDir == "" {
		return errors.New("library not loaded")
	}
	s.loadLocked()
	s.positions[relPath] = positionEntry{Pos: sec, Updated: time.Now().Unix()}
	return s.persistLocked()
}

func (s *positionStore) clear(relPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.libDir == "" {
		return nil
	}
	s.loadLocked()
	if _, ok := s.positions[relPath]; !ok {
		return nil
	}
	delete(s.positions, relPath)
	return s.persistLocked()
}

// rename relocates a position entry — called from cache-migration paths
// when a video or its parent folder/channel is renamed or moved.
func (s *positionStore) rename(oldRel, newRel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.libDir == "" {
		return
	}
	s.loadLocked()
	if e, ok := s.positions[oldRel]; ok {
		delete(s.positions, oldRel)
		s.positions[newRel] = e
		_ = s.persistLocked()
	}
}

// persistLocked writes the current map back to disk. Caller must hold
// s.mu. Best-effort: failures (e.g. read-only USB) are returned but
// don't corrupt the in-memory map, so subsequent gets still work.
func (s *positionStore) persistLocked() error {
	out := positionsFileFormat{Positions: s.positions}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.libDir, positionsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	// Clean up the tmp file if Rename fails (e.g. USB suddenly read-only
	// or yanked) — otherwise we'd accumulate one tmp per failed save in
	// the user's library root.
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ---- JS-callable methods bound on App (defined in app.go) -----------------

// GetPosition returns the saved playback position in seconds, or 0 if
// none exists. The frontend decides whether to actually resume (e.g.
// it skips the resume when the saved position is in the first 45 s or
// the last 20 s of the video).
func (a *App) GetPosition(relPath string) float64 {
	if a.positions == nil {
		return 0
	}
	return a.positions.get(relPath)
}

// SavePosition records the current playback position for a video. The
// frontend calls this on a timer while playing, on close, and on
// beforeunload so the bookmark survives ⌘W and app quit.
func (a *App) SavePosition(relPath string, sec float64) error {
	if a.positions == nil {
		return errors.New("library not loaded")
	}
	return a.positions.set(relPath, sec)
}

// ClearPosition removes the saved position. The frontend calls this
// when a video plays to completion so the next play starts fresh.
func (a *App) ClearPosition(relPath string) error {
	if a.positions == nil {
		return nil
	}
	return a.positions.clear(relPath)
}
