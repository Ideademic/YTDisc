#!/usr/bin/env bash
# Fetch the YTDisc icon set from Phosphor Icons (Bold weight) and
# build a single SVG sprite at frontend/src/icons.svg. Each icon is
# emitted as a <symbol id="ph-NAME"> referenced from the rest of the
# frontend via <use href="src/icons.svg#ph-NAME"/>.
#
# Run once to populate the sprite, or whenever you add/change icons
# in the ICONS list below. The resulting icons.svg is committed to
# the repo so a fresh checkout doesn't need network access — this
# script is for regeneration only.
#
# Usage: tools/fetch-icons.sh

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/frontend/src/icons.svg"

ICONS=(
  # Tabs
  user-circle
  monitor-play
  music-notes

  # Common controls
  plus
  x
  check
  caret-down
  caret-right
  arrow-left
  arrow-elbow-up-right
  arrow-square-out

  # Editor row actions
  pencil-simple
  trash
  folder
  folder-plus

  # Player transport
  play
  pause
  skip-back
  skip-forward
  shuffle
  corners-out
  corners-in

  # Music
  vinyl-record
  film-strip
)

base="https://raw.githubusercontent.com/phosphor-icons/core/main/raw/bold"

echo "Fetching ${#ICONS[@]} Phosphor Bold icons → $OUT"
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" style="display:none" aria-hidden="true">'
  for name in "${ICONS[@]}"; do
    url="$base/${name}-bold.svg"
    body=$(curl --fail --show-error --silent --location --retry 3 "$url")
    # Strip the outer <svg ...> wrapper. Phosphor SVGs use a single
    # opening tag terminated by '>' on one line, then path/rect
    # children, then a closing </svg>. The two seds drop those.
    inner=$(printf '%s' "$body" \
      | sed -E 's|^<svg[^>]*>||' \
      | sed -E 's|</svg>$||')
    echo "  <symbol id=\"ph-${name}\" viewBox=\"0 0 256 256\">${inner}</symbol>"
  done
  echo '</svg>'
} > "$OUT"

count=$(grep -c '<symbol ' "$OUT")
size=$(wc -c < "$OUT")
echo "Wrote $count symbols, ${size} bytes."
