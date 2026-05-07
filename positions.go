package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Resume-playback persistence. Saved positions are keyed by the
// video's forward-slash relative path under Videos/ (Channel/[Folder/]
// File.ext), and stored per-account at .data/accounts/<id>/positions.json
// so each account has their own watch progress.

const positionsFile = "positions.json"

type positionEntry struct {
	Pos     float64 `json:"pos"`
	Updated int64   `json:"updated"`
}

type positionsFileFormat struct {
	Positions map[string]positionEntry `json:"positions"`
}

type positionStore struct {
	mu      sync.Mutex
	storeFn func(accountID string) (string, error)
	cache   map[string]map[string]positionEntry // accountID → relPath → entry
	loaded  map[string]bool                     // accountID → has loaded?
}

func newPositionStore(stateDirFn func(string) (string, error)) *positionStore {
	return &positionStore{
		storeFn: stateDirFn,
		cache:   make(map[string]map[string]positionEntry),
		loaded:  make(map[string]bool),
	}
}

func (s *positionStore) loadLocked(accountID string) (map[string]positionEntry, error) {
	if s.loaded[accountID] {
		return s.cache[accountID], nil
	}
	dir, err := s.storeFn(accountID)
	if err != nil {
		return nil, err
	}
	m := make(map[string]positionEntry)
	data, err := os.ReadFile(filepath.Join(dir, positionsFile))
	if err == nil {
		var f positionsFileFormat
		if err := json.Unmarshal(data, &f); err == nil && f.Positions != nil {
			m = f.Positions
		}
		s.loaded[accountID] = true
	} else if errors.Is(err, os.ErrNotExist) {
		s.loaded[accountID] = true
	} else {
		return nil, err
	}
	s.cache[accountID] = m
	return m, nil
}

func (s *positionStore) persistLocked(accountID string) error {
	m := s.cache[accountID]
	if m == nil {
		return nil
	}
	dir, err := s.storeFn(accountID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(positionsFileFormat{Positions: m}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, positionsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *positionStore) get(accountID, relPath string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadLocked(accountID)
	if err != nil {
		return 0
	}
	return m[relPath].Pos
}

func (s *positionStore) set(accountID, relPath string, sec float64) error {
	if accountID == "" {
		return errors.New("no current account")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadLocked(accountID)
	if err != nil {
		return err
	}
	m[relPath] = positionEntry{Pos: sec, Updated: time.Now().Unix()}
	return s.persistLocked(accountID)
}

func (s *positionStore) clear(accountID, relPath string) error {
	if accountID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadLocked(accountID)
	if err != nil {
		return err
	}
	if _, ok := m[relPath]; !ok {
		return nil
	}
	delete(m, relPath)
	return s.persistLocked(accountID)
}

// renameAcrossAccounts updates every account's bookmark map (loaded
// + cold) so `oldRel` is rebound to `newRel`. Walks disk to handle
// accounts not yet loaded into memory this session, so a video
// rename never leaves stale bookmarks anywhere.
func (s *positionStore) renameAcrossAccounts(oldRel, newRel string) {
	s.walkAllPositionsFiles(func(m map[string]positionEntry) bool {
		if e, ok := m[oldRel]; ok {
			delete(m, oldRel)
			m[newRel] = e
			return true
		}
		return false
	})
}

// clearAcrossAccounts drops `relPath` from every account's map
// (loaded + cold). Called from DeleteVideo.
func (s *positionStore) clearAcrossAccounts(relPath string) {
	s.walkAllPositionsFiles(func(m map[string]positionEntry) bool {
		if _, ok := m[relPath]; ok {
			delete(m, relPath)
			return true
		}
		return false
	})
}

// walkAllPositionsFiles is the positionStore equivalent of
// subscriptionStore.walkAllSubsFiles. See that method for the
// rationale: channel/video changes must affect every account, even
// ones that haven't been logged into this session.
func (s *positionStore) walkAllPositionsFiles(mutator func(map[string]positionEntry) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for accountID, m := range s.cache {
		if mutator(m) {
			_ = s.persistLocked(accountID)
		}
	}
	if s.storeFn == nil {
		return
	}
	probeDir, err := s.storeFn("__probe")
	if err != nil {
		return
	}
	parentDir := filepath.Dir(probeDir)
	_ = os.RemoveAll(probeDir)
	entries, err := os.ReadDir(parentDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if _, ok := s.cache[id]; ok {
			continue
		}
		path := filepath.Join(parentDir, id, positionsFile)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var f positionsFileFormat
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		m := f.Positions
		if m == nil {
			continue
		}
		if mutator(m) {
			s.cache[id] = m
			s.loaded[id] = true
			_ = s.persistLocked(id)
		}
	}
}

// importLegacy seeds the given account's bookmarks with positions
// from a v1-style flat positions map. Used during the v1→v2 drive
// upgrade when the first account is created.
func (s *positionStore) importLegacy(accountID string, legacy map[string]positionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadLocked(accountID)
	if err != nil {
		return err
	}
	for k, v := range legacy {
		// Don't overwrite anything the new account already had — but
		// during a fresh upgrade there's nothing yet.
		if _, exists := m[k]; !exists {
			m[k] = v
		}
	}
	return s.persistLocked(accountID)
}
