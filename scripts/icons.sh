#!/usr/bin/env bash
# Rasterize the favicon from its single source of truth,
# internal/web/static/favicon.svg, into the two formats that cannot be
# served as SVG:
#
#   favicon.ico          — 16/32/48 fallback for browsers that ignore
#                          the SVG icon, and what a bare /favicon.ico
#                          request gets. The 16px layer is rendered from
#                          a bolder variant so the mark survives the
#                          size.
#   apple-touch-icon.png — 180×180 home-screen icon, rendered from a
#                          square variant with no corner radius of its
#                          own: iOS applies its own mask and would
#                          otherwise show the transparent corners black.
#
# Both outputs are committed (`//go:embed all:static` needs them at
# build time), so this only runs when the mark itself changes.
#
# Usage:  scripts/icons.sh

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
STATIC="${ROOT}/internal/web/static"
SRC="${STATIC}/favicon.svg"

for tool in magick rsvg-convert; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "error: $tool not found (need ImageMagick + librsvg)" >&2
        exit 1
    fi
done

[ -f "$SRC" ] || { echo "error: $SRC missing" >&2; exit 1; }

work="$(mktemp -d -t toggle-monitor-icons-XXXXXX)"
trap 'rm -rf "$work"' EXIT

# 48 and 32 come straight from the source. The 16px layer is rendered
# from a bolder variant: at that size the 3.6 stroke antialiases into a
# grey smear and the pulse stops reading as a pulse.
rsvg-convert -w 48 -h 48 "$SRC" -o "${work}/48.png"
rsvg-convert -w 32 -h 32 "$SRC" -o "${work}/32.png"
sed 's/stroke-width="3.6"/stroke-width="4.4"/' "$SRC" > "${work}/bold.svg"
rsvg-convert -w 16 -h 16 "${work}/bold.svg" -o "${work}/16.png"
magick "${work}/48.png" "${work}/32.png" "${work}/16.png" "${STATIC}/favicon.ico"

# Same mark, square: iOS applies its own corner mask, so the source for
# this one drops the corner radius rather than showing it twice.
sed 's/ rx="7"//' "$SRC" > "${work}/apple.svg"
rsvg-convert -w 180 -h 180 "${work}/apple.svg" -o "${STATIC}/apple-touch-icon.png"
magick "${STATIC}/apple-touch-icon.png" -strip "${STATIC}/apple-touch-icon.png"

echo "wrote ${STATIC}/favicon.ico and ${STATIC}/apple-touch-icon.png"
