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
| Bundling (SQLite is compiled in; no native library) | `mygo.json` | Linux native package produced; Windows/macOS cross-builds |

## Deliberate safety boundary

Keep the old `src/`, Cargo manifests and Rust tests as a behavior reference until real-history differential checks, Rust↔Go live sync, and Windows/macOS installation/update checks are complete. They are not part of the Go build. Do not interpret green unit tests or cross-compilation as proof that every old behavior survived.

## Known gaps and differences

- No complete real-history differential run against Rust. Importer fixtures exercise representative formats, not every historic provider version.
- Sync caches only content-addressed data, scoped by server and account: verified manifests, assembled packages and decoded v3 blocks in memory, and compressed blocks on disk (256 MiB LRU), so unchanged heads cost no manifest or block request and a restart does not re-download history. Each cycle lists only heads; up to three devices import in parallel under eight shared transfer slots. A remote device that fails keeps its last good package for the process and no longer blocks publishing this device (`ErrPartial`). Uploads skip objects the parent manifest holds and send others as one conditional PUT whose 412 ETag proves an existing copy. A daily full listing drops missing objects from reuse so they are re-uploaded, and each cycle checks this device's stored manifest by HEAD and restores it from verified bytes. The server rejects content-addressed objects whose bytes do not match their name. Block contents are not re-verified on the server after upload, and live Rust-client interoperability remains unverified. Running the Rust and Go clients on one device at the same time makes each re-encode all history every cycle: their block content hashes differ.
- Expired disk snapshots now seed Devin watermark/session-filtered incremental reads across restarts; DB/WAL signatures allow unchanged database reuse. Agents, database sources and JSONL decoding use bounded concurrency. Unchanged file sets reuse persisted parsed snapshots; if files change after a restart, accounting still rebuilds rather than reusing per-file parsed records. As in the Rust watermark strategy, edits to historical inference contents require an explicit full refresh.
- CLI human-readable output is English, not bilingual. Unicode terminal column width is approximate. The GUI follows the original Rust layout, dimensions, palette and bilingual presentation, including the tray's cost overview. The user-supplied original screenshots guide visual comparisons; native font/rasterization differences remain, and same-platform macOS pixel-identical parity has not been verified in this Linux orb.
- System language falls back to locale environment variables in development; a platform-native locale integration remains to be validated on Windows/macOS.
- Config edits support strict JSON and ordinary top-level TOML integer settings. Ambiguous TOML constructs are refused without writing rather than normalized. Existing Rust-shaped numeric compaction undo records are accepted.
- Compaction replay adds a context-safety policy: every additional replayed compaction is charged 1% of the baseline simulated cost as unmodelled context/behaviour drift, additions are capped at `max(10, baseline/2)`, and a recommendation must clear its savings bar on the net figure. This intentionally differs from the Rust cost-only selection when a few dollars of savings would substantially increase compaction frequency; apply/undo semantics are unchanged. Without a configured threshold, the baseline is the threshold whose replay reproduces the observed compaction count, not Rust's median compaction point (manual compactions pull that median low). When replay at the baseline differs from the observed count by more than 1.5× (and at least 3), the verdict is withheld as low confidence.
- macOS Keychain access, Windows Antigravity discovery and updater replacement/restart helpers have not been exercised on those native platforms in this Linux orb. CI configuration now includes native tests but has not been run on GitHub from this worktree.
- SQLite is the cgo-free `github.com/ncruces/go-sqlite3` behind `internal/sqlite`, so packages carry no native library. Values are read by storage class like the former MyGo plugin; a real-history differential run (Devin, OpenCode, ZCode, MimoCode, Antigravity and its state database) produced byte-identical results. The Windows ZIP holds only `devin-usage-metrics.exe`, the name Rust's updater replaces, so Rust installs can update to Go; packaged apps delete a leftover `mygo-sqlite3.dll` at startup. Successful replacement keeps `.old` backups and installation refuses to overwrite them; as in Rust, a packaged app removes them at its next startup. Like Rust, packaged installs check for updates 10 seconds after launch and every four hours (`auto_check_updates` in `config.json`), download an available update at once, and honour `skipped_update_version`. macOS updates replace the complete app bundle. Linux uses packaged/manual upgrades.
- Native MyGo is young and is pinned precisely. GPU acceleration depends on the host; the Linux orb's virtual display uses its software renderer.

## Sync diagnostics

The Go desktop writes JSON Lines to `sync.log` inside the platform cache directory's `devin-usage-metrics` folder (`~/Library/Caches/devin-usage-metrics/sync.log` on macOS; `XDG_CACHE_HOME` overrides the cache root). This is separate from the Rust reference's `~/.cache/devin-usage-metrics/load.log`. Restart the rebuilt Go executable to enable the new logging.

Each desktop sync has a `run` ID shared by local collection, cycle, import and publication. `stage_start`/`stage_end` record elapsed milliseconds and safe error categories. Downloads/uploads emit `transfer_progress` every five seconds, including completed/total counts; summaries record downloaded bytes, cache hits, uploaded objects and verified existing objects. Candidate failures, previous-manifest fallback, slow/failed HTTP requests and retry delays are logged. `head_cas_committed` confirms remote publication; `desktop_sync_applied` confirms whether the UI accepted the remote snapshot. A local collection summary includes source-error count; collection precedes the network deadline.

Logs omit credentials, server URLs, response/request bodies and session content. Device/object identifiers are hashed; unrecognized errors are logged by type rather than raw text. The file rotates at 4 MiB to `sync.log.1`, retaining one backup, and logging failures never abort sync. New log files use mode 0600.

```sh
tail -f "$HOME/Library/Caches/devin-usage-metrics/sync.log"
```

## Reproduce local validation

```sh
go vet ./...
go test -race ./...
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /tmp/aum.exe .
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /tmp/aum-macos .
go tool mygo build
```

SQLite fixtures, including the Antigravity state database, need no native library or download. To inspect UI fixtures, set `AUM_SCREENSHOTS` to an absolute directory and run `go test -count=1 ./internal/desktop`. These screenshots use synthetic histories/accounts, never real credentials.

No build-speed ratio against Rust is asserted: the original Rust build requires its private GPUI kit and was not benchmarked in this orb. Measure a cold Go build with a disposable `GOCACHE`, not `go clean -cache` on a cache shared with other work.
