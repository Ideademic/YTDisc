// Package bundled exposes platform-specific binaries that ship inside
// the YTDisc app — currently just yt-dlp.
//
// The binaries themselves are compiled in via `//go:embed` in the
// per-platform files (embed_darwin.go etc.). The repo only commits
// 1-byte placeholder files; CI replaces them with real yt-dlp
// binaries before `wails build`. Local developers can run
// `tools/fetch-ytdlp.sh` to do the same on their machine.
//
// At runtime, callers check `len(Ytdlp) >= MinEmbeddedSize` to tell
// whether a real binary is embedded vs. the in-repo placeholder, and
// fall back to PATH-based discovery when the placeholder is detected
// (so developer builds keep working without the fetch step).
package bundled

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

// MinEmbeddedSize is the byte threshold below which the embedded
// binary is treated as a placeholder rather than a real yt-dlp.
// Real yt-dlp standalone bundles are 17–30 MB depending on platform;
// 1 MB is comfortably above any plausible placeholder and well below
// any legitimate binary.
const MinEmbeddedSize = 1 << 20

// HasEmbeddedYtdlp reports whether a real yt-dlp binary is embedded
// (vs. the placeholder shipped in-repo). False on developer builds
// that haven't run tools/fetch-ytdlp.sh.
func HasEmbeddedYtdlp() bool {
	return len(Ytdlp) >= MinEmbeddedSize
}

// ExtractYtdlp writes the embedded yt-dlp binary into binDir and
// returns the absolute path of the executable. The destination is
// created with mode 0755 so it's directly executable. If a file
// already exists at the target path with the same byte length as
// the embedded binary, this is a no-op (no rewrite cost on every
// app launch).
//
// Callers should prefer this path when HasEmbeddedYtdlp() is true.
func ExtractYtdlp(binDir string) (string, error) {
	if !HasEmbeddedYtdlp() {
		return "", errors.New("no embedded yt-dlp (placeholder size)")
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(binDir, YtdlpFilename)

	if info, err := os.Stat(dst); err == nil && info.Size() == int64(len(Ytdlp)) {
		// Already extracted and the byte count matches — assume same
		// version and skip the rewrite. Cheap startup path.
		return dst, nil
	}

	// Atomic write via tmp + rename so a failed write (full disk,
	// USB yanked) doesn't leave a half-written executable behind.
	// The tmp suffix includes our PID so two app instances pointed
	// at the same Videos/.bin (USB stick mounted twice, double-
	// launch) don't stomp each other's tmp files mid-write —
	// Linux's `O_TRUNC` on a running executable returns ETXTBSY.
	tmp := dst + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, Ytdlp, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := postExtractFixup(dst); err != nil {
		// Don't fail the whole extraction — the binary is on disk
		// and might still execute. Caller will discover the problem
		// when it actually tries to run yt-dlp.
		_ = err
	}
	return dst, nil
}
