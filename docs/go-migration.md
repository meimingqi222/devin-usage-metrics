# Go / MyGo migration status

The default desktop entry point, CI and release workflow now use Go. The new executable does not invoke a Rust subprocess or link a Rust application library. `sync-server` already used Go and its protocol/server implementation is unchanged.

## Implemented surfaces

| Surface | Go owner | Validation |
| --- | --- | --- |
| Ten local importers, five-minute snapshots, device identity | `internal/data` | JSONL and read-only SQLite fixtures for all ten agents |
| Pricing and local calendar aggregation | `internal/pricing`, `internal/analytics` | catalog, tier, missing-price, timezone/DST and device fixtures |
| CLI arguments, inclusive dates, CSV/table output | `internal/cli` | public CLI tests using actual temporary transcripts |
| Native usage/session/quota/compaction/settings views | `internal/desktop` | MyGo renderer and pointer/keyboard tests; Linux native window exercised |
| Quota discovery, OAuth rotation, rate limiting, stale fallback | `internal/quota` | fixture credentials and local HTTP services; read-only Antigravity state fixture |
| v1/v2 import, v3 heads/manifests/blocks | `internal/syncclient` | source-derived protocol fixtures, independent libzstd frames, local HTTP/CAS tests |
| Context replay and field-level apply/undo | `internal/compaction` | replay fixtures, external edits, Rust-shaped undo journal, confirmation conflict |
| Release discovery, checksum, ZIP validation, replacement helpers | `internal/updater` | local HTTP/archive tests; native replacement not exercised |
| Bundling, including SQLite runtime | `mygo.json` | Linux native package produced; Windows/macOS cross-builds |

## Deliberate safety boundary

Keep the old `src/`, Cargo manifests and Rust tests as a behavior reference until real-history differential checks, Rust↔Go live sync, and Windows/macOS installation/update checks are complete. They are not part of the Go build. Do not interpret green unit tests or cross-compilation as proof that every old behavior survived.

## Known gaps and differences

- No complete real-history differential run against Rust. Importer fixtures exercise representative formats, not every historic provider version.
- Sync now has bounded, account/server-scoped in-memory immutable-block and parsed-package caches plus an actual-content no-op publication path. Heads/manifests remain revalidated; fallback recovery bypasses caches. Persistent disk sync caches, periodic integrity audit/automatic repair and parallel transfer remain unimplemented. Healthy cached immutable objects can mask server-side damage until daily expiry/eviction. Live Rust-client interoperability remains unverified.
- Expired disk snapshots now seed Devin watermark/session-filtered incremental reads across restarts; DB/WAL signatures allow unchanged database reuse. Agents, database sources and JSONL decoding use bounded concurrency. Unchanged file sets reuse persisted parsed snapshots; if files change after a restart, accounting still rebuilds rather than reusing per-file parsed records. As in the Rust watermark strategy, edits to historical inference contents require an explicit full refresh.
- CLI human-readable output is English, not bilingual. Unicode terminal column width is approximate. The GUI follows the original Rust layout, dimensions, palette and bilingual presentation, including the tray's cost overview. The user-supplied original screenshots guide visual comparisons; native font/rasterization differences remain, and same-platform macOS pixel-identical parity has not been verified in this Linux orb.
- System language falls back to locale environment variables in development; a platform-native locale integration remains to be validated on Windows/macOS.
- Config edits support strict JSON and ordinary top-level TOML integer settings. Ambiguous TOML constructs are refused without writing rather than normalized. Existing Rust-shaped numeric compaction undo records are accepted.
- macOS Keychain access, Windows Antigravity discovery and updater replacement/restart helpers have not been exercised on those native platforms in this Linux orb. CI configuration now includes native tests but has not been run on GitHub from this worktree.
- Windows packages must retain the SQLite DLL; updates replace both the executable and DLL. Successful replacement keeps `.old` backups and subsequent installation refuses to overwrite them. After verifying an update, remove those application backups before applying another update. macOS updates replace the complete app bundle. Linux uses packaged/manual upgrades.
- Native MyGo is young and is pinned precisely. GPU acceleration depends on the host; the Linux orb's virtual display uses its software renderer.

## Reproduce local validation

```sh
go vet ./...
go test -race ./...
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /tmp/aum.exe .
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /tmp/aum-macos .
go tool mygo build
```

To include the Antigravity state database test without any fixture-time download, set `MYGO_SQLITE_LIBRARY` to the absolute path of the SQLite runtime bundled by `go tool mygo build`. To inspect UI fixtures, set `AUM_SCREENSHOTS` to an absolute directory and run `go test -count=1 ./internal/desktop`. These screenshots use synthetic histories/accounts, never real credentials.

No build-speed ratio against Rust is asserted: the original Rust build requires its private GPUI kit and was not benchmarked in this orb. Measure a cold Go build with a disposable `GOCACHE`, not `go clean -cache` on a cache shared with other work.
