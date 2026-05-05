package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Sidecar image extensions to look for next to a video file.
// Order = preference (jpg first since it's most common).
var sidecarExts = []string{".jpg", ".jpeg", ".png", ".webp"}

// ---------------------------------------------------------------------------
// Embedded MP4 cover art
// ---------------------------------------------------------------------------

// extractEmbeddedThumb pulls a JPEG or PNG embedded in the MP4's metadata
// (typically moov.udta.meta.ilst.covr — iTunes/QuickTime cover art).
//
// Strategy: serialize the moov box back to bytes and scan for image
// signatures. Robust to whichever metadata layout the encoder used,
// avoids the variance in mp4ff's box-tree API across versions, and the
// moov box is always small (no sample data is in it).
func extractEmbeddedThumb(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	parsed, err := mp4.DecodeFile(f, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		return nil, err
	}
	if parsed.Moov == nil {
		return nil, errors.New("no moov box")
	}

	var buf bytes.Buffer
	if err := parsed.Moov.Encode(&buf); err != nil {
		return nil, err
	}
	return findEmbeddedImage(buf.Bytes())
}

// findEmbeddedImage looks for the first JPEG or PNG payload in a byte
// slice and returns it. Searches JPEG first (more common in covr atoms).
func findEmbeddedImage(data []byte) ([]byte, error) {
	jpegSOI := []byte{0xFF, 0xD8, 0xFF}
	jpegEOI := []byte{0xFF, 0xD9}
	if start := bytes.Index(data, jpegSOI); start >= 0 {
		// Find the EOI marker after start. Use Index on data[start+2:]
		// to skip the SOI itself and avoid false self-match.
		if rel := bytes.Index(data[start+2:], jpegEOI); rel >= 0 {
			end := start + 2 + rel + 2
			if end-start > 200 { // sanity floor: don't return ~empty JPEGs
				return data[start:end], nil
			}
		}
	}

	pngSig := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	pngIEND := []byte{0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82}
	if start := bytes.Index(data, pngSig); start >= 0 {
		if rel := bytes.Index(data[start:], pngIEND); rel > 0 {
			return data[start : start+rel+8], nil
		}
	}

	return nil, errors.New("no embedded image found")
}

// ---------------------------------------------------------------------------
// Sidecar files
// ---------------------------------------------------------------------------

// findSidecarThumb returns the path of an image file named like the
// video (e.g. "Foo.mp4" -> "Foo.jpg") if one exists, else "".
func findSidecarThumb(videoPath string) string {
	stem := strings.TrimSuffix(videoPath, filepath.Ext(videoPath))
	for _, ext := range sidecarExts {
		candidate := stem + ext
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// readSidecarThumb reads the sidecar bytes if one exists. If the file
// is not JPEG, the caller should re-encode (handled by saveThumbBytes
// by passing through unchanged — the webview accepts PNG/WebP equally
// well, only the on-disk filename uses .jpg for simplicity).
func readSidecarThumb(videoPath string) ([]byte, string, error) {
	p := findSidecarThumb(videoPath)
	if p == "" {
		return nil, "", errors.New("no sidecar")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, "", err
	}
	return data, p, nil
}

// ---------------------------------------------------------------------------
// YouTube ID extraction
// ---------------------------------------------------------------------------

// YouTube video IDs are exactly 11 characters from [A-Za-z0-9_-].
var ytIDRe = regexp.MustCompile(`[A-Za-z0-9_-]{11}`)

var ytURLPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?:youtube\.com/watch\?v=|youtu\.be/|youtube\.com/embed/|youtube\.com/shorts/|youtube\.com/v/)([A-Za-z0-9_-]{11})`),
	regexp.MustCompile(`[?&]v=([A-Za-z0-9_-]{11})`),
}

// extractYouTubeID accepts any of:
//   - a full YouTube URL (any common variant)
//   - a bare 11-char video ID
//   - a filename like "Title [VIDEOID].mp4" (yt-dlp default)
//
// Returns "" if nothing matches.
func extractYouTubeID(input string) string {
	input = strings.TrimSpace(input)

	// Bare ID.
	if len(input) == 11 && ytIDRe.MatchString(input) && ytIDRe.FindString(input) == input {
		return input
	}

	// URL patterns.
	for _, re := range ytURLPatterns {
		if m := re.FindStringSubmatch(input); m != nil {
			return m[1]
		}
	}

	// Bracketed ID in filename (yt-dlp default: "Title [abc12345678].mp4").
	if m := regexp.MustCompile(`\[([A-Za-z0-9_-]{11})\]`).FindStringSubmatch(input); m != nil {
		return m[1]
	}

	return ""
}

// ---------------------------------------------------------------------------
// YouTube thumbnail fetching
// ---------------------------------------------------------------------------

// YouTube thumbnail sizes, ordered best-to-worst. maxres isn't always
// available; sd and hq are guaranteed for any public video.
var ytThumbSizes = []string{"maxresdefault", "sddefault", "hqdefault"}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// fetchYouTubeThumb downloads the best-available thumbnail for the
// given YouTube video ID. Returns JPEG bytes.
func fetchYouTubeThumb(videoID string) ([]byte, error) {
	if videoID == "" {
		return nil, errors.New("empty video ID")
	}
	for _, size := range ytThumbSizes {
		url := fmt.Sprintf("https://i.ytimg.com/vi/%s/%s.jpg", videoID, size)
		resp, err := httpClient.Get(url)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}
		// YouTube serves a tiny "image not available" placeholder
		// (~800 B) when a particular size doesn't exist. Skip those.
		const placeholderCutoff = 1500
		body, err := io.ReadAll(io.LimitReader(resp.Body, 5_000_000))
		resp.Body.Close()
		if err != nil {
			continue
		}
		if len(body) < placeholderCutoff {
			continue
		}
		return body, nil
	}
	return nil, fmt.Errorf("no thumbnail available for video ID %q", videoID)
}
