#!/bin/sh
#
# shipd CLI installer.
#
#   curl -fsSL https://gt.plainskill.net/plainskill/shipd/raw/branch/main/install.sh | sh
#
# Downloads the latest release binary for this platform, verifies its
# sha256 against the release SHA256SUMS, and installs it to ~/.local/bin
# (or $SHIPD_BIN_DIR). Set SHIPD_VERSION to pin a tag.
#
set -eu

RELEASE_BASE="${SHIPD_RELEASE_BASE:-https://gt.plainskill.net/plainskill/shipd}"
BIN_DIR="${SHIPD_BIN_DIR:-$HOME/.local/bin}"
VERSION="${SHIPD_VERSION:-}"

say()  { printf 'shipd-install: %s\n' "$*"; }
die()  { printf 'shipd-install: ERROR: %s\n' "$*" >&2; exit 1; }

# --- platform ---------------------------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  *) die "unsupported OS: $os (linux and darwin only)" ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac
asset="shipd-cli-${os}-${arch}"
say "platform: ${os}/${arch} → ${asset}"

# --- resolve release --------------------------------------------------
if [ -z "$VERSION" ]; then
  # derive the API endpoint from the repo base so any forge works
  origin=$(printf '%s' "$RELEASE_BASE" | sed -E 's|^(https?://[^/]+).*|\1|')
  repo_path=$(printf '%s' "$RELEASE_BASE" | sed -E 's|^https?://[^/]+/||')
  api="$origin/api/v1/repos/$repo_path/releases?limit=1"
  VERSION=$(curl -fsSL "$api" 2>/dev/null | sed -n 's/.*"tag_name":"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$VERSION" ] || die "could not determine the latest release from $api"
fi
say "version: $VERSION"

base="$RELEASE_BASE/releases/download/$VERSION"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "downloading $asset"
curl -fsSL "$base/$asset" -o "$tmp/$asset" || die "download failed: $base/$asset"
curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS" || die "download failed: $base/SHA256SUMS"

# --- verify -----------------------------------------------------------
want=$(grep " $asset\$" "$tmp/SHA256SUMS" | awk '{print $1}')
[ -n "$want" ] || die "no checksum for $asset in SHA256SUMS"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$asset" | awk '{print $1}')
else
  got=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
fi
[ "$want" = "$got" ] || die "checksum mismatch for $asset (want $want, got $got)"
say "checksum ok"

# --- install ----------------------------------------------------------
mkdir -p "$BIN_DIR"
install -m 0755 "$tmp/$asset" "$BIN_DIR/shipd"
say "installed $BIN_DIR/shipd ($VERSION)"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) say "NOTE: $BIN_DIR is not in PATH — add: export PATH=\"$BIN_DIR:\$PATH\"" ;;
esac

say "next: shipd login --server https://your-shipd-host"
