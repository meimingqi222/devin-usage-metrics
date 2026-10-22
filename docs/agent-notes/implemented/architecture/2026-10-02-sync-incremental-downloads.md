# Agent Note: Cache immutable manifests and bound download concurrency

Status: implemented

## Problem

Serial head and block downloads accumulated network latency. A sixty-second local manifest TTL defeated reuse across the default hourly sync interval.

## Decision

In the Rust reference, head and manifest reads run with at most eight workers. A device's turn and session downloads share an eight-worker pool and only return a package after every decode succeeds. Immutable manifest caches are selected by content hash and corrupted cache entries are fetched again. Matching local manifests have no age limit after the current head hash is checked; the unchecked local import shortcut retains its short TTL.

The Go client (`internal/syncclient/protocol.go`, `internal/syncclient/publish.go`, `internal/syncclient/cache.go`, `internal/syncclient/diskcache.go`) bounds transfers with eight slots shared by every operation in the process (`parallel.go`), so up to three devices import concurrently without exceeding it, over one keep-alive connection pool. Turn and session references share the download pool; all downloads validate before a device package is assembled, and all uploads complete before the manifest/head CAS.

Go caches only content-addressed data, scoped by server and sync tenant rather than token: verified manifest bytes and assembled packages in memory (LRU), decoded v3 blocks per object in memory (so a republished manifest re-decodes only changed blocks; v2 validates partitions while decoding and is not shared), and compressed blocks on disk (LRU, 256 MiB) so a restart does not download history again. An unchanged head therefore costs no manifest or block request. Every cached block is checked against the referencing hash and size; a damaged disk copy is dropped and fetched again. Each cycle lists only head prefixes; servers that ignore the query still work because the client filters.

## Alternatives considered

Unlimited parallelism would increase memory pressure and load. Reusing parsed remote data solely by device identity would hide newer snapshots.

## Consequences

Cold sync overlaps network requests; unchanged manifests and blocks avoid downloads. All cached object bytes remain hash-checked, and one failed block still fails the whole device package.

## Verification

- `internal/syncclient/parallel_test.go::TestColdSyncDownloadsInParallel`
- `internal/syncclient/parallel_test.go::TestPublishTransfersInParallelAndDoesNotPublishPartialHead`
- `internal/syncclient/efficiency_test.go::TestUnchangedHeadsSkipManifestAndListOnlyHeads`
- `internal/syncclient/efficiency_test.go::TestDiskCacheSurvivesRestartAndRejectsDamage`

- `src/sync/v3.rs::remote_downloads_are_bounded_parallel_and_fail_as_a_unit`
- `src/sync/v3.rs::immutable_manifest_cache_reuses_verified_bytes_and_refetches_corruption`

Proved: Forcing one download worker failed the concurrency assertion. Disabling manifest cache reuse failed the request-count assertion. Restoring both passes bounded concurrency, warm-cache reuse, corruption recovery and package failure checks.

Go proof: Reducing the worker count to one failed both transfer tests at their concurrency/completion assertions; restored four workers and reran with the race detector. Evidence: `docs/agent-notes-evidence/go-sync-serial-red.txt`.

Proved: disabling the disk cache, the manifest byte cache, and the daily audit spacing each failed its bound assertion (restart re-download, manifest GET, full listing); all restored and rerun green. Output saved to `docs/agent-notes-evidence/go-sync-incremental-red.txt`.
