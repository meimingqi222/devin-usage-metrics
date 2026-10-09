# Agent Note: Keep cached quota accounting under tenant locks

Status: implemented

## Problem

Every object write rescanned the tenant directory under one global lock, causing cumulative scanning costs and blocking unrelated accounts.

## Decision

Each tenant has its own lock and lazily initialized usage totals. Successful writes update totals; migration markers and every GC attempt invalidate them. Quota checks, writes and GC serialize within one tenant. Temporary files always close even after write or sync failures.

## Alternatives considered

Removing locking would race quota and CAS operations. A database adds deployment complexity that is unnecessary for this single-process object service.

## Consequences

Normal writes avoid repeated directory scans and unrelated tenants proceed concurrently. The storage directory is owned by one service process; external changes require a restart to reset accounting.

## Verification

- `sync-server/main_test.go::TestConcurrentWritesRespectCachedQuota`
- `sync-server/main_test.go::TestCachedUsageTracksOverwriteAndGC`
- `sync-server/main_test.go::TestBusyTenantDoesNotBlockOtherTenant`

Proved: Temporarily sharing one lock across tenants caused the independent write test to time out. Disabling GC invalidation failed the accounting test. Restoring both passes these tests and concurrent hard-quota enforcement under the race detector.
