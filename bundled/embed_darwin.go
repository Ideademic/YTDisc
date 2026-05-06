//go:build darwin

package bundled

import _ "embed"

// yt-dlp's macOS standalone bundle is universal (arm64 + x86_64), so
// one binary covers both Apple Silicon and Intel.

//go:embed yt-dlp_macos
var Ytdlp []byte

// YtdlpFilename is what we name the file once extracted onto disk.
// Stripping the platform suffix keeps invocations portable.
const YtdlpFilename = "yt-dlp"
