#!/usr/bin/env python3
"""Build and package NFR release archives for Windows, Linux, macOS, and Android.

Produces one self-contained archive per (platform, arch) pair inside dist/:
  - Linux:   nfr_<version>_linux_{amd64,arm64,armv7}.tar.gz
  - macOS:   nfr_<version>_macos_{arm64,amd64}.tar.gz
  - Windows: nfr_<version>_windows_{amd64,arm64}.zip
  - Android: nfr_<version>_android_arm64.tar.gz

Plus dist/SHA256SUMS covering every generated archive.
"""

import argparse
import hashlib
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import zipfile
from datetime import datetime, timezone

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MODULE = "github.com/solitudeit/nfr"

# (label_os, label_arch, GOOS, GOARCH, GOARM, archive_ext)
TARGETS = [
    ("linux", "amd64", "linux", "amd64", "", "tar.gz"),
    ("linux", "arm64", "linux", "arm64", "", "tar.gz"),
    ("linux", "armv7", "linux", "arm", "7", "tar.gz"),
    ("macos", "arm64", "darwin", "arm64", "", "tar.gz"),
    ("macos", "amd64", "darwin", "amd64", "", "tar.gz"),
    ("windows", "amd64", "windows", "amd64", "", "zip"),
    ("windows", "arm64", "windows", "arm64", "", "zip"),
    ("android", "arm64", "android", "arm64", "", "tar.gz"),
]


def git_output(args, default):
    try:
        out = subprocess.check_output(
            ["git"] + args, cwd=ROOT, stderr=subprocess.DEVNULL, text=True
        ).strip()
        return out or default
    except Exception:
        return default


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


def build_binary(pkg, out_path, goos, goarch, goarm, ldflags):
    env = os.environ.copy()
    env["CGO_ENABLED"] = "0"
    env["GOOS"] = goos
    env["GOARCH"] = goarch
    if goarm:
        env["GOARM"] = goarm
    else:
        env.pop("GOARM", None)
    cmd = ["go", "build", "-trimpath", "-ldflags", ldflags, "-o", out_path, pkg]
    subprocess.check_call(cmd, cwd=ROOT, env=env)


def pack_tar_gz(src_dir, top_name, dst_path):
    with tarfile.open(dst_path, "w:gz") as tf:
        for dirpath, dirnames, filenames in os.walk(src_dir):
            dirnames.sort()
            filenames.sort()
            rel_dir = os.path.relpath(dirpath, src_dir)
            for fname in filenames:
                full = os.path.join(dirpath, fname)
                arc = (
                    os.path.join(top_name, fname)
                    if rel_dir == "."
                    else os.path.join(top_name, rel_dir, fname)
                )
                ti = tf.gettarinfo(full, arcname=arc)
                ti.uid = 0
                ti.gid = 0
                ti.uname = ""
                ti.gname = ""
                with open(full, "rb") as f:
                    tf.addfile(ti, f)


def pack_zip(src_dir, top_name, dst_path):
    with zipfile.ZipFile(dst_path, "w", compression=zipfile.ZIP_DEFLATED) as zf:
        for dirpath, dirnames, filenames in os.walk(src_dir):
            dirnames.sort()
            filenames.sort()
            rel_dir = os.path.relpath(dirpath, src_dir)
            for fname in filenames:
                full = os.path.join(dirpath, fname)
                arc = (
                    os.path.join(top_name, fname)
                    if rel_dir == "."
                    else os.path.join(top_name, rel_dir, fname)
                )
                zf.write(full, arcname=arc)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument(
        "--version",
        default=os.environ.get("VERSION") or git_output(["describe", "--tags", "--always"], "dev"),
    )
    ap.add_argument("--out", default=os.path.join(ROOT, "dist"))
    args = ap.parse_args()

    version = args.version
    commit = git_output(["rev-parse", "--short", "HEAD"], "none")
    date = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    ldflags = (
        f"-s -w "
        f"-X {MODULE}/internal/version.Version={version} "
        f"-X {MODULE}/internal/version.Commit={commit} "
        f"-X {MODULE}/internal/version.Date={date}"
    )

    out_dir = os.path.abspath(args.out)
    os.makedirs(out_dir, exist_ok=True)

    archives = []
    for label_os, label_arch, goos, goarch, goarm, ext in TARGETS:
        top_name = f"nfr_{version}_{label_os}_{label_arch}"
        archive_name = f"{top_name}.{ext}"
        archive_path = os.path.join(out_dir, archive_name)
        bin_ext = ".exe" if goos == "windows" else ""

        print(f"building {archive_name} ({goos}/{goarch})...")
        stage = tempfile.mkdtemp(prefix=f"nfr-pkg-{label_os}-{label_arch}-")
        try:
            nfrs_bin = os.path.join(stage, "nfrs" + bin_ext)
            nfrc_bin = os.path.join(stage, "nfrc" + bin_ext)
            build_binary("./cmd/nfrs", nfrs_bin, goos, goarch, goarm, ldflags)
            build_binary("./cmd/nfrc", nfrc_bin, goos, goarch, goarm, ldflags)
            if goos != "windows":
                os.chmod(nfrs_bin, 0o755)
                os.chmod(nfrc_bin, 0o755)

            shutil.copy2(os.path.join(ROOT, "LICENSE"), os.path.join(stage, "LICENSE"))
            shutil.copy2(os.path.join(ROOT, "README.md"), os.path.join(stage, "README.md"))

            cfg_src = os.path.join(ROOT, "configs")
            if os.path.isdir(cfg_src):
                shutil.copytree(cfg_src, os.path.join(stage, "configs"))

            if label_os == "linux":
                sysd_dst = os.path.join(stage, "systemd")
                os.makedirs(sysd_dst, exist_ok=True)
                shutil.copy2(
                    os.path.join(ROOT, "deploy", "systemd", "nfr-server.service"),
                    os.path.join(sysd_dst, "nfr-server.service"),
                )
                shutil.copy2(
                    os.path.join(ROOT, "deploy", "systemd", "nfr-agent.service"),
                    os.path.join(sysd_dst, "nfr-agent.service"),
                )

            if ext == "zip":
                pack_zip(stage, top_name, archive_path)
            else:
                pack_tar_gz(stage, top_name, archive_path)
            archives.append(archive_name)

            # Also emit a version-free lightweight client-only archive
            # (nfrc_<os>_<arch>.<ext>) containing strictly nfrc / nfrc.exe at
            # the archive root for fixed /releases/latest/download/ links.
            client_archive_name = f"nfrc_{label_os}_{label_arch}.{ext}"
            client_archive_path = os.path.join(out_dir, client_archive_name)
            client_bin_name = "nfrc" + bin_ext
            if ext == "zip":
                with zipfile.ZipFile(client_archive_path, "w", compression=zipfile.ZIP_DEFLATED) as zf:
                    zf.write(nfrc_bin, arcname=client_bin_name)
            else:
                with tarfile.open(client_archive_path, "w:gz") as tf:
                    ti = tf.gettarinfo(nfrc_bin, arcname=client_bin_name)
                    ti.uid = 0
                    ti.gid = 0
                    ti.uname = ""
                    ti.gname = ""
                    ti.mode = 0o755
                    with open(nfrc_bin, "rb") as f:
                        tf.addfile(ti, f)
            archives.append(client_archive_name)
        finally:
            shutil.rmtree(stage, ignore_errors=True)

    sums_path = os.path.join(out_dir, "SHA256SUMS")
    with open(sums_path, "w", encoding="utf-8") as f:
        for name in sorted(archives):
            digest = sha256_file(os.path.join(out_dir, name))
            f.write(f"{digest}  {name}\n")

    print("\nSHA256SUMS:")
    with open(sums_path, "r", encoding="utf-8") as f:
        sys.stdout.write(f.read())


if __name__ == "__main__":
    main()
