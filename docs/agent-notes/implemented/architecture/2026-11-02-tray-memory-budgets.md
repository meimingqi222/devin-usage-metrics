# Agent Note: Tray memory budgets

Status: implemented

## Problem

The tray app's ~500MB resident set was not live data but three cache layers plus a lazy GC.

## What was measured

`data.Load(all, 30d, refresh)` on a real history (11 agents, ~17k turns): `heap_alloc` peaked at 266MB and settled at ~178MB live; `HeapReleased` returned only 47MB to the OS. The settled figure was dominated by the decoded-transcript cache, not by `model.Data` (~40MB live).

## Decision

- `decodedBudget` 128→32 MiB (`internal/data/files.go`). The decoded JSONL cache was the largest resident owner. Reloads re-decode only evicted files on the existing 4-worker prefetcher; measured reload stays ~1.1s on a real history, so the cost is CPU, never I/O or correctness.
- Sync in-memory caches shrunk (`internal/syncclient/cache.go`): manifest 32→8, block 96→24, package 64→16 MiB. Compressed blocks already have the 256 MiB disk cache in `diskcache.go`; a miss only re-runs decode, not a fetch.
- `debug.FreeOSMemory()` after every `App.work` job (`internal/desktop/app.go`). The Go GC otherwise keeps freed pages and the tray's RSS sits at the load high-water mark for the process lifetime. `FreeOSMemory` makes the resident set track the live set.

Expected steady state after these: ~100-150MB resident (data ~40MB + decoded <=32MB + sync LRUs <=48MB + runtime/UI).

### Peak during load (added later)

Measured with `wait4` `ru_maxrss` on `--agent all --refresh`: ~435MB. Halved by:

- `parseAG` streams `steps`/`gen_metadata` rows via `each()` instead of `query()` materializing the whole table (blob(): also accepts `sqlite.BorrowedText` since `each` borrows TEXT columns).
- `LoadAllContext` runs at most 2 agent loaders; each loader is internally parallel already.
- Devin in-flight decode batches capped at `workers` (was `2*workers`).
- `interleavedBudget` 64->32 MiB (`readvfs.go`): the 64 MiB figure came from a 2.4 GB fragmented DB scan; 32 MiB keeps that speedup while halving its share of the load-time high-water mark.

Result: all-agent refresh peaks ~240-290MB, devin alone 297->171MB, load time ~1.6-2.8s on a real history (2-way loader concurrency trades a second of wall time for ~140MB of peak).

### Second pass (0.3.8+1)

`vmmap` on the shipped tray: Untagged (Go heap) ~134MB resident, IOSurface (GPU textures) 33MB, Malloc 15MB — non-Go surface is roughly fixed at ~65MB. The Go share breaks down as model.Data (~40MB), decoded cache (<=32MB), sync LRUs, and GC-spare spans.

- `blockCache` 24->8 MiB and `packageCache` 16->8 MiB: decoded blocks duplicate what `a.remote` already holds; the cache only needs to span one sync cycle.
- `debug.SetGCPercent(50)` in the tray only (host.go): keeps freed spans dense between jobs; CLI is unaffected.
- `decodedBudget` stays 32 MiB — measured reload at 16 MiB thrashed (multi-second re-decode), 32 MiB reuses the whole recent working set.

Ceiling after pass three: the Go share is one model.Data + caches; the remaining floor is macOS UI/GPU (~65MB) plus whatever is needed to keep agent switches instant.

### Third pass: single-copy agent data

`a.loaded` was `map[Agent]Data`; switching agents retained every visited agent's full model.Data (~5-40MB each). Now `a.loaded`/`a.loadedAgent` keep only the selected agent; `a.counts` (session count per agent) and `a.trayEntries` (compact 24B/turn cost+token rows) carry everything the sidebar and tray menu need. `storeLoaded` always records the aggregates but drops the data when the load finished after the user moved on.

Switch cost: snapshot-cache hit ~instant, stale snapshot re-decodes at the decoded-cache hit rate (~200-400ms for the largest agents). The content area shows `ui.Spinner` while `switching()` (selected agent's load in flight). `switchAgent` same-agent clicks still return from the quota/ext screens to history.

### Fourth pass: lazy remote turns

A sync pulled 127k remote turns (~50MB) that sit unused under the default "this device" scope. Now:

- `Cycle` records `Data.RemoteHeads` (device -> head object name, `json:"-"`, never snapshotted).
- `a.remoteTurns` holds remote turns only while the scope is "All devices" or a remote device; switching scope to a remote device runs `syncclient.DecodeRemoteDevices` (disk/object caches, no listing, no publish) and switching back frees the slice plus FreeOSMemory.
- `allData` merges `remoteView()`, so `dataKey` gained a device field.
- `lastGood` no longer clones packages — the remote merge already copies values, so the fallback and `a.remote` can share one copy.

### Fifth pass (measured on a live tray)

`vmmap` + heap profile of the real app settled after load+sync: inuse ~35MB, physical footprint 156MB (was ~220MB at 0.3.8, ~243MB at 0.3.9). What changed:

- `DropRemoteCache` clears `lastGood` after a cycle that runs under the local scope; it repopulates on the next remote-scope decode. It held a full second copy of every remote package (~25MB).
- `decodedBudget` 32->16 MiB: the earlier "thrash" measurement was polluted by the live tray competing for IO; on a quiet system reload stays ~2-3s.
- `trayMemoryLimit` 384->192 MiB, `SetGCPercent` 50->30: inuse is now ~35MB so tighter collection costs little CPU and keeps dirty spans close to the live set. `FreeOSMemory` continues to run after jobs — cumulative released >250MB per launch.

macOS reports: `ps` RSS (~450MB) counts compressed pages; Activity Monitor's "Memory" column matches vmmap's physical footprint (~156MB).

## Alternatives considered

**Cache everything forever.** The ~500MB resident set was the direct result; unbounded decoded, manifest and remote caches dominated it.

**Shrink caches but keep a global GC.** Settled RSS stayed at the load high-water mark because freed spans were retained; `FreeOSMemory` after each job and a tray-only `SetGCPercent` are what make resident memory track the live set.

**Keep every visited agent's data.** A `map[Agent]Data` retained ~5-40MB per agent; the sidebar and tray only need counts and compact turn rows, so the rest of the data is dropped once a load finishes after the user moved on.

## Consequences

Steady resident memory drops from ~500MB to ~100-150MB, and a live tray measured ~156MB physical footprint after load+sync (was ~220MB at 0.3.8, ~243MB at 0.3.9). The cost is bounded CPU on cache misses — re-decoding evicted files at ~200-400ms for the largest agents, re-running block decode on a compressed-disk-cache miss — not I/O or correctness. macOS `ps` RSS (~450MB) counts compressed pages and is misleading; Activity Monitor's "Memory" matches vmmap's physical footprint. The budget values (`decodedBudget` 16 MiB, `trayMemoryLimit` 192 MiB, `SetGCPercent` 30, sync LRUs) and the lazy remote-turns behavior are the contract these passes bought; loosening them re-inflates RSS.
