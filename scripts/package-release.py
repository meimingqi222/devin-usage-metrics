#!/usr/bin/env python3
"""Package MyGo output identically for CI acceptance and stable releases."""

import argparse
import hashlib
from pathlib import Path
import shutil
import subprocess
import zipfile

# Windows releases also publish a copy under the pre-branding archive name.
# Release publishing rewrites spaces in asset filenames, so the branded name
# cannot use them; clients older than this rename discover only the legacy
# name and would otherwise keep failing with "no matching asset".
WINDOWS_LEGACY_ARCHIVE = "devin-usage-metrics-windows-x64.zip"


def write_sidecar(archive: Path, digest: str) -> None:
    archive.with_suffix(".zip.sha256").write_text(
        f"{digest}  {archive.name}\n", encoding="utf-8"
    )


def package(target: str, build_root: Path, output: Path) -> Path:
    directory, asset = {
        "windows/amd64": ("windows-amd64", "windows-x64"),
        "darwin/arm64": ("darwin-arm64", "macos-arm64"),
    }[target]
    source = build_root.resolve() / directory
    output.mkdir(parents=True, exist_ok=True)
    # Windows release branding follows the native app's executable name. The
    # archive name avoids spaces because publishing replaces them with dots,
    # which breaks clients that match the asset name exactly.
    product = "Agent-Usage-Metrics" if target == "windows/amd64" else "devin-usage-metrics"
    archive = output.resolve() / f"{product}-{asset}.zip"
    if target == "windows/amd64":
        # Windows release branding follows the native app's executable name.
        executable = source / "Agent Usage Metrics.exe"
        with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as bundle:
            bundle.write(executable, executable.name)
    else:
        # Keep the native bundle layout, executable modes and signing resources.
        app = source / "Agent Usage Metrics.app"
        subprocess.run(["codesign", "--verify", "--deep", "--strict", "--verbose=2", str(app)], check=True)
        archive.unlink(missing_ok=True)
        subprocess.run(["zip", "-r", "-q", str(archive), app.name], cwd=source, check=True)
    with archive.open("rb") as content:
        digest = hashlib.file_digest(content, "sha256").hexdigest()
    write_sidecar(archive, digest)
    if target == "windows/amd64":
        # Byte-identical alias so pre-rename clients can still find an asset.
        legacy = output.resolve() / WINDOWS_LEGACY_ARCHIVE
        shutil.copyfile(archive, legacy)
        write_sidecar(legacy, digest)
        print(legacy)
    print(archive)
    return archive


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", required=True, choices=["windows/amd64", "darwin/arm64"])
    parser.add_argument("--build-root", type=Path, default=Path("build"))
    parser.add_argument("--output", type=Path, default=Path("build/acceptance"))
    args = parser.parse_args()
    package(args.target, args.build_root, args.output)
