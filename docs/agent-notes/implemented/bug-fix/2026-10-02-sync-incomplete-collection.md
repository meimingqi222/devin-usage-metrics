# Agent Note: Reject incomplete usage snapshots before publishing

Status: implemented

## Problem

Local database or file failures can return partial records with collection errors. The sync collector discarded those errors and exported the partial records as an authoritative device snapshot.

## Decision

The collector carries every source error into the combined data. Export rejects incomplete collection before taking the writer lock or writing any objects. The existing sync cycle still attempts remote import after export fails.

## Alternatives considered

Publishing successful sources only would silently remove the failing source from the device manifest. Merging old records without source deletion semantics could preserve stale or double-counted data.

## Consequences

A persistent source error requires attention before the local device can publish again; other devices retain its last complete snapshot.

## Verification

- `src/sync/v3.rs::collection_errors_prevent_any_remote_write`
- `src/main.rs::successful_source_does_not_hide_another_sources_failure`

Proved: Before adding the export guard, the test failed because the error did not contain the collection failure. After the guard, it passes and no remote directory is created. Temporarily discarding source errors in the collector made the mixed-source test fail with zero errors instead of one; restoring propagation passes it.
