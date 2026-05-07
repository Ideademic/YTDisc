package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"ytdisc/bundled"
)

// App is the singleton bound to the JS frontend. Wails turns its
// public methods into async functions on `window.go.main.App.*`.
//
// As of v2.0.0 the drive layout is:
//
//   DriveRoot/
//   ├── YTDisc.{app|exe|AppImage}
//   ├── .data/                 (account metadata, playlists, per-account state)
//   ├── Videos/                (video library + .thumbs + .trash)
//   └── Music/                 (music library + .arts + .trash)
//
// The drive root is the directory the binary lives in (or one of its
// ancestors, walking up until we find a Videos/ or .data/ marker).
// All persistent app state lives inside the drive root — nothing goes
// in the host's home directory unless extraction to Videos/.bin fails
// for the embedded yt-dlp.
type App struct {
	ctx context.Context

	// mu guards the in-memory library + thumbnail caches + currently
	// loaded music library + the path fields below.
	mu        sync.RWMutex
	driveRoot string
	videosDir string
	musicDir  string
	dataDir   string
	library   *Library
	music     *MusicLibrary

	// In-memory thumbnail fallback when disk cache isn't writable.
	memThumbs map[string][]byte

	// Per-account state stores (.data/accounts/<id>/...).
	accounts  *accountStore
	subs      *subscriptionStore
	positions *positionStore
	playlists *playlistStore

	// Background-extraction state for the bundled yt-dlp.
	ytdlpExtractedPath atomic.Pointer[string]
	ytdlpExtracting    atomic.Bool

	// Cached edit-capability state (yt-dlp present + internet
	// reachable + current account is Editor).
	editCap editCapCache

	// Tag bumped on each Rescan; thumbnail-discovery walks bail out
	// when their tag goes stale so churn doesn't stack goroutines.
	thumbWalkTag uint64

	// Boot state — set during startup based on what we find on disk.
	// Read by GetBootState; the frontend uses it to decide whether to
	// show the upgrade modal, the first-account creation modal, or
	// the regular UI.
	bootStateField BootState

	// Stashed v1 positions during an in-progress drive upgrade.
	// Populated by AcceptDriveUpgrade, consumed by CreateAccount
	// when the first real account is created (which is the natural
	// owner of the migrated bookmarks). nil when no upgrade is
	// pending.
	pendingV1Positions map[string]positionEntry
}

// BootState is the first thing the frontend asks for. The frontend
// branches on .State to render the appropriate boot UI.
type BootState struct {
	State            string  `json:"state"`            // "ready" | "needs-upgrade" | "needs-first-account" | "no-library"
	Message          string  `json:"message,omitempty"` // human-readable detail for "no-library"
	CurrentAccount   *Account `json:"currentAccount,omitempty"`
	HasMusicLibrary  bool    `json:"hasMusicLibrary"`
}

func NewApp() *App {
	a := &App{
		memThumbs: make(map[string][]byte),
		accounts:  newAccountStore(),
	}
	a.subs = newSubscriptionStore(a.accounts.stateDir)
	a.positions = newPositionStore(a.accounts.stateDir)
	a.playlists = newPlaylistStore()
	return a
}

// startup runs after the webview is ready. Locates the drive root,
// initializes per-account state, decides the boot state, and kicks
// off background work (yt-dlp extraction, thumbnail discovery).
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	root, err := findDriveRoot()
	if err != nil {
		a.mu.Lock()
		a.bootStateField = BootState{State: "no-library", Message: err.Error()}
		a.mu.Unlock()
		fmt.Fprintf(os.Stderr, "WARN: %v\n", err)
		return
	}

	videosDir := filepath.Join(root, "Videos")
	musicDir := filepath.Join(root, "Music")
	dataDir := filepath.Join(root, ".data")

	// Make sure Videos/ exists — the rest of the app assumes it. We
	// create Music/ on demand later.
	if _, err := os.Stat(videosDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_ = os.MkdirAll(videosDir, 0o755)
		}
	}

	// Detect drive state: if .data/ exists we're already on a v2
	// drive; otherwise check for the v1 marker (Videos/.state.json)
	// to know whether to offer upgrade or treat as fresh.
	hasDataDir := dirExists(dataDir)
	hasV1Marker := fileExists(filepath.Join(videosDir, ".state.json"))

	// CRITICAL: set the path fields AND the boot state BEFORE the
	// library scan. ScanLibrary parses every MP4's moov box and on a
	// USB drive with hundreds of videos that takes several seconds.
	// If the JS frontend calls GetBootState() during that window
	// (which it does — it's the very first call from init()), it'd
	// see the zero-value `State: ""` and fall through to a "no
	// current account" UI. Setting the boot state from cheap stat
	// checks here closes the race.
	a.mu.Lock()
	a.driveRoot = root
	a.videosDir = videosDir
	a.musicDir = musicDir
	a.dataDir = dataDir
	a.mu.Unlock()

	switch {
	case !hasDataDir && hasV1Marker:
		// v1 drive that needs explicit user consent to upgrade.
		// Don't initialize stores yet; AcceptDriveUpgrade does that.
		a.mu.Lock()
		a.bootStateField = BootState{
			State:           "needs-upgrade",
			HasMusicLibrary: dirExists(musicDir),
		}
		a.mu.Unlock()
	case !hasDataDir && !hasV1Marker:
		// Fresh drive (or one only used for media, never opened by
		// YTDisc). Initialize .data/, then prompt for first-account.
		if err := a.accounts.setDataDir(dataDir); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: setting up .data/: %v\n", err)
		}
		_ = a.playlists.setDataDir(dataDir)
		a.mu.Lock()
		a.bootStateField = BootState{
			State:           "needs-first-account",
			HasMusicLibrary: dirExists(musicDir),
		}
		a.mu.Unlock()
	default:
		// .data/ exists — normal v2 boot.
		if err := a.accounts.setDataDir(dataDir); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: %v\n", err)
			if errors.Is(err, ErrAccountsFileCorrupt) {
				a.mu.Lock()
				a.bootStateField = BootState{
					State:   "data-corrupt",
					Message: "accounts.json is corrupt — restore from a backup or remove it to start fresh",
				}
				a.mu.Unlock()
				return
			}
		}
		_ = a.playlists.setDataDir(dataDir)
		startupID := a.accounts.resolveStartupAccount()
		if startupID == "" {
			// .data/ existed but no real accounts — treat as
			// first-account state.
			a.mu.Lock()
			a.bootStateField = BootState{
				State:           "needs-first-account",
				HasMusicLibrary: dirExists(musicDir),
			}
			a.mu.Unlock()
		} else {
			acct, _ := a.accounts.byID(startupID)
			a.mu.Lock()
			a.bootStateField = BootState{
				State:           "ready",
				CurrentAccount:  &acct,
				HasMusicLibrary: dirExists(musicDir),
			}
			a.mu.Unlock()
		}
	}

	// Background yt-dlp extraction (writes to Videos/.bin, falls
	// back to user-cache dir if read-only). Same logic as v1.1.0.
	go a.extractEmbeddedYtdlpAsync()

	// Library scan + thumbnail discovery in the background. Boot
	// state is already set above so the frontend's first
	// GetBootState() call resolves immediately, even on slow drives
	// where the scan takes seconds. Channels()/Items() etc. handle
	// `library == nil` by returning empty results, which is fine
	// for the brief startup window.
	go a.initialScan()
}

// initialScan runs the first library + music scan and queues the
// thumbnail-discovery walk. Split out so startup() can return
// immediately once boot state is known.
func (a *App) initialScan() {
	a.mu.RLock()
	videosDir := a.videosDir
	musicDir := a.musicDir
	a.mu.RUnlock()
	if videosDir == "" {
		return
	}
	lib := ScanLibrary(videosDir)
	var music *MusicLibrary
	if dirExists(musicDir) {
		music = ScanMusicLibrary(musicDir)
	}
	a.mu.Lock()
	a.library = lib
	a.music = music
	a.thumbWalkTag++
	tag := a.thumbWalkTag
	a.mu.Unlock()
	go a.discoverThumbnails(tag)
}

func (a *App) extractEmbeddedYtdlpAsync() {
	if !bundled.HasEmbeddedYtdlp() {
		return
	}
	a.ytdlpExtracting.Store(true)
	defer a.ytdlpExtracting.Store(false)

	a.mu.RLock()
	dir := a.videosDir
	a.mu.RUnlock()

	if dir != "" {
		if path, err := bundled.ExtractYtdlp(filepath.Join(dir, ".bin")); err == nil {
			a.ytdlpExtractedPath.Store(&path)
			return
		}
	}
	if cache, err := os.UserCacheDir(); err == nil {
		if path, err := bundled.ExtractYtdlp(filepath.Join(cache, "YTDisc", "bin")); err == nil {
			a.ytdlpExtractedPath.Store(&path)
		}
	}
}

func (a *App) discoverThumbnails(tag uint64) {
	a.mu.RLock()
	lib := a.library
	current := a.thumbWalkTag
	a.mu.RUnlock()
	if lib == nil || current != tag {
		return
	}
	for _, ch := range lib.Channels {
		for _, v := range ch.allVideos() {
			a.mu.RLock()
			stale := a.thumbWalkTag != tag
			a.mu.RUnlock()
			if stale {
				return
			}
			if a.thumbCached(v.AbsPath) {
				continue
			}
			if data, err := extractEmbeddedThumb(v.AbsPath); err == nil {
				_ = a.writeThumb(v.AbsPath, data)
				continue
			}
			if data, _, err := readSidecarThumb(v.AbsPath); err == nil {
				_ = a.writeThumb(v.AbsPath, data)
				continue
			}
		}
	}
}

// ---- Boot + status JS-callable -------------------------------------------

func (a *App) GetBootState() BootState {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.bootStateField
}

// Status returns library aggregate counts. Used by the Accounts tab
// stats panel — formerly the bottom status bar.
func (a *App) Status() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := map[string]any{
		"ok":         a.library != nil,
		"videosDir":  a.videosDir,
		"driveRoot":  a.driveRoot,
		"musicDir":   a.musicDir,
		"channels":   0,
		"videos":     0,
		"totalBytes": int64(0),
		"totalSecs":  float64(0),
		"artists":    0,
		"albums":     0,
		"songs":      0,
		"musicSecs":  float64(0),
		"musicBytes": int64(0),
	}
	if a.library == nil {
		out["message"] = "Couldn't find a Videos/ folder next to the app."
		return out
	}
	out["channels"] = len(a.library.Channels)
	out["videos"] = a.library.TotalVideos()
	out["totalBytes"] = a.library.TotalBytes()
	out["totalSecs"] = a.library.TotalSeconds()
	if a.music != nil {
		artists, albums, songs, dur, bytes := a.music.Stats()
		out["artists"] = artists
		out["albums"] = albums
		out["songs"] = songs
		out["musicSecs"] = dur
		out["musicBytes"] = bytes
	}
	return out
}

// Rescan rebuilds both the video and music libraries. Scan happens
// outside the write lock so concurrent /video/, /thumb/, /audio/,
// /art/ requests aren't blocked while we walk the filesystem.
func (a *App) Rescan() {
	a.mu.RLock()
	videosDir := a.videosDir
	musicDir := a.musicDir
	a.mu.RUnlock()
	if videosDir == "" {
		return
	}
	lib := ScanLibrary(videosDir)
	var music *MusicLibrary
	if dirExists(musicDir) {
		music = ScanMusicLibrary(musicDir)
	}
	a.mu.Lock()
	a.library = lib
	a.music = music
	a.thumbWalkTag++
	tag := a.thumbWalkTag
	a.mu.Unlock()
	go a.discoverThumbnails(tag)
}

// ---- Channel listing (subscription-aware) --------------------------------

// Channels returns the channel list. For the Editor account we
// return all channels; for other accounts we return only the channels
// they're subscribed to.
func (a *App) Channels() []ChannelInfo {
	a.mu.RLock()
	lib := a.library
	a.mu.RUnlock()
	if lib == nil {
		return []ChannelInfo{}
	}
	editor := a.accounts.isCurrentEditor()
	currentID := a.accounts.currentID()
	out := make([]ChannelInfo, 0, len(lib.Channels))
	for _, c := range lib.Channels {
		if !editor && !a.subs.isSubscribed(currentID, c.Name) {
			continue
		}
		out = append(out, ChannelInfo{
			Name:       c.Name,
			VideoCount: c.totalVideoCount(),
			TotalSecs:  c.TotalSeconds(),
		})
	}
	return out
}

// Items returns the contents of a channel root or a specific folder
// inside a channel. Subscription gating is enforced by Channels()
// upstream; if the user manages to call Items() on an unsubscribed
// channel via direct API access we still serve it (Editor or not),
// since the data isn't sensitive — just hidden from the UI.
func (a *App) Items(channelName, folderName string) []Item {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.library == nil {
		return []Item{}
	}
	c := a.library.ChannelByName(channelName)
	if c == nil {
		return []Item{}
	}
	if folderName != "" {
		f := c.FolderByName(folderName)
		if f == nil {
			return []Item{}
		}
		out := make([]Item, 0, len(f.Videos))
		for _, v := range f.Videos {
			out = append(out, videoToItem(v))
		}
		sortItems(out)
		return out
	}
	out := make([]Item, 0, len(c.Videos)+len(c.Folders))
	for _, f := range c.Folders {
		out = append(out, Item{
			Kind:       "folder",
			Name:       f.Name,
			VideoCount: len(f.Videos),
			TotalSecs:  f.TotalSeconds(),
		})
	}
	for _, v := range c.Videos {
		out = append(out, videoToItem(v))
	}
	sortItems(out)
	return out
}

// AllChannels returns every channel in the library regardless of
// subscriptions, plus a "subscribed" boolean per channel for the
// current account. Used by the Manage Subscriptions UI.
func (a *App) AllChannels() []ChannelSub {
	a.mu.RLock()
	lib := a.library
	a.mu.RUnlock()
	if lib == nil {
		return []ChannelSub{}
	}
	currentID := a.accounts.currentID()
	editor := a.accounts.isCurrentEditor()
	out := make([]ChannelSub, 0, len(lib.Channels))
	for _, c := range lib.Channels {
		out = append(out, ChannelSub{
			Name:       c.Name,
			VideoCount: c.totalVideoCount(),
			Subscribed: editor || a.subs.isSubscribed(currentID, c.Name),
		})
	}
	return out
}

// ---- Account JS-callable -------------------------------------------------

func (a *App) GetAccounts() []Account {
	return a.accounts.list()
}

func (a *App) GetCurrentAccount() *Account {
	id := a.accounts.currentID()
	if id == "" {
		return nil
	}
	if acct, ok := a.accounts.byID(id); ok {
		return &acct
	}
	return nil
}

func (a *App) CreateAccount(username, colorA, colorB string, angle int) (Account, error) {
	acct, err := a.accounts.create(username, colorA, colorB, angle)
	if err != nil {
		return Account{}, err
	}
	// On first-account creation from a fresh OR upgraded drive,
	// auto-switch into the new account so the UI lands somewhere,
	// AND import any pending v1 positions into this account (which
	// completes the drive upgrade).
	if a.accounts.currentID() == "" {
		_ = a.accounts.switchTo(acct.ID)
	}
	a.completeV1MigrationIfNeeded(acct.ID)
	a.editCap.invalidate()
	a.refreshBootStateAfterAccountChange()
	return acct, nil
}

func (a *App) DeleteAccount(id string) error {
	if err := a.accounts.del(id); err != nil {
		return err
	}
	a.editCap.invalidate()
	a.refreshBootStateAfterAccountChange()
	return nil
}

func (a *App) SwitchAccount(id string) error {
	if err := a.accounts.switchTo(id); err != nil {
		return err
	}
	a.editCap.invalidate()
	a.refreshBootStateAfterAccountChange()
	return nil
}

func (a *App) UpdateLastTab(tab string) error {
	return a.accounts.updateLastTab(tab)
}

func (a *App) refreshBootStateAfterAccountChange() {
	id := a.accounts.currentID()
	a.mu.Lock()
	defer a.mu.Unlock()
	if id == "" {
		// Shouldn't happen on a normal switch, but if it does treat
		// as first-account again.
		a.bootStateField.State = "needs-first-account"
		a.bootStateField.CurrentAccount = nil
		return
	}
	if acct, ok := a.accounts.byID(id); ok {
		a.bootStateField.State = "ready"
		a.bootStateField.CurrentAccount = &acct
	}
}

// ---- Subscription JS-callable --------------------------------------------

func (a *App) Subscribe(channel string) error {
	id := a.accounts.currentID()
	if id == "" {
		return errors.New("no active account")
	}
	if a.accounts.isCurrentEditor() {
		return nil // editor sees everything; subscriptions are a no-op
	}
	return a.subs.subscribe(id, channel)
}

func (a *App) Unsubscribe(channel string) error {
	id := a.accounts.currentID()
	if id == "" {
		return errors.New("no active account")
	}
	if a.accounts.isCurrentEditor() {
		return nil
	}
	return a.subs.unsubscribe(id, channel)
}

// ---- Position JS-callable (per-account) ----------------------------------

func (a *App) GetPosition(relPath string) float64 {
	id := a.accounts.currentID()
	if id == "" || a.positions == nil {
		return 0
	}
	return a.positions.get(id, relPath)
}

func (a *App) SavePosition(relPath string, sec float64) error {
	id := a.accounts.currentID()
	if id == "" {
		return errors.New("no active account")
	}
	return a.positions.set(id, relPath, sec)
}

func (a *App) ClearPosition(relPath string) error {
	id := a.accounts.currentID()
	if id == "" {
		return nil
	}
	return a.positions.clear(id, relPath)
}

// ---- Thumbnail JS-callable -----------------------------------------------

func (a *App) HasThumbnail(relPath string) bool {
	abs, err := a.absVideoPath(relPath)
	if err != nil {
		return false
	}
	return a.thumbCached(abs)
}

func (a *App) FetchThumbnailFromYouTube(relPath string, urlOrID string) error {
	if !a.accounts.isCurrentEditor() {
		return errEditorOnly
	}
	abs, err := a.absVideoPath(relPath)
	if err != nil {
		return err
	}
	id := extractYouTubeID(urlOrID)
	if id == "" {
		id = extractYouTubeID(filepath.Base(relPath))
	}
	if id == "" {
		return errors.New("couldn't find a YouTube video ID in that input")
	}
	data, err := fetchYouTubeThumb(id)
	if err != nil {
		return err
	}
	return a.writeThumb(abs, data)
}

func (a *App) ImportThumbnailFromFile(relPath string, filePath string) error {
	if !a.accounts.isCurrentEditor() {
		return errEditorOnly
	}
	abs, err := a.absVideoPath(relPath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("empty image file")
	}
	return a.writeThumb(abs, data)
}

func (a *App) ClearThumbnail(relPath string) error {
	if !a.accounts.isCurrentEditor() {
		return errEditorOnly
	}
	abs, err := a.absVideoPath(relPath)
	if err != nil {
		return err
	}
	key := thumbKey(abs)
	a.mu.Lock()
	delete(a.memThumbs, key)
	dir := a.videosDir
	a.mu.Unlock()
	if dir != "" {
		_ = os.Remove(filepath.Join(dir, ".thumbs", key+".jpg"))
	}
	return nil
}

// errEditorOnly is the sentinel returned when a non-editor account
// tries to mutate the library. The frontend renders the error via
// the existing modal-status text path.
var errEditorOnly = errors.New("only the Editor account can do that")

// ---- internal helpers ----------------------------------------------------

func (a *App) absVideoPath(relPath string) (string, error) {
	a.mu.RLock()
	dir := a.videosDir
	a.mu.RUnlock()
	if dir == "" {
		return "", errors.New("library not loaded")
	}
	return safeJoin(dir, relPath)
}

func (a *App) absMusicPath(relPath string) (string, error) {
	a.mu.RLock()
	dir := a.musicDir
	a.mu.RUnlock()
	if dir == "" {
		return "", errors.New("music library not loaded")
	}
	return safeJoin(dir, relPath)
}

func (a *App) thumbCached(absPath string) bool {
	key := thumbKey(absPath)
	a.mu.RLock()
	dir := a.videosDir
	_, inMem := a.memThumbs[key]
	a.mu.RUnlock()
	if inMem {
		return true
	}
	if dir != "" {
		if fileExists(filepath.Join(dir, ".thumbs", key+".jpg")) {
			return true
		}
	}
	return false
}

func (a *App) writeThumb(absPath string, data []byte) error {
	a.mu.RLock()
	dir := a.videosDir
	a.mu.RUnlock()
	if dir == "" {
		return errors.New("library not loaded")
	}
	key := thumbKey(absPath)
	thumbsDir := filepath.Join(dir, ".thumbs")
	if err := os.MkdirAll(thumbsDir, 0o755); err == nil {
		path := filepath.Join(thumbsDir, key+".jpg")
		if err := os.WriteFile(path, data, 0o644); err == nil {
			return nil
		}
	}
	a.mu.Lock()
	a.memThumbs[key] = data
	a.mu.Unlock()
	return nil
}

func thumbKey(absPath string) string {
	sum := sha1.Sum([]byte(absPath))
	return hex.EncodeToString(sum[:])
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// ---- DTOs ----------------------------------------------------------------

type ChannelInfo struct {
	Name       string  `json:"name"`
	VideoCount int     `json:"videoCount"`
	TotalSecs  float64 `json:"totalSecs"`
}

type ChannelSub struct {
	Name       string `json:"name"`
	VideoCount int    `json:"videoCount"`
	Subscribed bool   `json:"subscribed"`
}

type Item struct {
	Kind string `json:"kind"`
	Name string `json:"name"`

	// folder fields
	VideoCount int     `json:"videoCount,omitempty"`
	TotalSecs  float64 `json:"totalSecs,omitempty"`

	// video fields (TotalSecs above doubles as video duration)
	Channel   string `json:"channel,omitempty"`
	Folder    string `json:"folder,omitempty"`
	RelPath   string `json:"relPath,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

func videoToItem(v *Video) Item {
	return Item{
		Kind:      "video",
		Name:      v.Title,
		Channel:   v.Channel,
		Folder:    v.Folder,
		RelPath:   filepath.ToSlash(v.RelPath),
		TotalSecs: v.DurationSec,
		Width:     v.Width,
		Height:    v.Height,
		SizeBytes: v.SizeBytes,
	}
}

func sortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
}

// ---- drive root discovery ------------------------------------------------

// findDriveRoot walks up from the binary location to find the
// directory that contains either a Videos/ subfolder OR a .data/
// subfolder. v1 drives only have the former; v2 drives have both.
// Falls back to the binary's directory if neither is found, so a
// fresh launch on an empty drive can still create them.
func findDriveRoot() (string, error) {
	check := func(dir string) bool {
		if dir == "" {
			return false
		}
		return dirExists(filepath.Join(dir, "Videos")) ||
			dirExists(filepath.Join(dir, ".data"))
	}

	// AppImage: $APPIMAGE points at the .AppImage file itself.
	if appimage := os.Getenv("APPIMAGE"); appimage != "" {
		dir := filepath.Dir(appimage)
		if check(dir) {
			return dir, nil
		}
	}

	exe, err := os.Executable()
	if err == nil {
		if resolved, err2 := filepath.EvalSymlinks(exe); err2 == nil {
			exe = resolved
		}
		dir := filepath.Dir(exe)
		for i := 0; i < 5; i++ {
			if check(dir) {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
		// Nothing matched — return the binary's own directory so we
		// can create Videos/ + .data/ there. This is the "fresh
		// drive, just plopped the binary on it" path.
		return filepath.Dir(exe), nil
	}

	if cwd, err := os.Getwd(); err == nil {
		if check(cwd) {
			return cwd, nil
		}
		return cwd, nil
	}

	return "", errors.New("couldn't determine drive root")
}
