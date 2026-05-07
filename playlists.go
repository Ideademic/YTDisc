package main

// Music playlists. One file per playlist at .data/playlists/<id>.json
// (not under accounts/<id>/) so shared playlists are discoverable
// without scanning every account directory. Each file carries the
// owner's account ID + a visibility level:
//
//   private        — only the owner sees it
//   shared-r       — everyone sees it; only the owner can edit
//   shared-rw      — everyone sees AND can add/remove songs (read-write
//                    collaborative); only the owner can delete
//
// Playlist songs are referenced by their forward-slash RelPath under
// the Music/ root (matching SongInfo.RelPath). Renames of song files
// don't update playlist references — that's a future concern; for
// now the UI just shows missing entries as inert "song not found".

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const playlistsDir = "playlists"

// PlaylistVisibility — three values.
type PlaylistVisibility string

const (
	PlaylistPrivate   PlaylistVisibility = "private"
	PlaylistSharedRO  PlaylistVisibility = "shared-r"
	PlaylistSharedRW  PlaylistVisibility = "shared-rw"
)

func validVisibility(v PlaylistVisibility) bool {
	return v == PlaylistPrivate || v == PlaylistSharedRO || v == PlaylistSharedRW
}

type Playlist struct {
	ID         string             `json:"id"`
	OwnerID    string             `json:"ownerId"`
	Name       string             `json:"name"`
	Visibility PlaylistVisibility `json:"visibility"`
	Songs      []string           `json:"songs"` // forward-slash RelPaths under Music/
	CreatedAt  int64              `json:"createdAt"`
	UpdatedAt  int64              `json:"updatedAt"`
}

type playlistStore struct {
	mu      sync.RWMutex
	dataDir string // .data/
}

func newPlaylistStore() *playlistStore { return &playlistStore{} }

func (s *playlistStore) setDataDir(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dataDir = dir
	if dir == "" {
		return nil
	}
	return os.MkdirAll(filepath.Join(dir, playlistsDir), 0o755)
}

func (s *playlistStore) dir() string {
	return filepath.Join(s.dataDir, playlistsDir)
}

func (s *playlistStore) load(id string) (Playlist, error) {
	data, err := os.ReadFile(filepath.Join(s.dir(), id+".json"))
	if err != nil {
		return Playlist{}, err
	}
	var p Playlist
	if err := json.Unmarshal(data, &p); err != nil {
		return Playlist{}, err
	}
	return p, nil
}

func (s *playlistStore) save(p Playlist) error {
	p.UpdatedAt = time.Now().Unix()
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir(), p.ID+".json")
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

// listAll reads every playlist file. The caller is expected to filter
// by visibility + ownership for the current account.
func (s *playlistStore) listAll() ([]Playlist, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dataDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(s.dir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Playlist, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if p, err := s.load(id); err == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// visibleTo returns the subset of playlists the given account is
// allowed to see (their own + every shared one).
func (s *playlistStore) visibleTo(accountID string) ([]Playlist, error) {
	all, err := s.listAll()
	if err != nil {
		return nil, err
	}
	out := make([]Playlist, 0, len(all))
	for _, p := range all {
		switch {
		case p.OwnerID == accountID:
			out = append(out, p)
		case p.Visibility == PlaylistSharedRO || p.Visibility == PlaylistSharedRW:
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *playlistStore) create(ownerID, name string, vis PlaylistVisibility) (Playlist, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Playlist{}, errors.New("playlist name cannot be empty")
	}
	if !validVisibility(vis) {
		return Playlist{}, fmt.Errorf("invalid visibility %q", vis)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := newPlaylistID()
	if err != nil {
		return Playlist{}, err
	}
	now := time.Now().Unix()
	p := Playlist{
		ID:         id,
		OwnerID:    ownerID,
		Name:       name,
		Visibility: vis,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.save(p); err != nil {
		return Playlist{}, err
	}
	return p, nil
}

func (s *playlistStore) delete(actorID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.load(id)
	if err != nil {
		return err
	}
	if p.OwnerID != actorID {
		return errors.New("only the owner can delete this playlist")
	}
	return os.Remove(filepath.Join(s.dir(), id+".json"))
}

// rename, retitle, etc.
func (s *playlistStore) rename(actorID, id, newName string) error {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return errors.New("playlist name cannot be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.load(id)
	if err != nil {
		return err
	}
	if p.OwnerID != actorID {
		return errors.New("only the owner can rename this playlist")
	}
	p.Name = newName
	return s.save(p)
}

func (s *playlistStore) setVisibility(actorID, id string, vis PlaylistVisibility) error {
	if !validVisibility(vis) {
		return fmt.Errorf("invalid visibility %q", vis)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.load(id)
	if err != nil {
		return err
	}
	if p.OwnerID != actorID {
		return errors.New("only the owner can change visibility")
	}
	p.Visibility = vis
	return s.save(p)
}

// canEdit returns whether actorID is allowed to add/remove songs to
// this playlist. Owners always can; non-owners can only edit when
// visibility is shared-rw.
func (s *playlistStore) canEdit(actorID string, p Playlist) bool {
	if p.OwnerID == actorID {
		return true
	}
	return p.Visibility == PlaylistSharedRW
}

func (s *playlistStore) addSong(actorID, id, songRelPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.load(id)
	if err != nil {
		return err
	}
	if !s.canEdit(actorID, p) {
		return errors.New("you don't have permission to edit this playlist")
	}
	for _, existing := range p.Songs {
		if existing == songRelPath {
			return nil
		}
	}
	p.Songs = append(p.Songs, songRelPath)
	return s.save(p)
}

func (s *playlistStore) removeSong(actorID, id, songRelPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.load(id)
	if err != nil {
		return err
	}
	if !s.canEdit(actorID, p) {
		return errors.New("you don't have permission to edit this playlist")
	}
	out := p.Songs[:0]
	for _, sng := range p.Songs {
		if sng != songRelPath {
			out = append(out, sng)
		}
	}
	p.Songs = out
	return s.save(p)
}

// reorderSongs sets the playlist's full song order. Called when the
// user drags songs around in the playlist view.
func (s *playlistStore) reorderSongs(actorID, id string, songs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.load(id)
	if err != nil {
		return err
	}
	if !s.canEdit(actorID, p) {
		return errors.New("you don't have permission to edit this playlist")
	}
	p.Songs = songs
	return s.save(p)
}

func newPlaylistID() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
