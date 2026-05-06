#!/usr/bin/env bash
# Downloads the latest yt-dlp standalone binaries into bundled/,
# replacing the in-repo placeholders so `wails build` actually
# embeds a working yt-dlp.
#
# Run this once locally before building if you want edit mode to
# work with embedded yt-dlp. CI runs the same downloads inside the
# release workflow.
#
# Usage:
#   tools/fetch-ytdlp.sh                # latest release
#   tools/fetch-ytdlp.sh 2026.01.01     # pinned version

set -euo pipefail

VERSION="${1:-latest}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$ROOT/bundled"

if [[ "$VERSION" == "latest" ]]; then
  base="https://github.com/yt-dlp/yt-dlp/releases/latest/download"
else
  base="https://github.com/yt-dlp/yt-dlp/releases/download/$VERSION"
fi

# Map: source asset name → destination filename (matches what
# bundled/embed_*.go embeds).
declare -a MAPPINGS=(
  "yt-dlp_macos:yt-dlp_macos"
  "yt-dlp.exe:yt-dlp.exe"
  "yt-dlp_linux:yt-dlp_linux"
)

echo "Fetching yt-dlp $VERSION → $DEST"
for entry in "${MAPPINGS[@]}"; do
  src="${entry%%:*}"
  dst="${entry##*:}"
  url="$base/$src"
  out="$DEST/$dst"
  echo "  $src"
  curl --fail --show-error --location --retry 5 --retry-delay 5 \
    --output "$out.new" "$url"
  mv "$out.new" "$out"
done

# Sanity-check what we just downloaded. A successful curl with a 200
# response can still leave us with garbage if (e.g.) the GitHub
# release URL ever serves an HTML error page that's still > 1 MB. We
# enforce two invariants: (a) every file is at least 1 MB (smallest
# real yt-dlp standalone bundle is ~17 MB on Windows), and (b)
# `file(1)` recognizes it as the right kind of binary for its target
# platform. If either check fails, abort loudly so the build doesn't
# silently embed broken bytes.
echo
echo "Verifying downloaded binaries..."

check() {
  local path="$1"
  local expect="$2"  # substring expected from `file` output
  if [[ ! -f "$path" ]]; then
    echo "  FAIL: $path missing" >&2
    return 1
  fi
  local size
  size=$(wc -c < "$path")
  if (( size < 1048576 )); then
    echo "  FAIL: $path is only $size bytes (expected >= 1 MB)" >&2
    return 1
  fi
  local kind
  kind=$(file -b "$path")
  if [[ "$kind" != *"$expect"* ]]; then
    echo "  FAIL: $path is '$kind' (expected to contain '$expect')" >&2
    return 1
  fi
  echo "  OK:   $(basename "$path") ($((size / 1048576)) MB, $kind)"
}

check "$DEST/yt-dlp_macos" "Mach-O"
check "$DEST/yt-dlp.exe"   "PE32"
check "$DEST/yt-dlp_linux" "ELF"

echo
echo "Done. yt-dlp binaries are in place; \`wails build\` will embed them."
