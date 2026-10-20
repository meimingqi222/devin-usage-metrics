# Agent Usage Metrics

**English** | [简体中文](README.zh-CN.md)

Local coding-agent analytics, now implemented in **Go + [MyGo native UI](https://github.com/egoist/mygo)**. No Rust compilation, WebView, JavaScript runtime, or cgo is required to build the desktop application.

> The Go implementation is a migration candidate, not a claim of proven Rust feature parity. The previous Rust sources remain as a behavior reference and do not participate in the Go build or release workflow. See [migration status and limitations](docs/go-migration.md).

## Features

- Ten local history importers: Devin, Claude Code, Codex, Antigravity, Grok Build, ZCode, OpenCode, pi-agent, MimoCode, WorkBuddy / CodeBuddy.
- Daily (15), weekly (12), and monthly (12) calendar buckets, stacked token charts, per-model distributions, recorded/API-equivalent costs, and device filtering.
- Latest 500 sessions, search, token totals, model resolution, timestamps, median TTFT and resume commands.
- Live quotas for Claude, Codex, Grok, Devin and Antigravity, including cached/stale results and rate-limit cooldowns. Keychain discovery requires opt-in.
- Claude/Codex historical compaction analysis, explicit local apply, external-edit protection and durable undo. Estimates do not predict task quality or subscription billing.
- Opt-in, GitHub-authenticated self-hosted synchronization, v1/v2 imports and content-addressed v3 publication.
- Native desktop window and tray quota panel; English/Chinese UI with persisted preference. CLI human-readable output currently remains English; CSV columns are stable English identifiers.

## Requirements

- Go **1.27.1 or newer**. MyGo is pinned to an exact pseudo-version in `go.mod`; the CLI tool uses the same version.
- Windows 10/11, macOS 12+, or Linux with GTK 3. Linux tray support additionally needs `libayatana-appindicator3` and a desktop with an indicator host. Linux uses a tray menu rather than direct tray-icon clicks.
- At least one supported agent's local history for real usage data.

MyGo's SQLite plugin loads a precompiled, platform-specific library via purego. `go tool mygo build` bundles it automatically and checks downloads against upstream SHA-256 metadata. Development runs may download it once into MyGo's cache. **Do not distribute only the executable**, especially on Windows: include `mygo-sqlite3.dll`.

## Run, test and package

```sh
go run .
go run . --cli --agent claude --days 30
go run . --cli --agent all --by-agent --since 2026-08-20 --until 2026-08-30 --format csv
go run . --cli --help

gofmt -l main.go internal
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build ./...

# Native packaging, including SQLite resources; no Bun/npm needed
go tool mygo build
go tool mygo build -platform darwin/arm64,windows/amd64
```

Windows PowerShell: set `$env:CGO_ENABLED = '0'` before `go build ./...` instead of using shell-style environment assignment. Race tests need the Go race toolchain/C compiler; release builds do not.

Packaged apps appear in `build/<os>-<arch>/`. macOS signing/DMG creation must run on macOS. Windows installers require NSIS (MyGo can obtain it); the release workflow distributes a portable ZIP with its runtime DLL. Updater installation/restart still requires native-platform validation before release.

The sync server is a separate, existing Go module:

```sh
(cd sync-server && go test -race ./... && go vet ./...)
```

## Data sources

| Agent | Default history location |
| --- | --- |
| Devin | `~/.local/share/devin/{cli,cli-next}/sessions.db` (platform data directory on Windows) |
| Claude Code | `~/.claude/projects/**/*.jsonl` |
| Codex | `$CODEX_HOME/{sessions,archived_sessions}/**/*.jsonl`, default `~/.codex` |
| Antigravity | `~/.gemini/antigravity/conversations/*.db` |
| Grok Build | `~/.grok/sessions/*/{summary.json,updates.jsonl}` |
| ZCode | `~/.zcode/cli/db/db.sqlite` |
| OpenCode | platform data directory `opencode/opencode.db`, including `opencode2` |
| pi-agent | `~/.pi/agent/sessions/**/*.jsonl` |
| MimoCode | platform data directory `mimocode/mimocode.db` |
| WorkBuddy | `~/.{workbuddy,workbuddy-ai,codebuddy}/projects/**/*.jsonl`; `WORKBUDDY_CONFIG_DIR` / `CODEBUDDY_CONFIG_DIR` supported |

The Go disk cache has a five-minute TTL and is separate from Rust snapshots. Reload bypasses snapshots. Transcript/database signatures support in-process reuse. Reads never modify session databases.

## Configuration and privacy

Configuration stays in `devin-usage-metrics/` under the OS configuration directory: `~/.config` on Linux, `~/Library/Application Support` on macOS, `%APPDATA%` on Windows. Existing device identity, language preference, sync configuration, encrypted login and compaction undo records are reused.

- Local history is sent only to the configured self-hosted API while sync is enabled. GitHub is used for identity.
- Quota queries contact provider APIs and may rotate/write agent OAuth credentials. Keychain access is opt-in.
- Pricing catalogs refresh from `models.opencode.ai` and Devin documentation (24h TTL, 1h failure backoff). Set `DEVIN_USAGE_DISABLE_MODELS_FETCH=1` to disable catalog fetching; `DEVIN_USAGE_MODELS_URL` / `DEVIN_USAGE_MODELS_PATH` override its source/cache.
- Update checks/downloads contact GitHub only on user action. ZIP checksums detect corruption, not publisher authenticity.
- Only explicit compaction Apply/Undo changes Claude/Codex settings; unrelated fields are preserved, external edits refused, undo stays local. Start a new agent session afterwards.

## Layout

`main.go` selects native GUI or CLI. `internal/desktop/host.go` owns windows/tray/lifecycle; `app.go` owns state and event delivery. Other `internal/` packages own `data`, `model`, `pricing`, `analytics`, `cli`, `quota`, `syncclient`, `compaction`, and `updater`. `mygo.json` configures packaging. `sync-server/` remains independent. `src/`, Cargo manifests, and Rust tests are retained only for migration comparison.

Private project; no open-source license assigned.
