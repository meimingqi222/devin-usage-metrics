# Agent Usage Metrics

**English** | [简体中文](README.zh-CN.md)

A native desktop app that reads local sessions from Devin, Amp, Claude Code, Codex, and Antigravity and presents token usage, model distribution, and session details. Switch agents from the top bar; all data stays on your machine.

## Features

- **Usage view**
  - Switch between Devin, Amp, Claude Code, Codex, and Antigravity
  - Aggregate token usage by day (14 days) / week (12 weeks) / month (6 months)
  - Stat cards for Total Tokens, Input (new), Output, Cached read, and Turns/Sessions
  - Stacked bar chart of token trends (input / output / cached layers)
  - Per-period detail table with session count, turns, token breakdown, and per-model usage
- **Sessions view**
  - Lists the 500 most recent sessions, sorted by last activity
  - Shows title, working directory, agent mode, selected model, message count, total tokens
  - Resolves `adaptive` routing to the real backing model
  - Click any session for details: median TTFT, turn count, agent messages, time span
- **Data sources**
  - Devin: `~/.local/share/devin/{cli,cli-next}/sessions.db` (platform data directory on Windows)
  - Amp: `~/.local/share/amp/threads/*.json`
  - Claude Code: `~/.claude/projects/**/*.jsonl` (subagent usage is merged into its parent session)
  - Codex: `~/.codex/{sessions,archived_sessions}/**/*.jsonl` or `$CODEX_HOME`
  - Antigravity: `~/.gemini/antigravity/conversations/*.db` (protobuf-encoded SQLite)
  - Loads multiple sources in parallel and merges the results
  - 5-minute on-disk cache in the platform cache directory for fast subsequent launches
  - "Reload" bypasses the cache and re-reads every local source

## Tech stack

| Component | Description |
| --- | --- |
| [GPUI](https://github.com/zed-industries/zed) | Zed's high-performance Rust UI framework for native windows and controls |
| [rusqlite](https://crates.io/crates/rusqlite) | SQLite compiled with the `bundled` feature; opens session DBs read-only |
| [serde](https://crates.io/crates/serde) / [serde_json](https://crates.io/crates/serde_json) | Parses session metadata and serializes the cache |
| [chrono](https://crates.io/crates/chrono) | Date/period aggregation and timezone handling |

## Prerequisites

- Windows or macOS (the current implementation has been verified on Windows)
- Rust toolchain (stable via `rustup` recommended)
- Prior use of at least one supported coding agent

## Build and run

```bash
# Debug run
cargo run

# Release build
cargo build --release

# Run tests (the dump integration test requires local session data)
cargo test
```

## Bundle as .app

`Cargo.toml` already contains a `[package.metadata.bundle]` section. Use [`cargo-bundle`](https://crates.io/crates/cargo-bundle) to package:

```bash
cargo install cargo-bundle
cargo bundle --release
```

The resulting `.app` appears under `target/release/bundle/osx/` and uses `assets/icon.icns` as its icon.

## Project layout

```
src/
├── main.rs    # GPUI app entry, UI rendering, usage/sessions views
├── lib.rs     # Module exports
├── data.rs    # Shared records, Devin SQLite reads, on-disk cache
├── local_sources.rs # Amp, Claude Code, Codex, and Antigravity importers
└── agg.rs     # Day/week/month bucketing and per-model grouping
tests/
└── dump.rs    # End-to-end integration test against real local data
assets/
└── icon*.{png,icns}  # App icon assets
```

## Data and privacy

- Opens Devin databases read-only and only reads the other agents' JSON/JSONL files
- Cache file is written via atomic rename to avoid partial files
- Makes no network requests; everything shown comes from the local machine

## License

Private project; no open-source license assigned yet.
