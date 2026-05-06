//go:build darwin

package bundled

import (
	"context"
	"os/exec"
	"time"
)

// postExtractFixup makes the freshly-written yt-dlp binary executable
// under macOS Gatekeeper. Two things can block exec on a fresh Mac:
//
//  1. The `com.apple.quarantine` extended attribute, which can be
//     inherited from a quarantined parent app (downloaded .app
//     before the user right-click→Opens it). Strip it.
//  2. The lack of any code signature, which Gatekeeper rejects on
//     macOS 14+. Apply an ad-hoc signature (sign with `-`).
//
// Both commands are best-effort: if `xattr` or `codesign` is
// missing or the binary is already signed, ignore the error. Bare
// `chmod +x` (already done by 0o755 on WriteFile) plus these two
// shellouts is the minimum to get an unsigned downloaded executable
// runnable from inside our app on a quarantined first launch.
func postExtractFixup(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// `-c` clears every xattr (including the quarantine bit and any
	// downloaded-from xattrs). `xattr` returns 0 if the file has no
	// xattrs, so this is safe regardless of quarantine state.
	_ = exec.CommandContext(ctx, "/usr/bin/xattr", "-c", path).Run()

	// `codesign -f -s -` applies an ad-hoc signature in place.
	// `-f` forces re-signing if anything's already there. Ad-hoc
	// signatures don't satisfy Notarization checks but Gatekeeper
	// accepts them for child processes of an app that's already
	// been launched.
	_ = exec.CommandContext(ctx, "/usr/bin/codesign", "--force", "--sign", "-", path).Run()

	return nil
}
