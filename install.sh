#!/bin/sh
# f2f installer for macOS and Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/vseplet/f2f/main/install.sh | sh
#
# Downloads the release binary matching this host's OS/arch from GitHub Releases
# and installs it to a bin dir on PATH. Override with env vars:
#   F2F_VERSION=v0.1.0   pin a version (default: latest release)
#   F2F_BIN_DIR=~/.local/bin   install location (default: /usr/local/bin)
set -eu

REPO="vseplet/f2f"
VERSION="${F2F_VERSION:-latest}"
BIN_DIR="${F2F_BIN_DIR:-/usr/local/bin}"

say()  { printf '%s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# --- detect target -----------------------------------------------------------

os="$(uname -s)"
case "$os" in
  Darwin) os="darwin" ;;
  Linux)  os="linux" ;;
  *) die "unsupported OS: $os (this installer covers macOS and Linux; use install.ps1 on Windows)" ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) die "unsupported architecture: $arch" ;;
esac

# We publish darwin only for Apple Silicon.
if [ "$os" = "darwin" ] && [ "$arch" != "arm64" ]; then
  die "macOS builds are Apple Silicon (arm64) only; got $arch"
fi

asset="f2f-${os}-${arch}"

# --- resolve download URL ----------------------------------------------------

if [ "$VERSION" = "latest" ]; then
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
fi

command -v curl >/dev/null 2>&1 || die "curl is required"

say "downloading $asset ($VERSION)..."
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
# -4: force IPv4. Some networks advertise IPv6 (AAAA records resolve) but have
# no working v6 route, so a default dual-stack curl picks the v6 address and
# hangs until timeout. GitHub is reachable over IPv4 everywhere we care about.
# -f fails on HTTP errors (a missing asset otherwise saves the 404 page).
curl -4 -fSL --proto '=https' --connect-timeout 20 "$url" -o "$tmp" \
  || die "download failed: $url (does a release exist for $VERSION?)"

chmod +x "$tmp"

# --- install -----------------------------------------------------------------

dest="${BIN_DIR%/}/f2f"
if mkdir -p "$BIN_DIR" 2>/dev/null && [ -w "$BIN_DIR" ]; then
  mv "$tmp" "$dest"
  chmod +x "$dest"
else
  say "installing to $BIN_DIR needs elevated rights; using sudo"
  sudo mkdir -p "$BIN_DIR"
  sudo mv "$tmp" "$dest"
  sudo chmod +x "$dest"
fi
trap - EXIT

say "installed: $dest"
"$dest" version 2>/dev/null || true

# --- notes -------------------------------------------------------------------

case ":$PATH:" in
  *":${BIN_DIR%/}:"*) ;;
  *) say ""; say "note: $BIN_DIR is not on your PATH — add it or run $dest directly" ;;
esac

say ""
say "f2f needs root to create the tunnel and routes. run it with:"
say "  sudo f2f"
