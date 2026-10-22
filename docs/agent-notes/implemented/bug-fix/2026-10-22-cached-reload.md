# Agent Note: Validate unchanged sources on reload without rebuilding history

Status: implemented

## Problem

The Go Reload button called `data.Load(..., true)`, rebuilding all local history even when sources were unchanged, and immediately started sync alongside the rebuild. The Rust UI refreshes its selected time window and only schedules another sync after five minutes. Optimizing SQLite overflow reads alone did not fix the Go cached-refresh path.

## Decision

- `internal/data/data.go` adds `Reload`: bypass the snapshot TTL, check current source signatures, and reuse unchanged parsed data. `Load(..., true)` remains an unconditional rebuild. Changed sources on manual reload are fully parsed, including historical edits outside the incremental watermark.
- A persisted `FreshSources` bit certifies that a snapshot came from a full source parse, or unchanged certified sources. Background incremental updates of changed sources clear this bit. Manual reload must not reuse an uncertified snapshot: it could otherwise preserve old metrics even after the user explicitly refreshes. Older caches omit the bit and conservatively rebuild once.
- Background Devin collection can seed its incremental watermark from a snapshot whose end is in the past, provided its version and start range remain valid. File importers still require full range coverage before snapshot reuse.
- `internal/desktop/app.go` applies refreshed data before deciding whether to sync. Successful or usable partial sync completion starts the five-minute refresh cooldown. Explicit sync and the existing timer retain their behavior. The Reload button keeps its layout and label.

## Alternatives considered

Increasing decoded JSONL retention from 128 MiB to 384 MiB risks undoing the importer memory fix. Tail-only append validation did not reliably detect in-place history rewrites. File append reuse now hashes the complete old prefix through the same open descriptor used for reading the suffix; incomplete final lines and prior errors force full decoding. Global ordered accounting still reruns, preserving cross-file deduplication. Parallel database scans did not provide a consistent cold-load improvement. Parallel database scans remain excluded. The bounded decoded cache now uses access recency and Claude field projection; see `2026-10-22-go-importer-memory.md`.

## Consequences

With real history and a warm certified snapshot, three consecutive manual reloads took 181–231 ms for Devin (22,156 turns), 17–20 ms for Claude (2,957 turns), and 24–28 ms for Codex. Each reload's sync content hash matched its full parse. These are local collection timings, not network-sync timings or a native Rust/Go speed comparison. Source changes, missing certification, errors and unconditional rebuilds still require parsing. No sync protocol, device identity, native UI layout or database contents change.

## Verification

- `internal/data/files_internal_test.go::TestReloadReusesUnchangedDecodedFiles`
- `internal/data/performance_test.go::TestReloadRebuildsHistoricalDevinEdits`
- `internal/data/performance_test.go::TestDevinStaleWindowKeepsIncrementalBaseline`
- `internal/desktop/app_test.go::TestReloadSyncIntervalMatchesRust`

Proved: disabled unchanged-source reuse, freshness certification, stale-window seeding, full parsing of changed sources and the sync cooldown separately; each focused regression failed, then passed after restoration. Output saved to `docs/agent-notes-evidence/cached-reload-red.txt`.
