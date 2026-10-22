# Agent Note: Require a complete device snapshot before suppressing legacy sync

Status: implemented

## Problem

The Go migration chose a single filename per device as soon as a v3 head appeared. A corrupt or incomplete v3 snapshot therefore hid a valid v2/v1 snapshot and failed the desktop cycle. The desktop intentionally keeps its last complete remote snapshot on failure, so this can prevent newly imported devices from appearing.

## Decision

`internal/syncclient/protocol.go` retains ordered v3/v2/v1 candidates per device. Only successful identity/schema/block validation suppresses lower protocols. Context cancellation stops fallback. A complete v3 snapshot replaces rather than merges older formats, avoiding duplicates.

When every candidate of a remote device fails, that device contributes its last good package from this process (if any), the other devices' data is still returned, and the cycle reports `ErrPartial`; the desktop applies a partial result and shows the error. As in Rust, where export and import are independent, a remote failure does not block publishing this device: publication uses only local data and this device's own history, which it re-reads under its CAS base. A failure to read this device's own legacy (v1/v2) history still aborts before publishing, since publishing without it would drop that history. This does not authorize overwriting a corrupt local v3 head; publishing retains its CAS validation.

## Alternatives considered

Choosing by head presence is cheaper but confuses discovery with a successful migration. Combining all formats duplicates data and can restore stale records. Failing the whole cycle on any remote device (the first Go version) let one broken device stop every other device's publication and hid all healthy devices from the desktop; the published snapshot never depends on other devices, so blocking it protected nothing.

## Consequences

Mixed-protocol imports can recover a valid older snapshot while a newer format is incomplete. The recovered data may lag that device's latest v3 update. A device that fails before any success in the process is absent until it loads. Snapshot repair and caching are owned by the incremental-download and audit notes.

## Verification

- `internal/syncclient/parallel_test.go::TestCycleFallsBackOnlyAfterInvalidV3`
- `internal/syncclient/efficiency_test.go::TestRemoteFailureNeitherBlocksPublishNorDropsOtherDevices`

Proved: Disabling the candidate fallback failed with zero sessions and invalid head; restored the guard and reran successfully. Output saved to `docs/agent-notes-evidence/go-sync-fallback-red.txt`.

Proved: restoring the early return on remote failure failed the partial-failure test at its `ErrPartial` assertion; restored and reran green. Output saved to `docs/agent-notes-evidence/go-sync-partial-red.txt`.
