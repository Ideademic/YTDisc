//go:build linux

package bundled

import _ "embed"

//go:embed yt-dlp_linux
var Ytdlp []byte

const YtdlpFilename = "yt-dlp"
