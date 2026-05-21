#!/usr/bin/env bash
# Download the platform-specific tailwindcss standalone CLI binary into
# bin/tailwindcss if it isn't already present. No Node, no npm, no
# package.json — the binary ships a complete Tailwind compiler.
#
# Usage:  scripts/tailwindcss.sh [--version vX.Y.Z]
# The downloaded binary is then invoked by the Makefile's `tailwind`
# target to compile internal/web/tailwind/input.css into
# internal/web/static/css/app.css.

set -euo pipefail

VERSION="${TAILWIND_VERSION:-v3.4.18}"
BIN_DIR="$(cd "$(dirname "$0")/.." && pwd)/bin"
BIN_PATH="${BIN_DIR}/tailwindcss"

mkdir -p "${BIN_DIR}"

if [[ -x "${BIN_PATH}" ]]; then
  # Already installed — version pin is best-effort; remove bin/tailwindcss
  # if you want to force a re-download against a new TAILWIND_VERSION.
  echo "tailwindcss already present: ${BIN_PATH}"
  exit 0
fi

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "${OS}-${ARCH}" in
  linux-x86_64)   ASSET="tailwindcss-linux-x64"   ;;
  linux-aarch64)  ASSET="tailwindcss-linux-arm64" ;;
  darwin-x86_64)  ASSET="tailwindcss-macos-x64"   ;;
  darwin-arm64)   ASSET="tailwindcss-macos-arm64" ;;
  *)
    echo "unsupported platform: ${OS}-${ARCH}" >&2
    exit 1
    ;;
esac

URL="https://github.com/tailwindlabs/tailwindcss/releases/download/${VERSION}/${ASSET}"
echo "downloading ${URL}"
curl -fsSL -o "${BIN_PATH}" "${URL}"
chmod +x "${BIN_PATH}"
echo "installed: ${BIN_PATH} (${VERSION})"
