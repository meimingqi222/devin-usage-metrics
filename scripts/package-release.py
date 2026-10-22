#!/usr/bin/env python3
"""Package MyGo output identically for CI acceptance and stable releases."""

import argparse
import hashlib
from pathlib import Path
import subprocess
import zipfile


def package(target: str, build_root: Path, output: Path) -> Path:
    directory, asset = {
        "windows/amd64": ("windows-amd64", "windows-x64"),
        "darwin/arm64": ("darwin-arm64", "macos-arm64"),
    }[target]
    source = build_root.resolve() / directory
    output.mkdir(parents=True, exist_ok=True)
    archive = output.resolve() / f"devin-usage-metrics-{asset}.zip"
    if target == "windows/amd64":
        # Rust 0.2.11's updater replaces this exact name. SQLite is compiled in.
        executable = source / "Agent Usage Metrics.exe"
        with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as bundle:
            bundle.write(executable, "devin-usage-metrics.exe")
    else:
        # Keep the native bundle layout, executable modes and signing resources.
        app = source / "Agent Usage Metrics.app"
        subprocess.run(["codesign", "--verify", "--deep", "--strict", "--verbose=2", str(app)], check=True)
        archive.unlink(missing_ok=True)
        subprocess.run(["zip", "-r", "-q", str(archive), app.name], cwd=source, check=True)
    with archive.open("rb") as content:
        digest = hashlib.file_digest(content, "sha256").hexdigest()
    archive.with_suffix(".zip.sha256").write_text(f"{digest}  {archive.name}\n", encoding="utf-8")
    print(archive)
    return archive


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", required=True, choices=["windows/amd64", "darwin/arm64"])
    parser.add_argument("--build-root", type=Path, default=Path("build"))
    parser.add_argument("--output", type=Path, default=Path("build/acceptance"))
    args = parser.parse_args()
    package(args.target, args.build_root, args.output)
