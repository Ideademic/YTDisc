package main

// v1 → v2 drive upgrade flow.
//
// The v2 binary refuses to write into a v1 drive without explicit
// user consent because the layout change is permanent: the bookmark
// file moves from `Videos/.state.json` to `.data/accounts/<id>/
// positions.json`, and `Videos/.bin/yt-dlp` will continue to be
// re-extracted but doesn't get migrated. The upgrade modal in the
// frontend offers two choices — "Upgrade" calls AcceptDriveUpgrade,
// "Quit" calls QuitApp.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// AcceptDriveUpgrade migrates a v1 drive to v2 layout. Steps:
//
//  1. Create .data/ + .data/accounts/.
//  2. Read Videos/.state.json into memory; remove the file.
//  3. Initialize the account/playlist stores.
//  4. Move the boot state to "needs-first-account" so the frontend
//     prompts for account creation. The legacy positions are stashed
//     in the App; CreateAccount completes the migration by importing
//     them into the first account's positions.json.
//
// After this call returns successfully the frontend re-reads
// GetBootState and displays the first-account creation modal.
func (a *App) AcceptDriveUpgrade() error {
	a.mu.RLock()
	dataDir := a.dataDir
	videosDir := a.videosDir
	state := a.bootStateField.State
	a.mu.RUnlock()

	if state != "needs-upgrade" {
		return fmt.Errorf("drive isn't in upgrade-required state (got %q)", state)
	}
	if dataDir == "" || videosDir == "" {
		return errors.New("drive root not located")
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("creating .data: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "accounts"), 0o755); err != nil {
		return fmt.Errorf("creating .data/accounts: %w", err)
	}

	// Read the legacy positions file so CreateAccount can import it
	// into the first account. Leave the file in place until the
	// import succeeds; we remove it then.
	legacy := readLegacyPositions(filepath.Join(videosDir, ".state.json"))

	if err := a.accounts.setDataDir(dataDir); err != nil {
		return err
	}
	if err := a.playlists.setDataDir(dataDir); err != nil {
		return err
	}

	// Stash the legacy positions for CreateAccount to consume.
	a.mu.Lock()
	a.pendingV1Positions = legacy
	a.bootStateField.State = "needs-first-account"
	a.mu.Unlock()
	return nil
}

// QuitApp shuts the app cleanly. Bound to the "Quit" button on the
// drive-upgrade modal, since v2 won't operate on a v1 drive without
// upgrade. Also useful as a generic close-app hook.
func (a *App) QuitApp() {
	wruntime.Quit(a.ctx)
}

// readLegacyPositions reads the v1 `.state.json` flat-positions file
// if present. Returns an empty map on missing / corrupt file (the
// upgrade should still proceed).
func readLegacyPositions(path string) map[string]positionEntry {
	out := map[string]positionEntry{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var f positionsFileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return out
	}
	if f.Positions != nil {
		out = f.Positions
	}
	return out
}

// completeV1MigrationIfNeeded is called from CreateAccount after a
// new account is added. If we have stashed v1 positions waiting,
// import them into the new account and remove the v1 file.
func (a *App) completeV1MigrationIfNeeded(accountID string) {
	a.mu.Lock()
	pending := a.pendingV1Positions
	a.pendingV1Positions = nil
	videosDir := a.videosDir
	a.mu.Unlock()

	if len(pending) == 0 {
		return
	}
	if err := a.positions.importLegacy(accountID, pending); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: importing legacy positions: %v\n", err)
		return
	}
	// All migrated — drop the old file. Best-effort; if removal
	// fails the user just has a stale .state.json sitting next to
	// videos. Harmless.
	_ = os.Remove(filepath.Join(videosDir, ".state.json"))
}
