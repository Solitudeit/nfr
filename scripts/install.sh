#!/bin/sh
# Install the latest nfr release for this machine.
#
#   curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh | sh
#
# Environment:
#   NFR_VERSION  pin a release (e.g. v0.1.0), default: latest
#   NFR_PREFIX   install directory, default: /usr/local/bin
set -eu

repo="Solitudeit/nfr"
prefix="${NFR_PREFIX:-/usr/local/bin}"
version="${NFR_VERSION:-latest}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux) ;;
  darwin) ;;
  *) echo "unsupported system: $os" >&2; exit 1 ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  armv7l|armv7) arch=armv7 ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

if [ "$version" = latest ]; then
  base="https://github.com/$repo/releases/latest/download"
else
  base="https://github.com/$repo/releases/download/$version"
fi
url="$base/nfr-$os-$arch"
if [ "$os" = windows ]; then url="$url.exe"; fi

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$2" "$1"
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c 'import sys, urllib.request; urllib.request.urlretrieve(sys.argv[1], sys.argv[2])' "$1" "$2"
  else
    echo "need curl, wget or python3 to download the release" >&2
    exit 1
  fi
}

echo "downloading $url"
fetch "$url" "$tmp"
chmod +x "$tmp"
mkdir -p "$prefix"
mv "$tmp" "$prefix/nfr"
trap - EXIT
echo "installed $prefix/nfr"
"$prefix/nfr" version
