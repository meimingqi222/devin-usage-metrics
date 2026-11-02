# Tray memory budgets

The tray app's ~500MB resident set was not live data but three cache
layers plus a lazy GC.

## What was measured

`data.Load(all, 30d, refresh)` on a real history (11 agents,
~17k turns): `heap_alloc` peaked at 266MB and settled at ~178MB live;
`HeapReleased` returned only 47MB to the OS. The settled figure was
dominated by the decoded-transcript cache, not by `model.Data`
(~40MB live).

## Decisions

- `decodedBudget` 128→32 MiB (`internal/data/files.go`). The decoded
  JSONL cache was the largest resident owner. Reloads re-decode only
  evicted files on the existing 4-worker prefetcher; measured reload
  stays ~1.1s on a real history, so the cost is CPU, never I/O or
  correctness.
- Sync in-memory caches shrunk (`internal/syncclient/cache.go`):
  manifest 32→8, block 96→24, package 64→16 MiB. Compressed blocks
  already have the 256 MiB disk cache in `diskcache.go`; a miss only
  re-runs decode, not a fetch.
- `debug.FreeOSMemory()` after every `App.work` job
  (`internal/desktop/app.go`). The Go GC otherwise keeps freed pages
  and the tray's RSS sits at the load high-water mark for the process
  lifetime. `FreeOSMemory` makes the resident set track the live set.
Expected steady state after these: ~100-150MB resident (data ~40MB +
decoded <=32MB + sync LRUs <=48MB + runtime/UI).

## Peak during load (added later)

Measured with `wait4` `ru_maxrss` on `--agent all --refresh`: ~435MB.
Halved by:

- `parseAG` streams `steps`/`gen_metadata` rows via `each()` instead of
  `query()` materializing the whole table (blob(): also accepts
  `sqlite.BorrowedText` since `each` borrows TEXT columns).
- `LoadAllContext` runs at most 2 agent loaders; each loader is
  internally parallel already.
- Devin in-flight decode batches capped at `workers` (was `2*workers`).
- `interleavedBudget` 64->32 MiB (`readvfs.go`): the 64 MiB figure came
  from a 2.4 GB fragmented DB scan; 32 MiB keeps that speedup while
  halving its share of the load-time high-water mark.

Result: all-agent refresh peaks ~240-290MB, devin alone 297->171MB,
load time ~1.6-2.8s on a real history (2-way loader concurrency trades
a second of wall time for ~140MB of peak).

## Second pass (0.3.8+1)

`vmmap` on the shipped tray: Untagged (Go heap) ~134MB resident, IOSurface
(GPU textures) 33MB, Malloc 15MB — non-Go surface is roughly fixed at
~65MB. The Go share breaks down as model.Data (~40MB), decoded cache
(<=32MB), sync LRUs, and GC-spare spans.

- `blockCache` 24->8 MiB and `packageCache` 16->8 MiB: decoded blocks
  duplicate what `a.remote` already holds; the cache only needs to span
  one sync cycle.
- `debug.SetGCPercent(50)` in the tray only (host.go): keeps freed spans
  dense between jobs; CLI is unaffected.
- `decodedBudget` stays 32 MiB — measured reload at 16 MiB thrashed
  (multi-second re-decode), 32 MiB reuses the whole recent working set.

Ceiling: steady state is bound near ~120-160MB resident; macOS UI/GPU
plus model.Data cannot go lower without a disk-resident decoded cache
or dropping per-agent `a.loaded` retention.
