#!/bin/sh
# Install or manage NFR (nfrs / nfrc) on Linux or macOS.
#
# Public server (binaries + systemd service):
#   curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh \
#     | sudo sh -s -- --server --systemd
#
#   # Optional: set the initial console password directly instead of random:
#   curl -fsSL .../install.sh | sudo NFR_PASSWORD='my-strong-password' sh -s -- --server --systemd
#
# Private machine (install nfrc + join + install & start systemd service in one command):
#   curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh \
#     | sudo sh -s -- --client --join example.com:8443 ABC123 --systemd
#
# Binaries only (no systemd):
#   curl -fsSL .../install.sh | sudo sh
#
# Uninstall binaries and systemd units (keeps /etc/nfr and /var/lib/nfr):
#   curl -fsSL .../install.sh | sudo sh -s -- --uninstall
#
# Environment:
#   NFR_VERSION   pin a release (e.g. v0.11.0), default: latest
#   NFR_PREFIX    install directory, default: /usr/local/bin
#   NFR_ONLY      "server" or "client" to install just one binary
#   NFR_SYSTEMD   set to 1 to install and start the corresponding systemd unit
#   NFR_PASSWORD  initial admin password on a fresh server install (>= 8 chars)
set -eu

repo="Solitudeit/nfr"
prefix="${NFR_PREFIX:-/usr/local/bin}"
version="${NFR_VERSION:-latest}"
only="${NFR_ONLY:-}"
systemd="${NFR_SYSTEMD:-}"
password="${NFR_PASSWORD:-}"
join_server=""
join_code=""
join_name=""
uninstall=0

while [ "$#" -gt 0 ]; do
  case "$1" in
    --systemd) systemd=1 ;;
    --server) only=server ;;
    --client) only=client ;;
    --uninstall) uninstall=1 ;;
    --password)
      shift
      [ "$#" -gt 0 ] || { echo "--password requires a value" >&2; exit 1; }
      password="$1"
      ;;
    --name)
      shift
      [ "$#" -gt 0 ] || { echo "--name requires a value" >&2; exit 1; }
      join_name="$1"
      ;;
    --join)
      shift
      [ "$#" -ge 2 ] || { echo "--join requires <server> <code>" >&2; exit 1; }
      join_server="$1"
      join_code="$2"
      shift
      if [ -z "$only" ]; then only=client; fi
      ;;
    -h|--help)
      sed -n '2,27p' "$0" 2>/dev/null || echo "see comments at the top of install.sh"
      exit 0
      ;;
    *) echo "unknown option: $1" >&2; exit 1 ;;
  esac
  shift
done

do_uninstall() {
  if [ "$(id -u)" != 0 ]; then
    echo "--uninstall needs root (try: ... | sudo sh -s -- --uninstall)" >&2
    exit 1
  fi
  if [ -d /run/systemd/system ]; then
    for unit in nfr-server nfr-agent; do
      if systemctl list-unit-files "$unit.service" >/dev/null 2>&1; then
        systemctl disable --now "$unit" 2>/dev/null || true
      fi
      rm -f "/etc/systemd/system/$unit.service"
    done
    systemctl daemon-reload 2>/dev/null || true
  fi
  rm -f "$prefix/nfrs" "$prefix/nfrc"
  echo "uninstalled nfrs/nfrc binaries and systemd units."
  echo "configuration (/etc/nfr) and state (/var/lib/nfr) were preserved; remove them manually if no longer needed."
  exit 0
}

if [ "$uninstall" = 1 ]; then
  do_uninstall
fi

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

sums_file=""
fetch_sums() {
  if [ -n "$sums_file" ]; then return 0; fi
  sums_file=$(mktemp)
  if ! fetch "$base/SHA256SUMS" "$sums_file" 2>/dev/null; then
    rm -f "$sums_file"
    sums_file=""
  fi
}

cleanup_sums() {
  if [ -n "$sums_file" ] && [ -f "$sums_file" ]; then
    rm -f "$sums_file"
  fi
}
trap cleanup_sums EXIT

verify_checksum() {
  asset="$1"
  file="$2"
  fetch_sums
  if [ -z "$sums_file" ]; then
    return 0
  fi
  expected=$(awk -v f="$asset" '$2 == f || $2 == ("*" f) { print $1; exit }' "$sums_file")
  if [ -z "$expected" ]; then
    return 0
  fi
  actual=""
  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$file" | awk '{print $1}')
  elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$file" | awk '{print $1}')
  else
    return 0
  fi
  if [ "$actual" != "$expected" ]; then
    echo "checksum mismatch for $asset: got $actual want $expected" >&2
    exit 1
  fi
}

install_one() {
  name="$1"
  asset="$name-$os-$arch"
  if [ "$os" = windows ]; then asset="$asset.exe"; fi
  url="$base/$asset"
  tmp=$(mktemp)
  echo "downloading $url"
  fetch "$url" "$tmp"
  verify_checksum "$asset" "$tmp"
  chmod +x "$tmp"
  mkdir -p "$prefix"
  mv "$tmp" "$prefix/$name"
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

install_server_systemd() {
  if [ "$(id -u)" != 0 ]; then
    echo "--systemd needs root (try: curl ... | sudo sh -s -- --server --systemd)" >&2
    exit 1
  fi
  if [ ! -d /run/systemd/system ]; then
    echo "systemd not detected; binaries are installed, start them yourself: $prefix/nfrs -c /etc/nfr/server.toml" >&2
    return 0
  fi
  # Stop any legacy -d/--daemon process before systemd binds control_listen.
  if out=$("$prefix/nfrs" stop -c /etc/nfr/server.toml 2>&1); then
    case "$out" in
      *没有正在运行*) ;;
      *) echo "$out" ;;
    esac
  fi
  data_dir=/var/lib/nfr
  mkdir -p /etc/nfr "$data_dir"
  unit=/etc/systemd/system/nfr-server.service
  tmp=$(mktemp)
  fetch "$base/nfr-server.service" "$tmp"
  mv "$tmp" "$unit"
  echo "installed $unit"

  fresh=0
  [ -f "$data_dir/state.json" ] || fresh=1
  if [ "$fresh" = 1 ] && [ -n "$password" ]; then
    "$prefix/nfrs" reset-password -c /etc/nfr/server.toml "$password"
    fresh=0
  fi

  was_active=$(systemctl is-active nfr-server 2>/dev/null || true)
  systemctl daemon-reload
  if [ "$was_active" = active ]; then
    systemctl restart nfr-server
  else
    systemctl enable --now nfr-server
  fi
  sleep 2
  echo
  systemctl status nfr-server --no-pager --lines=12 || true
  echo
  if [ "$fresh" = 1 ]; then
    echo "首次启动的登录信息（密码只打印这一次）："
  else
    echo "面板地址（密码沿用 $data_dir/state.json；忘了用 sudo nfrs reset-password）："
  fi
  shown=$(journalctl -u nfr-server --since '-3 min' --no-pager -o cat 2>/dev/null |
    grep -E '面板|密码|证书指纹' || true)
  if [ -n "$shown" ]; then
    echo "$shown"
  elif [ -f "$data_dir/initial-password.txt" ]; then
    echo "  （这台机器没有可读的 journal，下面是 nfrs 写下的启动信息）"
    cat "$data_dir/initial-password.txt"
    echo "  （登录成功后这个文件会自动删除）"
  else
    "$prefix/nfrs" status -c /etc/nfr/server.toml || true
  fi
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

install_client_systemd() {
  if [ "$(id -u)" != 0 ]; then
    echo "--systemd needs root (try: curl ... | sudo sh -s -- --client --join <server> <code> --systemd)" >&2
    exit 1
  fi
  if [ ! -d /run/systemd/system ]; then
    echo "systemd not detected; start client yourself: $prefix/nfrc -c /etc/nfr/agent.toml" >&2
    return 0
  fi
  mkdir -p /etc/nfr
  unit=/etc/systemd/system/nfr-agent.service
  tmp=$(mktemp)
  fetch "$base/nfr-agent.service" "$tmp"
  mv "$tmp" "$unit"
  echo "installed $unit"
  was_active=$(systemctl is-active nfr-agent 2>/dev/null || true)
  systemctl daemon-reload
  if [ "$was_active" = active ]; then
    systemctl restart nfr-agent
  else
    systemctl enable --now nfr-agent
  fi
  sleep 1
  echo
  systemctl status nfr-agent --no-pager --lines=10 || true
}

if command -v "$prefix/nfrs" >/dev/null 2>&1; then "$prefix/nfrs" version; fi
if command -v "$prefix/nfrc" >/dev/null 2>&1; then "$prefix/nfrc" version; fi

if [ -n "$join_server" ] && [ -n "$join_code" ]; then
  if [ -n "$join_name" ]; then
    "$prefix/nfrc" join --no-start --name "$join_name" "$join_server" "$join_code"
  else
    "$prefix/nfrc" join --no-start "$join_server" "$join_code"
  fi
fi

if [ -n "$systemd" ]; then
  case "$only" in
    client) install_client_systemd ;;
    *) install_server_systemd ;;
  esac
fi
