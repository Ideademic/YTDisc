package main

// Accounts let multiple people share a drive with separate per-account
// state (resume positions, music playlists, channel subscriptions, the
// last-active tab). Persisted in `.data/accounts.json` at the drive
// root, alongside `.data/accounts/<id>/` directories for that
// account's per-account state files.
//
// One account is always the special "Editor" — a built-in singleton
// that sees every channel and is the only account that can mutate the
// library (add/rename/delete content, change thumbnails, etc.). The
// app refuses to persist Editor as the last-used account, so the next
// launch always falls back to whichever real account was last active.

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

const (
	// EditorAccountID is the fixed ID for the built-in Editor account.
	// It's spelled out (not a UUID) so it's recognizable on disk.
	EditorAccountID = "editor"

	// accountsFile lives at .data/accounts.json. The file format is
	// versioned so future schema changes can be detected without
	// guessing.
	accountsFile = "accounts.json"

	// AccountsSchemaVersion is the format we currently write. Reads
	// reject anything higher (so future v3 drives don't silently lose
	// fields when opened by an older v2).
	AccountsSchemaVersion = 2
)

// Account is the persistence model. UI maps it to AccountInfo for the
// frontend.
type Account struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	ColorA    string `json:"colorA"` // CSS hex like "#ff7788"
	ColorB    string `json:"colorB"` // CSS hex
	Angle     int    `json:"angle"`  // conic-gradient angle 0–359
	IsEditor  bool   `json:"isEditor"`
	LastTab   string `json:"lastTab"` // "accounts" | "videos" | "music"
	CreatedAt int64  `json:"createdAt"`
}

type accountsFileFormat struct {
	Version           int       `json:"version"`
	Accounts          []Account `json:"accounts"`
	LastUsedAccountID string    `json:"lastUsedAccountId,omitempty"`
}

// accountStore is the in-memory + on-disk model of accounts.json.
// All fields guarded by mu.
type accountStore struct {
	mu                sync.RWMutex
	dataDir           string
	loaded            bool
	accounts          []Account // includes Editor as accounts[0]
	lastUsedAccountID string    // never set to EditorAccountID
	currentAccountID  string    // in-memory only — which account is active *this session*
}

func newAccountStore() *accountStore { return &accountStore{} }

// setDataDir attaches the store to a `.data/` directory and reloads
// accounts.json. Creates the directory if missing.
func (s *accountStore) setDataDir(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dataDir = dir
	s.loaded = false
	s.accounts = nil
	s.lastUsedAccountID = ""
	s.currentAccountID = ""
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "accounts"), 0o755); err != nil {
		return err
	}
	if err := s.loadLocked(); err != nil {
		return err
	}
	return nil
}

// ErrAccountsFileCorrupt signals that accounts.json existed but
// failed to parse or version-validate. The boot path catches this
// and shows a "data-corrupt" state instead of silently clobbering
// the file on the next account-create.
var ErrAccountsFileCorrupt = errors.New("accounts.json is corrupt")

func (s *accountStore) loadLocked() error {
	if s.loaded || s.dataDir == "" {
		return nil
	}
	path := filepath.Join(s.dataDir, accountsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// First boot of a v2 drive — nothing to load. The Editor
			// account is materialized on demand by ensureEditorLocked.
			s.ensureEditorLocked()
			s.loaded = true
			return nil
		}
		return err
	}
	var f accountsFileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		// Corrupt file — return a sentinel error so the boot layer
		// can surface a recovery prompt. Crucially we do NOT mark
		// loaded=true here, AND we leave s.accounts empty: any later
		// persist would otherwise blow away the corrupt file's
		// contents (which the user might still want to recover by
		// hand).
		return fmt.Errorf("%w: %v", ErrAccountsFileCorrupt, err)
	}
	if f.Version > AccountsSchemaVersion {
		return fmt.Errorf("%s schema v%d is newer than supported v%d — upgrade YTDisc",
			accountsFile, f.Version, AccountsSchemaVersion)
	}
	s.accounts = f.Accounts
	s.lastUsedAccountID = f.LastUsedAccountID
	s.ensureEditorLocked()
	s.loaded = true
	return nil
}

// IsCorrupt returns whether the last load attempt found a corrupt
// accounts.json. Used by the boot layer.
func (s *accountStore) IsCorrupt() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// loaded == false AND dataDir set means the load failed.
	return s.dataDir != "" && !s.loaded
}

// ensureEditorLocked guarantees the Editor account exists in the
// accounts slice with the canonical appearance. Editor isn't user-
// customizable (username, colors, angle are all fixed) so we
// force-overwrite any stored Editor record with the canonical
// values on every load. That way bumping the Editor visual style
// in a future release applies to existing drives without a manual
// migration step.
func (s *accountStore) ensureEditorLocked() {
	canonical := Account{
		ID:        EditorAccountID,
		Username:  "Editor",
		ColorA:    "#7a7a7a", // medium gray
		ColorB:    "#2e2e2e", // dark gray
		Angle:     45,
		IsEditor:  true,
		LastTab:   "videos",
		CreatedAt: time.Now().Unix(),
	}
	for i, a := range s.accounts {
		if a.IsEditor {
			// Preserve the original CreatedAt so account ordering
			// (by CreatedAt asc) stays stable across launches.
			canonical.CreatedAt = a.CreatedAt
			s.accounts[i] = canonical
			return
		}
	}
	s.accounts = append([]Account{canonical}, s.accounts...)
}

func (s *accountStore) persistLocked() error {
	if s.dataDir == "" {
		return errors.New("no data dir")
	}
	if !s.loaded {
		// Refuse to write if we never managed to read the file
		// successfully. Writing here would silently overwrite a
		// corrupt-but-recoverable file with whatever happens to be
		// in memory (typically just the editor singleton).
		return errors.New("accounts.json hasn't been loaded — refusing to overwrite")
	}
	out := accountsFileFormat{
		Version:           AccountsSchemaVersion,
		Accounts:          s.accounts,
		LastUsedAccountID: s.lastUsedAccountID,
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dataDir, accountsFile)
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

// ---- public-ish (called from App methods) --------------------------------

// list returns a snapshot of accounts (Editor first, then real
// accounts ordered by CreatedAt ascending — stable across launches so
// the UI doesn't reshuffle).
func (s *accountStore) list() []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Account, 0, len(s.accounts))
	out = append(out, s.accounts...)
	sort.SliceStable(out, func(i, j int) bool {
		// Editor always first.
		if out[i].IsEditor != out[j].IsEditor {
			return out[i].IsEditor
		}
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out
}

// realAccountCount is the number of non-Editor accounts. Used at boot
// to detect the "first run, no accounts created yet" state.
func (s *accountStore) realAccountCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, a := range s.accounts {
		if !a.IsEditor {
			n++
		}
	}
	return n
}

func (s *accountStore) byID(id string) (Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.accounts {
		if a.ID == id {
			return a, true
		}
	}
	return Account{}, false
}

// create adds a new (non-editor) account. Validates the name; rejects
// duplicate usernames so the UI doesn't render two indistinguishable
// rows.
func (s *accountStore) create(username, colorA, colorB string, angle int) (Account, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return Account{}, errors.New("username cannot be empty")
	}
	if len(username) > 64 {
		return Account{}, errors.New("username too long")
	}
	if !looksLikeHexColor(colorA) || !looksLikeHexColor(colorB) {
		return Account{}, errors.New("colors must be CSS hex like #ff7788")
	}
	angle = ((angle % 360) + 360) % 360

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if !a.IsEditor && strings.EqualFold(a.Username, username) {
			return Account{}, fmt.Errorf("an account named %q already exists", username)
		}
	}
	id, err := newAccountID()
	if err != nil {
		return Account{}, err
	}
	acct := Account{
		ID:        id,
		Username:  username,
		ColorA:    colorA,
		ColorB:    colorB,
		Angle:     angle,
		IsEditor:  false,
		LastTab:   "videos",
		CreatedAt: time.Now().Unix(),
	}
	s.accounts = append(s.accounts, acct)
	if err := os.MkdirAll(filepath.Join(s.dataDir, "accounts", id), 0o755); err != nil {
		return Account{}, err
	}
	if err := s.persistLocked(); err != nil {
		return Account{}, err
	}
	return acct, nil
}

// del removes a non-editor account and its per-account state dir.
// Refuses to delete the Editor singleton; otherwise allows deletion
// even of the last real account — the caller (App.DeleteAccount) is
// responsible for re-arming the boot-state machinery so the user is
// immediately prompted to create another. Quitting the app at that
// point and reopening lands in the same first-account modal, so
// there's no escape into a logged-out state.
func (s *accountStore) del(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == EditorAccountID {
		return errors.New("can't delete the Editor account")
	}
	idx := -1
	for i, a := range s.accounts {
		if a.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("no account with id %q", id)
	}
	s.accounts = append(s.accounts[:idx], s.accounts[idx+1:]...)
	if s.lastUsedAccountID == id {
		s.lastUsedAccountID = ""
	}
	if s.currentAccountID == id {
		s.currentAccountID = ""
	}
	// Best-effort: remove the per-account state directory.
	_ = os.RemoveAll(filepath.Join(s.dataDir, "accounts", id))
	return s.persistLocked()
}

// switchTo sets the current account for this session and (unless it's
// Editor) updates lastUsedAccountID on disk so the next launch logs
// in to the same account.
func (s *accountStore) switchTo(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, a := range s.accounts {
		if a.ID == id {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no account with id %q", id)
	}
	s.currentAccountID = id
	if id != EditorAccountID {
		// Don't persist Editor as last-used. The Editor session is
		// transient by design; reopening the app should drop the user
		// back to whichever real account they were on before.
		s.lastUsedAccountID = id
		return s.persistLocked()
	}
	return nil
}

// resolveStartupAccount picks the account to log in on app boot:
//
//   - If lastUsedAccountID points at a still-existing real account,
//     pick it.
//   - Otherwise, the first non-editor account by CreatedAt.
//   - If there are no real accounts, returns "" — the caller should
//     prompt for first-account creation.
//
// The returned ID is also stored as currentAccountID so subsequent
// per-account operations work without an explicit switchTo() call.
func (s *accountStore) resolveStartupAccount() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Last-used.
	if s.lastUsedAccountID != "" {
		for _, a := range s.accounts {
			if a.ID == s.lastUsedAccountID && !a.IsEditor {
				s.currentAccountID = a.ID
				return a.ID
			}
		}
	}
	// First real account by creation time.
	var earliest *Account
	for i := range s.accounts {
		a := &s.accounts[i]
		if a.IsEditor {
			continue
		}
		if earliest == nil || a.CreatedAt < earliest.CreatedAt {
			earliest = a
		}
	}
	if earliest != nil {
		s.currentAccountID = earliest.ID
		return earliest.ID
	}
	return ""
}

// currentID is the in-session current account, or "" if none yet.
func (s *accountStore) currentID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentAccountID
}

// isCurrentEditor returns true iff the current account is Editor.
func (s *accountStore) isCurrentEditor() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.accounts {
		if a.ID == s.currentAccountID {
			return a.IsEditor
		}
	}
	return false
}

// updateLastTab records the tab the current account had open at last
// view. Persisted so the next launch comes back to the same place.
// No-op if the current account is Editor.
func (s *accountStore) updateLastTab(tab string) error {
	if tab != "accounts" && tab != "videos" && tab != "music" {
		return fmt.Errorf("invalid tab %q", tab)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == s.currentAccountID {
			if s.accounts[i].IsEditor {
				return nil
			}
			s.accounts[i].LastTab = tab
			return s.persistLocked()
		}
	}
	return nil
}

// stateDir returns `.data/accounts/<id>/` where per-account state
// files live. Creates it on demand.
func (s *accountStore) stateDir(accountID string) (string, error) {
	s.mu.RLock()
	d := s.dataDir
	s.mu.RUnlock()
	if d == "" {
		return "", errors.New("data dir not set")
	}
	dir := filepath.Join(d, "accounts", accountID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// ---- helpers --------------------------------------------------------------

func newAccountID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func looksLikeHexColor(s string) bool {
	if len(s) != 4 && len(s) != 7 {
		return false
	}
	if s[0] != '#' {
		return false
	}
	for _, r := range s[1:] {
		ok := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !ok {
			return false
		}
	}
	return true
}
