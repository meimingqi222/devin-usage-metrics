#!/usr/bin/env python3
"""Compare Rust/Go CLI reports on a private, frozen copy of local histories.

Run --snapshot once, then rerun comparisons against that same directory.
Only history JSONL, Grok summaries and SQLite databases are copied; login credentials are excluded.
Raw reports/caches stay in the private snapshot. The summary contains no sessions.
"""

import argparse
import csv
from decimal import Decimal
import hashlib
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import sys
import time

AGENTS = ["claude", "codex", "devin", "antigravity", "grok", "zcode", "opencode", "pi", "mimocode", "workbuddy"]
ROOTS = [".claude/projects", ".codex/sessions", ".codex/archived_sessions", ".pi/agent/sessions", ".grok/sessions", ".workbuddy/projects", ".workbuddy-ai/projects", ".codebuddy/projects"]
DBS = [".local/share/devin/cli/sessions.db", ".local/share/devin/cli-next/sessions.db", ".zcode/cli/db/db.sqlite", "Library/Application Support/zcode/cli/db/db.sqlite", "Library/Application Support/opencode/opencode.db", ".local/share/opencode/opencode.db", "Library/Application Support/mimocode/mimocode.db", ".local/share/mimocode/mimocode.db"]


def snapshot(source, root):
    root.mkdir(parents=True, mode=0o700, exist_ok=False)
    home = root / "home"
    home.mkdir()
    paths = [source / p for p in DBS if (source / p).is_file()]
    for directory in ROOTS:
        paths.extend(p for p in (source / directory).rglob("*.jsonl") if p.is_file() and not p.is_symlink())
    paths.extend(p for p in (source / ".grok/sessions").rglob("summary.json") if p.is_file())
    paths.extend(p for p in (source / ".gemini/antigravity/conversations").rglob("*.db") if p.is_file())
    manifest = []
    for original in paths:
        destination = home / original.relative_to(source)
        destination.parent.mkdir(parents=True, exist_ok=True)
        if original.suffix in {".db", ".sqlite"}:
            with sqlite3.connect(original.as_uri() + "?mode=ro", uri=True) as src, sqlite3.connect(destination) as dst:
                src.backup(dst)
        else:
            # A file being appended during collection must not yield a torn copy.
            for attempt in range(3):
                before = original.stat()
                shutil.copy2(original, destination)
                after = original.stat()
                if (before.st_size, before.st_mtime_ns) == (after.st_size, after.st_mtime_ns):
                    break
            else:
                raise RuntimeError("A history kept changing; retry snapshot collection")
        with destination.open("rb") as content:
            digest = hashlib.file_digest(content, "sha256").hexdigest()
        manifest.append({"path": str(destination.relative_to(home)), "bytes": destination.stat().st_size, "sha256": digest})
    config = home / "Library/Application Support/devin-usage-metrics"
    config.mkdir(parents=True, exist_ok=True)
    (config / "device.json").write_text('{"id":"migration-differential","name":"Migration fixture"}')
    (config / "config.json").write_text('{"lang":"en","auto_check_updates":false}')
    (root / "snapshot.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(json.dumps({"snapshot_files": len(manifest), "snapshot_bytes": sum(p["bytes"] for p in manifest)}), flush=True)


def environment(root):
    env = os.environ.copy()
    for key in ["CODEX_HOME", "WORKBUDDY_CONFIG_DIR", "CODEBUDDY_CONFIG_DIR", "WORKBUDDY_DIR", "DEVIN_USAGE_MODELS_PATH", "DEVIN_USAGE_PRO_PRICING_PATH", "XDG_DATA_HOME"]:
        env.pop(key, None)
    env.update(XDG_CONFIG_HOME=str(root / "home/.config"), APPDATA=str(root / "home/AppData/Roaming"), LOCALAPPDATA=str(root / "home/AppData/Local"), HOME=str(root / "home"), USERPROFILE=str(root / "home"), XDG_CACHE_HOME=str(root / "home/Library/Caches"), DEVIN_USAGE_DISABLE_MODELS_FETCH="1", TZ="Asia/Taipei", LANG="en_US.UTF-8")
    return env


def report(path):
    with path.open(newline="") as content:
        reader = csv.DictReader(content)
        if set(reader.fieldnames or []) != {"date", "models", "input", "output", "cache_creation", "cache_read", "total_tokens", "cost_usd"}:
            raise ValueError("Unexpected CLI CSV schema")
        rows = list(reader)
    if not rows:
        raise ValueError("CLI emitted no CSV rows")
    result = {r["date"]: r for r in rows}
    if len(result) != len(rows):
        raise ValueError("Duplicate CLI date rows")
    return result


def differences(left, right):
    result = {"missing_days": len(left.keys() - right.keys()), "extra_days": len(right.keys() - left.keys()), "different_rows": 0, "fields": {}}
    for day in left.keys() & right.keys():
        mismatches = []
        for key in left[day]:
            a, b = left[day][key], right[day].get(key)
            if key in {"date", "models"}:
                same = a == b or (key == "models" and day == "Total")
            else:
                same = b is not None and abs(Decimal(a) - Decimal(b)) <= (Decimal("0.00000002") if key == "cost_usd" else Decimal(0))
            if not same:
                mismatches.append(key)
                result["fields"][key] = result["fields"].get(key, 0) + 1
        result["different_rows"] += bool(mismatches)
    result["pass"] = not any(result[k] for k in ["missing_days", "extra_days", "different_rows"])
    return result


def cache_stats(root, agent, implementation):
    slug = {"claude": "claude-code", "grok": "grok-build", "pi": "pi-agent"}.get(agent, agent)
    if implementation == "rust":
        path = root / f"home/.cache/devin-usage-metrics/cache-{slug}.json"
    else:
        path = root / f"home/Library/Caches/devin-usage-metrics/go-data-{agent}.json"
    value = json.loads(path.read_text())
    data = value.get("Data", value)
    return {key: len(data.get(key) or []) for key in ["sessions", "turns", "errors"]}


def expire(root):
    for directory in [root / "home/.cache/devin-usage-metrics", root / "home/Library/Caches/devin-usage-metrics"]:
        for path in directory.glob("*.json"):
            try:
                value = json.loads(path.read_text())
                for key in ["At", "cached_at"]:
                    if key in value:
                        value[key] = int(time.time()) - 301
                path.write_text(json.dumps(value))
            except (ValueError, OSError):
                pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--snapshot", action="store_true")
    parser.add_argument("--source-home", type=Path, default=Path.home())
    parser.add_argument("--rust", type=Path, required=True)
    parser.add_argument("--go", type=Path, required=True)
    parser.add_argument("--since", default="2020-01-01")
    parser.add_argument("--until", required=True)
    parser.add_argument("--agents", nargs="+", choices=AGENTS, default=AGENTS)
    parser.add_argument("--timeout", type=int, default=1800)
    args = parser.parse_args()
    if sys.platform != "darwin":
        parser.error("This snapshot runner currently targets macOS source paths and cache layouts")
    root = args.root.resolve()
    if args.snapshot:
        snapshot(args.source_home.resolve(), root)
    if not (root / "snapshot.json").is_file():
        parser.error("Create a frozen snapshot first with --snapshot")
    env = environment(root)
    output = root / "reports"
    output.mkdir(exist_ok=True)
    binaries = {}
    for name, binary in [("rust", args.rust), ("go", args.go)]:
        with binary.open("rb") as content:
            binaries[name] = hashlib.file_digest(content, "sha256").hexdigest()
    manifest = json.loads((root / "snapshot.json").read_text())
    summary = {"since": args.since, "until": args.until, "timezone": env["TZ"], "offline_pricing": True,
               "binary_sha256": binaries, "snapshot_files": len(manifest),
               "snapshot_bytes": sum(p["bytes"] for p in manifest),
               "snapshot_manifest_sha256": hashlib.sha256((root / "snapshot.json").read_bytes()).hexdigest(), "results": []}
    for agent in args.agents:
        for phase in ["full", "cache", "expired"]:
            if phase == "expired":
                expire(root)
            row = {"agent": agent, "phase": phase, "runs": {}}
            for name, binary in [("rust", args.rust), ("go", args.go)]:
                command = [str(binary.resolve()), "--cli", "--agent", agent, "--since", args.since, "--until", args.until, "--format", "csv"]
                if phase == "full":
                    command.append("--refresh")
                prefix = output / f"{agent}-{phase}-{name}"
                start = time.monotonic()
                print(f"start {agent} {phase} {name}", flush=True)
                with prefix.with_suffix(".csv").open("w") as out, prefix.with_suffix(".err").open("w") as err:
                    try:
                        run = subprocess.run(command, env=env, stdout=out, stderr=err, timeout=args.timeout)
                        status = run.returncode
                    except subprocess.TimeoutExpired:
                        status = "timeout"
                row["runs"][name] = {"status": status, "seconds": round(time.monotonic() - start, 3), "stderr_bytes": prefix.with_suffix(".err").stat().st_size}
                if status == 0:
                    row["runs"][name]["cache_records"] = cache_stats(root, agent, name)
            if all(r["status"] == 0 for r in row["runs"].values()):
                row["comparison"] = differences(report(output / f"{agent}-{phase}-rust.csv"), report(output / f"{agent}-{phase}-go.csv"))
                stats = [r["cache_records"] for r in row["runs"].values()]
                row["comparison"]["record_counts_match"] = stats[0] == stats[1]
                row["comparison"]["pass"] &= stats[0] == stats[1]
            else:
                row["comparison"] = {"pass": False}
            summary["results"].append(row)
            (output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
            print(json.dumps(row), flush=True)
    return 0 if all(r["comparison"]["pass"] for r in summary["results"]) else 1


if __name__ == "__main__":
    raise SystemExit(main())
