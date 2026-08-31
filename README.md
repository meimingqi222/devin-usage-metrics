# Agent Usage Metrics

**English** | [简体中文](README.zh-CN.md)

A native desktop app that reads local sessions from Devin, Amp, Claude Code, Codex, Antigravity, Grok Build, ZCode, OpenCode, and pi-agent and presents token usage, model distribution, and session details. Switch agents from the left sidebar, in a Chinese or English interface; all data stays on your machine. Multi-device users can sync and aggregate data across machines via iCloud Drive or any shared folder — no central server required.

## Features

- **Usage view**
  - Left sidebar with every supported agent (Devin, Amp, Claude Code, Codex, Antigravity, Grok Build, ZCode, OpenCode, pi-agent) and its session count
  - Chinese and English interface, defaulting to the system language; switch from the top bar and the choice is remembered
  - Aggregate token usage by day (15 days) / week (12 weeks) / month (12 months)
  - Stat cards for Total Tokens, Input (new), Output, Cached read, and Turns/Sessions
  - Stacked bar chart of token trends (input / output / cached layers)
  - Per-period detail table with session count, turns, token breakdown, and per-model usage
- **Sessions view**
  - Lists the 500 most recent sessions, sorted by last activity
  - Shows title, working directory, agent mode, selected model, message count, total tokens
  - Resolves `adaptive` routing to the real backing model
  - Click any session for details: median TTFT, turn count, agent messages, time span
- **Multi-device sync**
  - Sync usage data across machines via iCloud Drive (macOS) or a user-configured shared folder
  - Sync is off by default; toggle it from the top bar ("Sync Off / Sync On"), then it runs fully automatically
  - Once enabled, the app auto-exports local data and imports other devices' data every 15 minutes in the background. Startup and a manual reload also sync immediately.
  - Each device auto-generates a stable device ID. Sync data uses compressed, content-addressed shards with an atomic per-device head, so large histories do not require one ever-growing package.
  - Existing v1 packages are imported automatically during migration. Upgrade every actively syncing client before relying on v2 because older releases cannot read the new format.
  - The sidebar "Devices" section lets you view a single device or aggregate all devices
  - No central server — sync relies entirely on your existing cloud storage, data never touches a third party
- **Data sources**
  - Devin: `~/.local/share/devin/{cli,cli-next}/sessions.db` (platform data directory on Windows)
  - Amp: `~/.local/share/amp/threads/*.json`
  - Claude Code: `~/.claude/projects/**/*.jsonl` (subagent usage is merged into its parent session)
  - Codex: `~/.codex/{sessions,archived_sessions}/**/*.jsonl` or `$CODEX_HOME`
  - Antigravity: `~/.gemini/antigravity/conversations/*.db` (protobuf-encoded SQLite)
  - Grok Build: `~/.grok/sessions/*/summary.json` (with per-session `updates.jsonl` for turn details)
  - ZCode: `~/.zcode/cli/db/db.sqlite`
  - OpenCode: `~/.local/share/opencode/opencode.db` (covers both `opencode` and `opencode2`; legacy `storage/` JSON sessions have been migrated into this database)
  - pi-agent: `~/.pi/agent/sessions/**/*.jsonl`
  - Each agent is loaded lazily when first selected, sources parse in parallel
  - 5-minute on-disk cache in the platform cache directory for fast subsequent launches
  - "Reload" bypasses the cache; unchanged files are detected by mtime and only modified files are re-parsed

## Interface language

The UI is available in Simplified Chinese and English. The language is resolved once at startup:

1. `config.json` in the platform config directory, if it holds a valid `lang`
2. otherwise the system locale (a `zh` prefix selects Chinese, anything else English)

| Platform | Config file |
| --- | --- |
| macOS | `~/Library/Application Support/devin-usage-metrics/config.json` |
| Linux | `~/.config/devin-usage-metrics/config.json` |
| Windows | `%APPDATA%\devin-usage-metrics\config.json` |

Clicking 中 / EN in the top bar writes that file immediately, so the choice survives restarts. The same setting applies to human-readable `--cli` output; CSV column names remain stable English identifiers for scripts.

Known limitation: data-source error messages are produced while loading and stored alongside the cache, so they keep the language of the run that loaded them until the next reload.

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

# CLI: daily Claude Code usage (ccusage-compatible columns)
cargo run -- --cli --agent claude --since 2026-08-20 --until 2026-08-30 --refresh

# Machine-readable CSV
cargo run -- --cli --agent claude --days 30 --format csv

# All agents with per-agent breakdowns
cargo run -- --cli --agent all --by-agent --days 30

# Release build
cargo build --release

# Run tests (integration tests skip automatically when no local session data exists)
cargo test
```

The CLI defaults to the last 30 days of Claude Code usage. It supports `all`, `devin`, `amp`, `claude`, `codex`, `antigravity`, `grok`, `zcode`, `opencode`, and `pi`. Use `--by-agent` to include per-agent rows under the `all` totals. Run `cargo run -- --cli --help` for all options.

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
├── main.rs    # GPUI app entry, UI rendering, usage/sessions/quota views, multi-device sync UI
├── lib.rs     # Module exports
├── data.rs    # Shared records, Devin SQLite reads, on-disk cache, device identity
├── local_sources.rs # Amp, Claude Code, Codex, Antigravity, Grok Build, ZCode, OpenCode, and pi-agent importers
├── sync.rs    # Multi-device sync orchestration, storage transports, and v1 migration
├── sync/v2.rs # Content-addressed, compressed and integrity-checked sync protocol
├── pricing.rs # Model pricing table and cost calculation
├── i18n.rs    # Chinese/English UI strings, language detection and preference file
├── cli.rs     # --cli mode: daily usage rollup, table and CSV output
├── devin-model-pricing.json    # Official Devin model price list (embedded at compile time)
├── models-dev-pricing.json     # Non-Devin model prices extracted from models.dev
└── agg.rs     # Day/week/month bucketing and per-model grouping (with per-device filtering)
tests/
└── dump.rs    # End-to-end integration test against real local data
assets/
└── icon*.{png,icns}  # App icon assets
```

## Data and privacy

- Opens Devin databases read-only and only reads the other agents' JSON/JSONL files
- Cache file is written via atomic rename to avoid partial files
- Makes no network requests; everything shown comes from the local machine and the user's configured shared sync folder
- Multi-device sync never goes through a central server — data packages are written only to your own iCloud Drive or shared folder

## License

Private project; no open-source license assigned yet.
