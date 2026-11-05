# Agent Note: Account sync is an LWW-element-set over StableID

Status: implemented

## Problem

Calling the feature "account sync" while treating a missing remote copy as a deletion (or keying accounts by token hash) makes two devices disagree about intent. A token refresh looks like a new account. A device that has not yet seen an account looks like it deleted it. A user who signs in again after deleting the same identity loses the new copy to an old tombstone if credential time does not advance. File-mtime as credTime lets a touch overwrite a newer remote token.

## Decision

Cross-device merge in `internal/accounts/backup.go` `MergeSync` / `syncIdentity` and `internal/accounts/sync.go` `ApplySync` is a last-write-wins element set:

- Identity is `provider` plus `credentialid` StableID (email / accountUuid / sub). Token bytes are not part of the key. Credentials with no stable claim stay local; `Syncable` strips them before publish. Portable file import still uses `MergeBackups` and may restore opaque documents.
- Absence is not a deletion. An account present locally, absent from the decrypted union, and without a tombstone is uploaded. Only an explicit delete record removes it.
- Tombstone time competes with credential `ModifiedAt`. A later add (in-app sign-in, capture, or file import) raises `ModifiedAt` past the tombstone so the account is kept.
- Name time and credential time compare independently.
- `ModifiedAt` advances when the credential document hash changes, not when the file is only touched. Quota rotation that rewrites tokens still advances it.
- Each device still publishes its own encrypted object; the payload is the merged live set plus still-valid tombstones. The sync server stays a ciphertext store. Sealed sync objects must fit `MaxSyncObjectBytes` (8 MiB, the server object cap).

## Alternatives considered

**Infer deletion from absence, or from "how recently the user deleted".** Absence has three opposite meanings (never seen, deleted, not yet synced). Recency does not encode intent.

**OR-Set add-tags / vector clocks.** Correct for concurrent add+delete without clocks, but this product has few devices and human-scale edits. LWW on Unix-nano credential time is enough; a later add already beats a tombstone.

**One plaintext account table on the server.** Breaks end-to-end encryption and does not remove the add-versus-delete ambiguity.

**Keying sync by document hash (the file-backup identity).** Token refresh forks a second account and old tombstones never match.

## Consequences

Accounts that never carry a stable identity will not appear on other devices; users must complete provider identity (email or equivalent) for those to sync. Clock skew of minutes can still pick the wrong token when two devices refresh at once; expiry-based tie-break is not added. Tombstones are kept (capped at 4096) rather than compacted.

## Verification

- `internal/accounts/sync_test.go::TestAccountSyncStableIdentityAndReadd`
- `internal/accounts/sync_test.go::TestAccountSyncMatchesStableIDAndIgnoresUntouchedMtime`
