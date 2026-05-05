package main

import (
	"os"
	"path/filepath"
	"strings"
)

// init runs before main. It augments PATH with the common Homebrew /
// MacPorts / user-bin directories so we can find yt-dlp, ffmpeg, etc.
//
// macOS GUI apps launched from Finder or the Dock inherit a minimal
// PATH from launchd — typically just /usr/bin:/bin:/usr/sbin:/sbin —
// which excludes /usr/local/bin (Intel Homebrew) and /opt/homebrew/bin
// (Apple Silicon Homebrew). Without this fix, `exec.LookPath("yt-dlp")`
// returns "not found" even when the user has yt-dlp installed and
// `which yt-dlp` works fine in their terminal.
//
// We only add directories that actually exist on disk, and we don't
// duplicate entries already in PATH, so this is a no-op when launched
// from a terminal where PATH is already correct.
func init() {
	augmentSearchPath()
}

func augmentSearchPath() {
	candidates := []string{
		"/opt/homebrew/bin",  // Apple Silicon Homebrew
		"/opt/homebrew/sbin",
		"/usr/local/bin",     // Intel Homebrew + many manual installs
		"/usr/local/sbin",
		"/opt/local/bin",     // MacPorts
		"/opt/local/sbin",
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "bin"),
		)
	}

	sep := string(os.PathListSeparator)
	current := os.Getenv("PATH")
	existing := make(map[string]bool)
	for _, p := range strings.Split(current, sep) {
		if p != "" {
			existing[p] = true
		}
	}

	var toAdd []string
	for _, c := range candidates {
		if existing[c] {
			continue
		}
		// Only add directories that actually exist — adding a
		// non-existent path is harmless but pollutes PATH for any
		// child processes we spawn.
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			toAdd = append(toAdd, c)
		}
	}

	if len(toAdd) == 0 {
		return
	}

	newPath := strings.Join(toAdd, sep)
	if current != "" {
		newPath = current + sep + newPath
	}
	os.Setenv("PATH", newPath)
}
