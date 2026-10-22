# Agent Note: Coalesce SQLite overflow-page reads behind a read-only VFS

Status: implemented

## Problem

Reloading the dashboard with real history took about 5.4–8.4 s in local collection, and a Devin full refresh alone spent 4–6 s on the two session databases (933 MiB and 600 MiB). Profiling a refresh counted 1.21 M `ReadAt` calls on the main database file: SQLite's direct overflow-read path (large `chat_message` bodies) bypasses its page cache and issues one read per 4 KiB page, so the pure-Go VFS turned one scan into hundreds of thousands of syscalls. `PRAGMA mmap_size` was already tried and showed no measurable gain, because that read path bypasses the mapping in this build.

## Decision

- Read-only opens go through a VFS registered as `aum-read` that wraps the default `os` VFS (`internal/sqlite/readvfs.go`); only the main database file of a read-only connection is wrapped, and WAL/journal files keep the original VFS.
- By default the wrapper keeps a 16 × 256 KiB direct-mapped block cache. Devin import connections select `OpenOptions.InterleavedReads`: 128 × 64 KiB (8 MiB), reducing over-read for alternating index and payload pages. Other agents and quota connections keep the default. A read inside one block is served from the buffer, so 64 adjacent overflow pages cost one read instead of 64. Reads larger than a block or spanning two blocks pass through.
- The cache is enabled only for the duration of one statement in `DB.run`, and is reset when the statement ends and on every `Lock`/`Unlock`. That lifetime is the safety argument: a rollback-journal reader holds `LOCK_SHARED` for the whole statement, so main-file pages cannot change; a WAL reader holds no database-file lock, and relies on SQLite's read mark keeping a checkpoint from backfilling frames the snapshot still needs. The per-statement reset means no page is reused across snapshots.
- `DB.Each` returns the callback error immediately and aborts the statement instead of stepping the remaining rows.
- Read-only paths are converted to `file:` URIs because the VFS name is selected by a URI parameter. The URI shape matters on Windows: a drive letter must be `file:C:/x`, not `file:///C:/x` (SQLite would hand the VFS `/C:/x`, which the pure-Go VFS does not normalize), and a UNC path keeps both slashes after the empty authority (`file:////server/share/x`). `readOnlyURI` owns this and is table-tested.

## Alternatives considered

- `PRAGMA mmap_size`: measured no gain; the overflow read path bypasses the mapping.
- Mapping the whole file: it retained file-resident pages with no gain over the block cache.
- Full-body typed JSON decoding still visits discarded payloads. The importer now validates the whole document and selectively materializes consumed fields; differential fuzzing and all-agent snapshot comparisons guard semantics.
- Forcing a table scan (`NOT INDEXED`): inconsistent results, reverted.

- Large JSON TEXT columns use `EachBorrowedText`: read ordinary columns first, fetch SQLite-owned text last, and parse within the callback. Retained JSON values own their storage; BLOB, NULL and numeric columns preserve their prior types. No statement operation may intervene between borrowing and consuming the bytes.

## Consequences

- On the real Devin history (full refresh, warm OS page cache, interleaved runs against the previous build): median 6.2 s → 5.3 s (about 12–14%), and 1.21 M `ReadAt` calls become 0.23 M file reads (81% served from the cache). Parsed results are byte-identical: a SHA-256 over every session and turn for a fixed window matches the pre-change build.
- Default read-only connections retain at most 4 MiB of buffers; Devin imports retain at most 8 MiB per connection. Buffers allocate lazily and statement resets keep capacity. The two measured Devin databases therefore add at most 8 MiB over the previous configuration.
- A scattered read can read 256 KiB for one 4 KiB page; the sequential overflow scan is the case this targets.
- Windows read-only opens depend on the URI shape; `TestReadOnlyURIShapes` pins it, and the windows-latest CI job exercises real opens.

## Verification

- `internal/sqlite/readvfs_test.go::TestReadAheadReducesSQLiteOverflowReads`
- `internal/sqlite/readvfs_test.go::TestReadAheadCoalescesPagesAndInvalidates`
- `internal/sqlite/readvfs_test.go::TestReadAheadObservesWALAndCheckpointChanges`
- `internal/sqlite/readvfs_test.go::TestReadAheadObservesRollbackJournalWrites`
- `internal/sqlite/readvfs_test.go::TestReadOnlyURIShapes`
- `internal/sqlite/readvfs_test.go::TestEachAbortLeavesConnectionUsable`

Proved: disabling read-ahead made the 2 MiB overflow-read regression fail with 515 file reads; restoring it passed. Output saved to `docs/agent-notes-evidence/sqlite-read-ahead-red.txt`.

Later frozen-fixture trials selected 256 KiB × 16: 16 KiB × 64 increased Devin cold load to 5.67 s, while 64 KiB × 64 and 256 KiB × 16 were about 2.43 s and 2.38 s before borrowed-text decoding. A later 1 MiB × 4 trial regressed Devin (3.06 s versus 2.66 s); it was reverted. These are warm-OS-cache loader timings, not native UI startup times. Borrowed-text storage classes, callback abort and connection reuse are covered by `internal/sqlite/sqlite_test.go::TestEachBorrowedTextPreservesValuesAndStorageClasses`.

Devin-specific refinement: smaller 64 KiB blocks with 128 slots reduce message-query reads from 14.03 GB to 3.51 GB on the frozen fixture. These are cumulative file API bytes, not physical disk bytes. Larger SQLite page caches did not materially reduce requests and were rejected. The statement/lock invalidation, WAL/checkpoint, rollback, boundary and overflow tests run against both profiles. `internal/sqlite/readvfs_test.go::TestInterleavedReadAheadBoundsScatteredReads` bounds amplification and allocation; restoring the default profile failed its scattered-read assertion, then restoring the new profile passed. Evidence: `docs/agent-notes-evidence/devin-read-ahead-red.txt`.
