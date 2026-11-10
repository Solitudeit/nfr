#!/bin/sh
# Install the latest nfr release for this machine.
#
#   curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh | sh
#
# Two binaries are installed:
#   nfrs  the control node, for the public server
#   nfrc  the client, for the machines inside your network
#
# Environment:
#   NFR_VERSION  pin a release (e.g. v0.9.0), default: latest
#   NFR_PREFIX   install directory, default: /usr/local/bin
#   NFR_ONLY     "server" or "client" to install just one of them
#   NFR_SYSTEMD  set to 1 to also install nfr-server.service and start it
#                (server only, Linux with systemd, must run as root)
#
# Pass --systemd as the first argument to do the same without env vars:
#   curl -fsSL .../install.sh | sudo sh -s -- --systemd
set -eu

repo="Solitudeit/nfr"
prefix="${NFR_PREFIX:-/usr/local/bin}"
version="${NFR_VERSION:-latest}"
only="${NFR_ONLY:-}"
systemd="${NFR_SYSTEMD:-}"
for arg in "$@"; do
  case "$arg" in
    --systemd) systemd=1 ;;
    --server) only=server ;;
    --client) only=client ;;
    -h|--help)
      sed -n '2,20p' "$0" 2>/dev/null || echo "see comments at the top of install.sh"
      exit 0
      ;;
    *) echo "unknown option: $arg" >&2; exit 1 ;;
  esac
done

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

install_one() {
  name="$1"
  url="$base/$name-$os-$arch"
  if [ "$os" = windows ]; then url="$url.exe"; fi
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' EXIT
  echo "downloading $url"
  fetch "$url" "$tmp"
  chmod +x "$tmp"
  mkdir -p "$prefix"
  mv "$tmp" "$prefix/$name"
  trap - EXIT
  echo "installed $prefix/$name"
}

case "$only" in
  server) install_one nfrs ;;
  client) install_one nfrc ;;
  *)
    install_one nfrs
    install_one nfrc
    ;;
esac

install_systemd() {
  if [ "$(id -u)" != 0 ]; then
    echo "NFR_SYSTEMD=1 needs root (try: curl ... | sudo NFR_SYSTEMD=1 sh)" >&2
    exit 1
  fi
  if [ ! -d /run/systemd/system ]; then
    echo "systemd not detected; binaries are installed, start them yourself" >&2
    return 0
  fi
  # A server started by hand (bare `nfrs`) holds the control port; systemd
  # cannot bind it. Stop it first — state survives, only the process goes.
  # `stop` reads data_dir from the config, so this also works when the data
  # directory was moved; a quiet "nothing running" answer is expected.
  if out=$("$prefix/nfrs" stop -c /etc/nfr/server.toml 2>&1); then
    case "$out" in
      *没有正在运行*) ;;
      *) echo "$out" ;;
    esac
  fi
  # The unit uses StateDirectory=/ConfigurationDirectory=, which makes systemd
  # create these; doing it here too keeps ReadWritePaths-style overrides and
  # older units working.
  mkdir -p /etc/nfr /var/lib/nfr
  unit=/etc/systemd/system/nfr-server.service
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' EXIT
  fetch "$base/nfr-server.service" "$tmp"
  mv "$tmp" "$unit"
  trap - EXIT
  echo "installed $unit"
  systemctl daemon-reload
  systemctl enable --now nfr-server
  # First start writes the config and prints the panel password to the journal.
  sleep 2
  echo
  systemctl status nfr-server --no-pager --lines=12 || true
  if ! systemctl is-active --quiet nfr-server; then
    echo
    echo "nfr-server did not stay up; last journal lines:" >&2
    journalctl -u nfr-server -n 20 --no-pager >&2 || true
    echo
    echo "binaries are installed; fix the cause and run:" >&2
    echo "  sudo systemctl restart nfr-server" >&2
    exit 1
  fi
}

# Report what landed. A partial install (NFR_ONLY) skips the other binary.
if command -v "$prefix/nfrs" >/dev/null 2>&1; then "$prefix/nfrs" version; fi
if command -v "$prefix/nfrc" >/dev/null 2>&1; then "$prefix/nfrc" version; fi

if [ -n "$systemd" ]; then
  case "$only" in
    client) echo "NFR_SYSTEMD ignored with NFR_ONLY=client" >&2 ;;
    *) install_systemd ;;
  esac
fi
