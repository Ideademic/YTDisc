//go:build windows

package bundled

import _ "embed"

//go:embed yt-dlp.exe
var Ytdlp []byte

const YtdlpFilename = "yt-dlp.exe"
