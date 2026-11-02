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
load time unchanged (~1.6s on a real history).
