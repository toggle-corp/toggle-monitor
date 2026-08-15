#!/usr/bin/env bash
# Vendor the design system's two faces — Hanken Grotesk (sans) and IBM
# Plex Mono (mono) — as latin-subset woff2 into
# internal/web/static/fonts/, where `//go:embed all:static` picks them
# up. Self-hosting keeps the console renderable with no third-party
# network reachable, which is the point of an ops tool.
#
# Usage:  scripts/fonts.sh [--force]
# Existing files are left alone unless --force is passed.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT_DIR="${ROOT}/internal/web/static/fonts"
FORCE="${1:-}"

# A modern UA is what makes the Google Fonts CSS API serve woff2 rather
# than a legacy format.
UA="Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"

mkdir -p "${OUT_DIR}"

# fetch <css-url> <out-name> [weight]
#   Pulls the CSS, keeps the `latin` @font-face block (optionally the one
#   for a given weight), and downloads its woff2 to <out-name>.
fetch() {
  local url="$1" out="$2" weight="${3:-}"
  local dest="${OUT_DIR}/${out}"

  if [[ -f "${dest}" && "${FORCE}" != "--force" ]]; then
    echo "have ${out}"
    return 0
  fi

  local css
  css="$(curl -fsSL -A "${UA}" "${url}")"

  local src
  src="$(CSS="${css}" WEIGHT="${weight}" python3 - <<'PY'
import os, re

css = os.environ["CSS"]
want = os.environ.get("WEIGHT", "")
# Google Fonts labels each block with a `/* subset */` comment.
blocks = re.split(r"/\*\s*([\w-]+)\s*\*/", css)[1:]
for subset, body in zip(blocks[::2], blocks[1::2]):
    if subset != "latin":
        continue
    if want and not re.search(r"font-weight:\s*%s\s*;" % re.escape(want), body):
        continue
    m = re.search(r"src:\s*url\((https://[^)]+\.woff2)\)", body)
    if m:
        print(m.group(1))
        break
PY
)"

  if [[ -z "${src}" ]]; then
    echo "could not find a latin woff2 for ${out} in ${url}" >&2
    exit 1
  fi

  echo "downloading ${out} <- ${src}"
  curl -fsSL -A "${UA}" -o "${dest}" "${src}"
}

# Hanken Grotesk ships as a variable font — one file covers 400..800.
fetch "https://fonts.googleapis.com/css2?family=Hanken+Grotesk:wght@400..800" \
  "hanken-grotesk-latin.woff2"

# IBM Plex Mono is static on Google Fonts, so each weight is its own file.
fetch "https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500;600" \
  "ibm-plex-mono-latin-400.woff2" 400
fetch "https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500;600" \
  "ibm-plex-mono-latin-500.woff2" 500
fetch "https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500;600" \
  "ibm-plex-mono-latin-600.woff2" 600

echo "fonts vendored into ${OUT_DIR}"
