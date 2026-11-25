#!/bin/sh
# Install or manage NFR (nfrs / nfrc) on Linux, macOS, or Android (Termux).
#
# Public server (binaries + systemd service):
#   curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh \
#     | sudo sh -s -- --server --systemd
#
#   # Optional: preset admin password & shared auth_token on fresh install:
#   curl -fsSL .../install.sh \
#     | sudo NFR_PASSWORD='change-me-now' NFR_AUTH_TOKEN='my-shared-token' sh -s -- --server --systemd
#
# Private machine (install nfrc + write client.toml/client.yaml + enable systemd):
#   curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh \
#     | sudo sh -s -- --client --server-addr 203.0.113.10:8443 --token my-shared-token --name nas --systemd
#
#   # Prefer YAML format:
#   curl -fsSL .../install.sh \
#     | sudo sh -s -- --client --server-addr 203.0.113.10:8443 --token my-shared-token --format yaml --systemd
#
# Binaries only (Linux / macOS / Android Termux):
#   curl -fsSL .../install.sh | sh
#
# Uninstall binaries and systemd units (keeps /etc/nfr and /var/lib/nfr):
#   curl -fsSL .../install.sh | sudo sh -s -- --uninstall
#
# Environment:
#   NFR_VERSION     pin a release (e.g. v0.12.0), default: latest
#   NFR_PREFIX      install directory (default: /usr/local/bin, or $PREFIX/bin on Termux)
#   NFR_ONLY        "server" or "client" to install just one binary
#   NFR_SYSTEMD     set to 1 to install and start the corresponding systemd unit
#   NFR_PASSWORD    initial admin password on a fresh server install (>= 8 chars)
#   NFR_AUTH_TOKEN  shared client auth_token (written to server/client config when given)
set -eu

repo="Solitudeit/nfr"
default_prefix="/usr/local/bin"
if [ -n "${PREFIX:-}" ] && [ -d "${PREFIX}/bin" ] && [ -d /system/bin ]; then
  default_prefix="${PREFIX}/bin"
fi
prefix="${NFR_PREFIX:-$default_prefix}"
version="${NFR_VERSION:-latest}"
only="${NFR_ONLY:-}"
systemd="${NFR_SYSTEMD:-}"
password="${NFR_PASSWORD:-}"
auth_token="${NFR_AUTH_TOKEN:-}"
server_addr=""
node_name=""
cfg_format="toml"
join_server=""
join_code=""
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
    --token)
      shift
      [ "$#" -gt 0 ] || { echo "--token requires a value" >&2; exit 1; }
      auth_token="$1"
      ;;
    --server-addr)
      shift
      [ "$#" -gt 0 ] || { echo "--server-addr requires <host:port>" >&2; exit 1; }
      server_addr="$1"
      if [ -z "$only" ]; then only=client; fi
      ;;
    --name)
      shift
      [ "$#" -gt 0 ] || { echo "--name requires a value" >&2; exit 1; }
      node_name="$1"
      ;;
    --format)
      shift
      [ "$#" -gt 0 ] || { echo "--format requires toml or yaml" >&2; exit 1; }
      case "$1" in
        toml|yaml|yml) cfg_format="$1" ;;
        *) echo "unsupported --format: $1 (use toml or yaml)" >&2; exit 1 ;;
      esac
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
      sed -n '2,32p' "$0" 2>/dev/null || echo "see comments at the top of install.sh"
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

raw_os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$raw_os" in
  linux)
    if [ "$(uname -o 2>/dev/null || true)" = "Android" ] || [ -d /system/bin ]; then
      platform=android
    else
      platform=linux
    fi
    ;;
  darwin) platform=macos ;;
  *) echo "unsupported system: $raw_os" >&2; exit 1 ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  armv7l|armv7) arch=armv7 ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

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

resolve_latest_tag() {
  url="https://github.com/$repo/releases/latest"
  tag=""
  if command -v curl >/dev/null 2>&1; then
    final=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$url" 2>/dev/null || true)
    tag=$(printf '%s' "$final" | sed -n 's#.*/tag/\([^/?]*\).*#\1#p')
  fi
  if [ -z "$tag" ] && command -v python3 >/dev/null 2>&1; then
    tag=$(python3 -c 'import json, urllib.request; print(json.load(urllib.request.urlopen("https://api.github.com/repos/'"$repo"'/releases/latest"))["tag_name"])' 2>/dev/null || true)
  fi
  if [ -z "$tag" ]; then
    echo "could not resolve latest release tag; set NFR_VERSION=vX.Y.Z explicitly" >&2
    exit 1
  fi
  printf '%s' "$tag"
}

if [ "$version" = latest ]; then
  tag=$(resolve_latest_tag)
else
  tag="$version"
fi
base="https://github.com/$repo/releases/download/$tag"

workdir=$(mktemp -d)
cleanup() {
  rm -rf "$workdir"
}
trap cleanup EXIT

sums_file="$workdir/SHA256SUMS"
has_sums=0
if fetch "$base/SHA256SUMS" "$sums_file" 2>/dev/null; then
  has_sums=1
fi

verify_checksum() {
  asset="$1"
  file="$2"
  if [ "$has_sums" != 1 ]; then
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

install_from_archive() {
  archive="nfr_${tag}_${platform}_${arch}.tar.gz"
  archive_path="$workdir/$archive"
  echo "downloading $base/$archive"
  fetch "$base/$archive" "$archive_path"
  verify_checksum "$archive" "$archive_path"
  extract_dir="$workdir/extract"
  mkdir -p "$extract_dir" "$prefix"
  tar -xzf "$archive_path" -C "$extract_dir"
  top_dir="$extract_dir/nfr_${tag}_${platform}_${arch}"
  case "$only" in
    server)
      cp "$top_dir/nfrs" "$prefix/nfrs"
      chmod +x "$prefix/nfrs"
      echo "installed $prefix/nfrs"
      ;;
    client)
      cp "$top_dir/nfrc" "$prefix/nfrc"
      chmod +x "$prefix/nfrc"
      echo "installed $prefix/nfrc"
      ;;
    *)
      cp "$top_dir/nfrs" "$prefix/nfrs"
      cp "$top_dir/nfrc" "$prefix/nfrc"
      chmod +x "$prefix/nfrs" "$prefix/nfrc"
      echo "installed $prefix/nfrs"
      echo "installed $prefix/nfrc"
      ;;
  esac
}

install_from_archive

install_server_systemd() {
  if [ "$(id -u)" != 0 ]; then
    echo "--systemd needs root (try: curl ... | sudo sh -s -- --server --systemd)" >&2
    exit 1
  fi
  if [ ! -d /run/systemd/system ]; then
    echo "systemd not detected; start server yourself: $prefix/nfrs" >&2
    return 0
  fi
  if out=$("$prefix/nfrs" stop 2>&1); then
    case "$out" in
      *没有正在运行*) ;;
      *) echo "$out" ;;
    esac
  fi
  data_dir=/var/lib/nfr
  mkdir -p /etc/nfr "$data_dir"

  # Write a starter server config on a fresh install if none exists yet.
  if [ ! -f /etc/nfr/server.toml ] && [ ! -f /etc/nfr/server.yaml ] && [ ! -f /etc/nfr/server.yml ]; then
    srv_cfg="/etc/nfr/server.${cfg_format}"
    if [ -n "$auth_token" ]; then
      "$prefix/nfrs" init -c "$srv_cfg" --token "$auth_token"
    else
      "$prefix/nfrs" init -c "$srv_cfg"
    fi
  fi

  unit=/etc/systemd/system/nfr-server.service
  fetch "$base/nfr-server.service" "$workdir/nfr-server.service"
  mv "$workdir/nfr-server.service" "$unit"
  echo "installed $unit"

  fresh=0
  [ -f "$data_dir/state.json" ] || fresh=1
  if [ "$fresh" = 1 ] && [ -n "$password" ]; then
    "$prefix/nfrs" reset-password "$password"
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
    echo "面板与连接密钥（密码沿用 $data_dir/state.json；忘了用 sudo nfrs reset-password）："
  fi
  shown=$(journalctl -u nfr-server --since '-3 min' --no-pager -o cat 2>/dev/null |
    grep -E '面板|密码|连接密钥|证书指纹' || true)
  if [ -n "$shown" ]; then
    echo "$shown"
  elif [ -f "$data_dir/initial-password.txt" ]; then
    echo "  （这台机器没有可读的 journal，下面是 nfrs 写下的启动信息）"
    cat "$data_dir/initial-password.txt"
    echo "  （登录成功后这个文件会自动删除）"
  else
    "$prefix/nfrs" status || true
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
    echo "--systemd needs root (try: curl ... | sudo sh -s -- --client --server-addr <addr> --token <tok> --systemd)" >&2
    exit 1
  fi
  if [ ! -d /run/systemd/system ]; then
    echo "systemd not detected; start client yourself: $prefix/nfrc" >&2
    return 0
  fi
  mkdir -p /etc/nfr
  unit=/etc/systemd/system/nfr-agent.service
  fetch "$base/nfr-agent.service" "$workdir/nfr-agent.service"
  mv "$workdir/nfr-agent.service" "$unit"
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

if [ -n "$server_addr" ] && [ -n "$auth_token" ]; then
  if [ "$(id -u)" = 0 ]; then
    cli_cfg="/etc/nfr/client.${cfg_format}"
  else
    cli_cfg="${HOME:-.}/.config/nfr/client.${cfg_format}"
  fi
  if [ -n "$node_name" ]; then
    "$prefix/nfrc" init --force -c "$cli_cfg" --server "$server_addr" --token "$auth_token" --name "$node_name"
  else
    "$prefix/nfrc" init --force -c "$cli_cfg" --server "$server_addr" --token "$auth_token"
  fi
elif [ -n "$join_server" ] && [ -n "$join_code" ]; then
  if [ -n "$node_name" ]; then
    "$prefix/nfrc" join --no-start --name "$node_name" "$join_server" "$join_code"
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
