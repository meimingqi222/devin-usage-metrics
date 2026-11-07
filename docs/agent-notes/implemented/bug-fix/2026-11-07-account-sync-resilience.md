# Agent Note: One bad record no longer stops account sync, and deletion records are bounded

Status: implemented

## Problem

Account sync and export failed as a whole on any one of these, with a
generic message and no hint which account caused it:

- a saved record whose credential file was missing or unreadable
  (`snapshotLocked` and `ApplySync` returned on the first read error,
  although `decorateLocked` and the fold logic already treat a missing copy
  as a normal state);
- a name longer than the sync format's 512 bytes (`Rename` took any
  length; `validateBackup` rejected the snapshot);
- more than 4096 deletion records: tombstones were never dropped, merged
  across devices only grew, and past the cap every snapshot failed
  validation on every device.

## Decision

- `snapshotLocked` skips a record whose copy is missing or unreadable, or
  whose name is past the limit, and returns it as a `SkippedAccount`
  (`missing_copy`, `unreadable`, `invalid_name`). `Store.Unsyncable` lists
  them; `SyncResult.Skipped` carries them out of `ApplySync`.
- `ApplySync` keeps such a record untouched. When the merged snapshot holds
  the same account and it is not in use, the copy is restored under the
  existing record; the key is always removed from the new-entry set so no
  second record is filed under the same ID.
- `Rename` caps names at `MaxAccountNameRunes` (128).
- `pruneTombstones` drops deletion records older than `TombstoneRetention`
  (180 days) and keeps the newest `MaxTombstones` (4096). It runs in
  `mergeBackups`, `snapshotLocked` and `tombstoneLocked`.
- The desktop names each skipped account in the manager with what to do,
  adds "N account(s) not synced" to the sync status in amber (not as a
  failure), and lists accounts left out of an export.

## Alternatives considered

**Stamp a surviving record past the tombstone it outlived.** Proposed to
keep an in-use account alive on other devices. Rejected: a record kept
because it was in use when another device deleted it is a deferred
deletion, and `TestAccountSyncProtectsTheLiveSignIn` pins that the deletion
completes once this device switches away. `unsuppressSurvivorsLocked`
carries a comment saying so.

**Fail the sync but name the record.** Every other account would stay
unsynced until the user repaired one record.

**Keep tombstones forever and raise the cap.** Only moves the cliff.

## Consequences

A record left out is reported, never treated as deleted, so it does not
propagate a removal. A device offline for more than 180 days can bring back
an account deleted meanwhile. Snapshots from older builds with more than
4096 deletion records still fail validation and are counted as unreadable
until those devices update.

## Verification

- `internal/accounts/sync_resilience_test.go::TestSyncSkipsRecordWithMissingCopy`
- `internal/accounts/sync_resilience_test.go::TestSyncRestoresMissingCopyFromAnotherDevice`
- `internal/accounts/sync_resilience_test.go::TestLongRenameStaysSyncable`
- `internal/accounts/sync_resilience_test.go::TestOldTombstonesArePruned`
- `internal/desktop/account_ux_test.go::TestSkippedAccountIsNamedWhereItIsManaged`

Proved: the accounts tests were run against the pre-fix code with only the
new names declared (stubbed `Unsyncable`, unused constants) and failed at
their bound assertions: snapshot failed on a missing copy, snapshot
rejected a long rename, a deletion past retention survived; output saved to
`docs/agent-notes-evidence/account-sync-resilience-red.txt`. That log also
shows `TestProtectedSurvivorOutranksRemoteTombstone`, the rejected
alternative above, which was withdrawn. `TestSyncRestoresMissingCopyFromAnotherDevice`
was added with the fix and has not been observed red.
