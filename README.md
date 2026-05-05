# YTDisc

**A portable YouTube library that lives on a thumb drive.** Drop a
single binary onto any USB stick or SD card next to a `Videos/`
folder, plug it into any Mac / Windows / Linux machine, and double-
click. You get a Finder-style three-column browser with full playback
and (optional) library editing via yt-dlp — no install, no settings
to tweak per machine, nothing left behind on the host.

```
📁 USB-stick/
├── YTDisc.app          ← (or YTDisc.exe / YTDisc-x86_64.AppImage)
└── Videos/
    ├── Channel A/
    │   ├── Video 1.mp4
    │   └── Video 2.mp4
    └── Channel B/
        ├── My playlist/
        │   ├── 001 - First.mp4
        │   └── 002 - Second.mp4
        └── Video 3.mp4
```

The whole library — videos, thumbnails, resume bookmarks — lives
inside `Videos/`, so unplugging the stick takes the entire setup with
it. Nothing is written outside that folder.

## Features

- Three-column UI: channels → videos → detail with thumbnail and play
- Native HTML5 `<video>` playback with full controls and fullscreen
- **Folders** inside channels (one level deep, A-Z interleaved with videos)
- **Resume playback** — closing a video saves your position; opening it
  again jumps back. Skipped if you're in the first 45 s or last 20 s.
  Bookmarks live in `Videos/.state.json` and travel with the library.
- **Playlist downloads** — paste a YouTube playlist URL, get a folder
  with every entry pre-numbered in playlist order
- **Quality selection** — FHD (1080p) / HD (720p) / SD (480p)
- Thumbnail discovery (in priority order):
  1. Embedded MP4 cover art
  2. Sidecar image: `{name}.jpg`/`.jpeg`/`.png`/`.webp` next to the video
  3. Manual fetch from YouTube via paste-URL (no API key)
  4. Manual import from local image file
- Optional edit mode (requires yt-dlp + ffmpeg + internet):
  - Add / rename / delete channels and folders
  - Add videos and playlists by pasting YouTube URLs
  - Move videos between folders
  - Empty a folder (keeps the videos at the channel root) or delete
    it (sends it to `.trash` along with everything inside)
  - Rename / delete videos
  - Deletes go to `Videos/.trash/` — recoverable until you empty it

## Install (the USB-stick way)

The intended setup: format a thumb drive or SD card, drop the YTDisc
binary on it, make a `Videos/` folder next to it, and you're done.
The same drive plugs into a Mac, a Windows laptop, or a Linux box and
Just Works — no installer, no system files written outside the drive.

1. **Get a binary** from the [releases page](../../releases):
   - **macOS** — `YTDisc-darwin-universal.zip` (universal: Apple
     Silicon + Intel). Unzip on the stick to get `YTDisc.app`.
   - **Windows** — `YTDisc-windows-amd64.exe` (single portable .exe,
     no installer, no setup wizard — just the binary)
   - **Linux** — `YTDisc-x86_64.AppImage` (single executable file)
2. **Copy it to your USB stick / SD card.** You can grab one binary
   per OS you might want to plug into and put them all on the same
   drive — the app only ever reads from `Videos/` next to itself, so
   they coexist fine.
3. **Make a `Videos/` folder on the stick** and arrange media as
   `Videos/<Channel>/<Video>.mp4` (the app's edit mode handles this
   for you, but you can also drop in existing files).
4. **Double-click the binary.** That's it.

You _can_ also use it locally on an internal SSD if you want — the
app doesn't care. The portable design just means it doesn't have to.

## Optional: yt-dlp + ffmpeg for editing

The library editing UI is gated on yt-dlp + ffmpeg being installed and
internet being available. Install via Homebrew on Mac:

```sh
brew install yt-dlp ffmpeg
```

Without ffmpeg, downloads of >720p videos fail — yt-dlp uses ffmpeg
to merge the separate video and audio streams YouTube serves at
higher resolutions. With both installed, the edit toggle in the
status bar lights up. If you install them with the app already
running, click the disabled badge to re-check.

## Building from source

Requires Go 1.25+, Node 18+, and Wails CLI v2:

```sh
go install github.com/wailsapp/wails/v2/cmd/wails@latest
```

Then in the project directory:

```sh
go mod tidy
wails build -platform darwin/universal      # universal Mac binary
wails build -platform windows/amd64         # Windows
wails build -platform linux/amd64           # Linux (needs WebKit2GTK)
```

Output is in `build/bin/`. For dev with hot reload, use `wails dev`.

## Architecture

- `main.go` — Wails entry point and window options
- `app.go` — JS-bound App methods + thumbnail cache + library scan tag
- `library.go` — filesystem scanner + MP4 metadata reader (mp4ff)
- `thumbnails.go` — embedded cover art + sidecar + YouTube fetch
- `editor.go` — yt-dlp invocation + channel/folder/video CRUD +
  playlist + format-selector + capability detection
- `positions.go` — resume-playback bookmarks (`.state.json`)
- `video_handler.go` — HTTP handler with Range support for `<video>`
  + symlink-aware path-traversal guard
- `pathfix.go` — prepends Homebrew paths so Finder-launched apps can
  find yt-dlp / ffmpeg
- `frontend/` — vanilla HTML/CSS/JS, no bundler

See [CLAUDE.md](CLAUDE.md) for deeper architecture notes.
