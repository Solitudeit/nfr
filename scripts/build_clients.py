#!/usr/bin/env python3
"""Cross-compile and pack lightweight per-platform nfrc client archives.

Each archive is named by OS + arch (e.g. nfrc_linux_amd64.tar.gz or
nfrc_windows_amd64.zip) and contains a single short-named binary ("nfrc" or
"nfrc.exe") at the archive root so users can extract and run directly.

Usage:
    python3 scripts/build_clients.py --out /var/lib/nfr/clients
"""

import argparse
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

# (label_os, label_arch, goos, goarch, goarm, ext)
TARGETS = [
    ("windows", "amd64", "windows", "amd64", "", "zip"),
    ("windows", "arm64", "windows", "arm64", "", "zip"),
    ("linux", "amd64", "linux", "amd64", "", "tar.gz"),
    ("linux", "arm64", "linux", "arm64", "", "tar.gz"),
    ("linux", "armv7", "linux", "arm", "7", "tar.gz"),
    ("macos", "arm64", "darwin", "arm64", "", "tar.gz"),
    ("macos", "amd64", "darwin", "amd64", "", "tar.gz"),
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


def pack_single_binary(bin_path, arc_name, dst_path, ext):
    tmp_archive = dst_path + ".tmp"
    if ext == "zip":
        with zipfile.ZipFile(tmp_archive, "w", compression=zipfile.ZIP_DEFLATED) as zf:
            zf.write(bin_path, arcname=arc_name)
    else:
        with tarfile.open(tmp_archive, "w:gz") as tf:
            ti = tf.gettarinfo(bin_path, arcname=arc_name)
            ti.uid = 0
            ti.gid = 0
            ti.uname = ""
            ti.gname = ""
            ti.mode = 0o755
            with open(bin_path, "rb") as f:
                tf.addfile(ti, f)
    os.replace(tmp_archive, dst_path)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=os.path.join(ROOT, "bin", "clients"))
    ap.add_argument(
        "--version",
        default=os.environ.get("VERSION") or git_output(["describe", "--tags", "--always"], "dev"),
    )
    args = ap.parse_args()

    out_dir = os.path.abspath(args.out)
    os.makedirs(out_dir, exist_ok=True)

    commit = git_output(["rev-parse", "--short", "HEAD"], "none")
    date = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    ldflags = (
        f"-s -w "
        f"-X {MODULE}/internal/version.Version={args.version} "
        f"-X {MODULE}/internal/version.Commit={commit} "
        f"-X {MODULE}/internal/version.Date={date}"
    )

    for label_os, label_arch, goos, goarch, goarm, ext in TARGETS:
        archive_name = f"nfrc_{label_os}_{label_arch}.{ext}"
        dst = os.path.join(out_dir, archive_name)
        bin_name = "nfrc.exe" if goos == "windows" else "nfrc"
        stage = tempfile.mkdtemp(prefix=f"nfrc-pkg-{label_os}-{label_arch}-")
        try:
            bin_path = os.path.join(stage, bin_name)
            env = os.environ.copy()
            env["CGO_ENABLED"] = "0"
            env["GOOS"] = goos
            env["GOARCH"] = goarch
            if goarm:
                env["GOARM"] = goarm
            else:
                env.pop("GOARM", None)
            print(f"building {archive_name} ({goos}/{goarch} -> {bin_name})...")
            subprocess.check_call(
                ["go", "build", "-trimpath", "-ldflags", ldflags, "-o", bin_path, "./cmd/nfrc"],
                cwd=ROOT,
                env=env,
            )
            if goos != "windows":
                os.chmod(bin_path, 0o755)
            pack_single_binary(bin_path, bin_name, dst, ext)
        finally:
            shutil.rmtree(stage, ignore_errors=True)

    print(f"done: {len(TARGETS)} client archives written to {out_dir}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
