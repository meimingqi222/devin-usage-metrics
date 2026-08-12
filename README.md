# Devin Usage Metrics

**English** | [简体中文](README.zh-CN.md)

A native macOS desktop app that reads the local session databases left by the Devin CLI and presents a dashboard of token usage, model distribution, and session details. All data stays on your machine — nothing is uploaded.

## Features

- **Usage view**
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
  - Opens `~/.local/share/devin/cli/sessions.db` and `~/.local/share/devin/cli-next/sessions.db` read-only
  - Loads multiple sources in parallel and merges the results
  - 5-minute on-disk cache (`~/.cache/devin-usage-metrics/cache.json`) for fast subsequent launches
  - "Reload" button in the top bar bypasses the cache and re-reads the databases

## Tech stack

| Component | Description |
| --- | --- |
| [GPUI](https://github.com/zed-industries/zed) | Zed's high-performance Rust UI framework for native windows and controls |
| [rusqlite](https://crates.io/crates/rusqlite) | SQLite compiled with the `bundled` feature; opens session DBs read-only |
| [serde](https://crates.io/crates/serde) / [serde_json](https://crates.io/crates/serde_json) | Parses session metadata and serializes the cache |
| [chrono](https://crates.io/crates/chrono) | Date/period aggregation and timezone handling |

## Prerequisites

- macOS (this project is currently configured and verified for macOS)
- Rust toolchain (stable via `rustup` recommended)
- Prior use of the Devin CLI so that `~/.local/share/devin/cli/sessions.db` exists

## Build and run

```bash
# Debug run
cargo run

# Release build
cargo build --release

# Run tests (requires local Devin session data)
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
├── data.rs    # SQLite reads, metadata parsing, on-disk cache
└── agg.rs     # Day/week/month bucketing and per-model grouping
tests/
└── dump.rs    # End-to-end integration test against real local data
assets/
└── icon*.{png,icns}  # App icon assets
```

## Data and privacy

- Opens Devin CLI's local databases with `SQLITE_OPEN_READ_ONLY` only — never writes to or modifies Devin's data
- Cache file is written via atomic rename to avoid partial files
- Makes no network requests; everything shown comes from the local machine

## License

Private project; no open-source license assigned yet.
