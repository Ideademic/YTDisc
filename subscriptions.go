package main

// Per-account channel subscriptions for the Videos tab. Editor sees
// every channel regardless; other accounts only see channels they've
// subscribed to. The subscription list is the channel folder name
// (matching what Library.Channels uses), not a UUID — so subscriptions
// survive across drive moves and channel renames are handled by
// migrateSubscriptionsForChannelRename.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const subscriptionsFile = "subscriptions.json"

type subsFileFormat struct {
	Channels []string `json:"channels"`
}

type subscriptionStore struct {
	mu      sync.RWMutex
	loaded  map[string]map[string]bool // accountID → set of channel names
	storeFn func(accountID string) (string, error)
}

func newSubscriptionStore(stateDirFn func(string) (string, error)) *subscriptionStore {
	return &subscriptionStore{
		loaded:  make(map[string]map[string]bool),
		storeFn: stateDirFn,
	}
}

func (s *subscriptionStore) loadLocked(accountID string) (map[string]bool, error) {
	if set, ok := s.loaded[accountID]; ok {
		return set, nil
	}
	dir, err := s.storeFn(accountID)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool)
	data, err := os.ReadFile(filepath.Join(dir, subscriptionsFile))
	if err == nil {
		var f subsFileFormat
		if err := json.Unmarshal(data, &f); err == nil {
			for _, ch := range f.Channels {
				set[ch] = true
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	s.loaded[accountID] = set
	return set, nil
}

func (s *subscriptionStore) persistLocked(accountID string) error {
	set := s.loaded[accountID]
	if set == nil {
		return nil
	}
	dir, err := s.storeFn(accountID)
	if err != nil {
		return err
	}
	channels := make([]string, 0, len(set))
	for ch := range set {
		channels = append(channels, ch)
	}
	sort.Strings(channels)
	data, err := json.MarshalIndent(subsFileFormat{Channels: channels}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, subscriptionsFile)
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

// isSubscribed returns whether `channel` is in the account's
// subscription set. The Editor account is reported as subscribed-to-
// everything by callers via the IsEditor short-circuit; this method
// itself doesn't special-case Editor (the store wouldn't typically
// be queried for Editor at all).
func (s *subscriptionStore) isSubscribed(accountID, channel string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, err := s.loadLocked(accountID)
	if err != nil {
		return false
	}
	return set[channel]
}

func (s *subscriptionStore) subscribe(accountID, channel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, err := s.loadLocked(accountID)
	if err != nil {
		return err
	}
	if set[channel] {
		return nil
	}
	set[channel] = true
	return s.persistLocked(accountID)
}

func (s *subscriptionStore) unsubscribe(accountID, channel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, err := s.loadLocked(accountID)
	if err != nil {
		return err
	}
	if !set[channel] {
		return nil
	}
	delete(set, channel)
	return s.persistLocked(accountID)
}

// list returns a sorted snapshot of the account's subscribed channels.
func (s *subscriptionStore) list(accountID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, err := s.loadLocked(accountID)
	if err != nil || len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for ch := range set {
		out = append(out, ch)
	}
	sort.Strings(out)
	return out
}

// renameChannel rewrites every account's subscription set so that
// references to oldName become newName. Walks the on-disk
// .data/accounts/<id>/ tree (not just the in-memory loaded cache)
// so cold accounts that haven't been opened this session still get
// their stale reference rewritten. Called from RenameChannel
// (Editor-only op).
func (s *subscriptionStore) renameChannel(oldName, newName string) {
	s.walkAllSubsFiles(func(set map[string]bool) (mutated bool) {
		if set[oldName] {
			delete(set, oldName)
			set[newName] = true
			return true
		}
		return false
	})
}

// removeChannel drops a channel from every account's set on disk
// (called when a channel is deleted). Cold-account-aware just like
// renameChannel.
func (s *subscriptionStore) removeChannel(channel string) {
	s.walkAllSubsFiles(func(set map[string]bool) (mutated bool) {
		if set[channel] {
			delete(set, channel)
			return true
		}
		return false
	})
}

// walkAllSubsFiles iterates every account directory under .data/accounts/
// (or the in-memory loaded cache for accounts we already have in
// memory, to avoid stale-read races) and applies `mutator` to each
// account's subscription set. Persists per-account if mutator
// returns true. The point is to NOT depend on `loaded` being
// populated for every account — channel rename / delete must affect
// every account, even ones that haven't been logged into this
// session.
func (s *subscriptionStore) walkAllSubsFiles(mutator func(map[string]bool) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// First, mutate every in-memory set so the running session sees
	// the change immediately.
	for accountID, set := range s.loaded {
		if mutator(set) {
			_ = s.persistLocked(accountID)
		}
	}
	// Then walk the disk for accounts NOT in s.loaded.
	if s.storeFn == nil {
		return
	}
	// Use any cached account's stateDir to find the parent
	// .data/accounts/ dir. If no accounts are loaded yet, ask
	// storeFn for a synthetic account ID — it'll create+return a
	// path; we use the parent.
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
		if _, ok := s.loaded[id]; ok {
			continue // already handled above
		}
		path := filepath.Join(parentDir, id, subscriptionsFile)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var f subsFileFormat
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		set := make(map[string]bool, len(f.Channels))
		for _, ch := range f.Channels {
			set[ch] = true
		}
		if mutator(set) {
			// Reload the cold account's set into our cache so the
			// persist call writes the right thing.
			s.loaded[id] = set
			_ = s.persistLocked(id)
		}
	}
}
