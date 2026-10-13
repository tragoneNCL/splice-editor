#!/bin/sh
# Install Splice on macOS or Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/tragoneNCL/splice-editor/main/install.sh | sh
#
# Downloads the latest release for this machine, verifies its SHA-256 checksum,
# installs it under ~/.local/share/splice with a `splice` command in
# ~/.local/bin, and installs ffmpeg if it is missing. Nothing needs sudo.
set -eu

REPO="${SPLICE_REPO:-tragoneNCL/splice-editor}"
API="${SPLICE_API_BASE:-https://api.github.com}"
APP_DIR="${SPLICE_DEST:-$HOME/.local/share/splice}"
BIN_DIR="${SPLICE_BIN:-$HOME/.local/bin}"

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

have curl || die "curl is required"
have tar || die "tar is required"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) die "this installer supports macOS and Linux (on Windows use install.ps1)" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) die "unsupported CPU: $(uname -m)" ;;
esac
[ "$os-$arch" = "linux-arm64" ] && die "no Linux arm64 build yet"

sha256() {
  if have sha256sum; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

say "Looking up the latest Splice release..."
json=$(curl -fsSL -H 'Accept: application/vnd.github+json' "$API/repos/$REPO/releases/latest") \
  || die "could not reach the release server (is there a published release yet?)"
# Split on commas so this works whether the JSON is pretty-printed or compact.
flat=$(printf '%s' "$json" | tr ',' '\n')
tag=$(printf '%s\n' "$flat" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
[ -n "$tag" ] || die "no release found"
asset="splice-${tag}-${os}-${arch}.tar.gz"
url=$(printf '%s\n' "$flat" | sed -n 's/.*"browser_download_url": *"\([^"]*\)".*/\1/p' | grep "/${asset}\$" | head -1)
sums=$(printf '%s\n' "$flat" | sed -n 's/.*"browser_download_url": *"\([^"]*\)".*/\1/p' | grep '/checksums.txt$' | head -1)
[ -n "$url" ] || die "release $tag has no build for $os/$arch"
[ -n "$sums" ] || die "release $tag has no checksums.txt; refusing to install unverified code"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
say "Downloading $asset ..."
curl -fsSL "$url" -o "$tmp/$asset" || die "download failed"
curl -fsSL "$sums" -o "$tmp/checksums.txt" || die "could not download checksums"

want=$(grep " \*\?${asset}\$" "$tmp/checksums.txt" | cut -d' ' -f1 | head -1)
[ -n "$want" ] || die "no checksum listed for $asset"
got=$(sha256 "$tmp/$asset")
[ "$want" = "$got" ] || die "checksum mismatch; not installing"
say "Checksum verified."

tar -xzf "$tmp/$asset" -C "$tmp"
src=$(find "$tmp" -type f -name splice | head -1)
[ -n "$src" ] || die "the archive did not contain the app"

mkdir -p "$APP_DIR" "$BIN_DIR"
cp "$src" "$APP_DIR/splice.new"
chmod 755 "$APP_DIR/splice.new"
mv -f "$APP_DIR/splice.new" "$APP_DIR/splice"
ln -sf "$APP_DIR/splice" "$BIN_DIR/splice"
[ "$os" = darwin ] && xattr -dr com.apple.quarantine "$APP_DIR/splice" 2>/dev/null || true

if [ "${SPLICE_SKIP_FFMPEG:-0}" != 1 ] && ! { have ffmpeg && have ffprobe; }; then
  say "ffmpeg is needed to process video; installing it..."
  if have brew; then brew install ffmpeg
  elif have apt-get && [ "$(id -u)" = 0 ]; then apt-get install -y ffmpeg
  elif have apt-get; then say "Run:  sudo apt-get install -y ffmpeg"
  elif have dnf; then say "Run:  sudo dnf install -y ffmpeg"
  else say "Please install ffmpeg (https://ffmpeg.org/download.html), then start Splice."; fi
fi

say ""
say "Installed Splice $tag  ->  $APP_DIR/splice"
case ":$PATH:" in *":$BIN_DIR:"*) ;; *) say "Add $BIN_DIR to your PATH, or run it directly: $BIN_DIR/splice -open" ;; esac
say "Start it with:  splice -open"
say "It updates itself; you only install once."
