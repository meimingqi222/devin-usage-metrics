# Agent Note: Cache immutable manifests and bound download concurrency

Status: implemented

## Problem

Serial head and block downloads accumulated network latency. A sixty-second local manifest TTL defeated reuse across the default hourly sync interval.

## Decision

Head and manifest reads run with at most eight workers. A device's turn and session downloads share an eight-worker pool and only return a package after every decode succeeds. Immutable manifest caches are selected by content hash and corrupted cache entries are fetched again. Matching local manifests have no age limit after the current head hash is checked; the unchecked local import shortcut retains its short TTL.

## Alternatives considered

Unlimited parallelism would increase memory pressure and load. Reusing parsed remote data solely by device identity would hide newer snapshots.

## Consequences

Cold sync overlaps network requests; unchanged manifests and blocks avoid downloads. All cached object bytes remain hash-checked, and one failed block still fails the whole device package.

## Verification

- `src/sync/v3.rs::remote_downloads_are_bounded_parallel_and_fail_as_a_unit`
- `src/sync/v3.rs::immutable_manifest_cache_reuses_verified_bytes_and_refetches_corruption`

Proved: Forcing one download worker failed the concurrency assertion. Disabling manifest cache reuse failed the request-count assertion. Restoring both passes bounded concurrency, warm-cache reuse, corruption recovery and package failure checks.
