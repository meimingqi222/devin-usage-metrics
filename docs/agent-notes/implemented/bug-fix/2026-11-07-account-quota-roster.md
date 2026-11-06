# Agent Note: Keep quota tabs aligned with the account manager

Status: implemented

## Problem

Adding, importing, renaming or removing a profile refreshed the manager but only scheduled account sync. With sync disabled, quota tabs kept the old roster. Quota discovery also omitted missing credential files. Matching a tab by email could fold separate organization profiles or give a saved copy another profile's active badge. An in-flight quota query could restore an older list after an edit.

## Decision

`internal/desktop/account_roster.go` builds quota targets from the reconciled profile snapshot. A live source joins only its active profile ID. Every other visible profile remains a tab, including unavailable credentials, which produce a local error instead of disappearing. Names and duplicate-name suffixes use the same snapshot as the manager.

Local edits and successful account sync immediately rebuild the roster on the UI thread and schedule a quota refresh. Roster revisions reject older worker snapshots; requests made during a refresh are coalesced into a follow-up. Cached quota can follow a rename but cannot cross profile IDs when a live source switches accounts. Saved preview sources never serve as live switch destinations.

## Alternatives considered

- Deduplicate tabs by email or shared tokens: those are not the UI identity. Different organizations can share an email, and a suspect saved copy must remain visible with its error. Stable-identity repair remains the account store's responsibility.
- Wait for the next periodic query or successful sync: account editing must update both views even with sync disabled or provider requests unavailable.
- Hide profiles without credential files: conceals the account and its recovery path while leaving the manager count different.

## Consequences

Unavailable profiles count toward the visible roster and cannot be switched from quota or tray controls. Roster changes are immediate; provider quota values still arrive asynchronously. Switching off account capture retains visibility of already-saved profiles.

## Tombstone poison (same incident, second defect)

Folding a duplicate record tombstoned the account's identity keys, not just
the record's. The surviving copy claims the same `syncIdentity` /
`backupIdentity`, so the tombstone kept the live sign-in from ever binding
to the survivor (`sync_tombstone` skip → `unmatched_live` → the account
rendered twice on the card) and would have `ApplySync`-removed the survivor
on every device at the next sync.

`suppressRecordBackupLocked` now retires only keys no surviving profile
claims; `unsuppressSurvivorsLocked` in `repairLocked` drops `Deleted` keys
colliding with a profile that still exists, healing manifests already
poisoned. A tombstoned identity still binds the live login to a surviving
record holding it (sync key, backup key or shared tokens) so the card can
name the account; only recreation stays suppressed.

## Verification

- `internal/desktop/account_roster_test.go::TestQuotaRosterKeepsEverySavedProfile`
- `internal/desktop/account_roster_test.go::TestAccountChangesUpdateQuotaRosterImmediately`
- `internal/desktop/account_roster_test.go::TestQuotaActiveBadgeUsesProfileID`
- `internal/accounts/accounts_test.go::TestFoldDoesNotTombstoneSurvivingAccount`
- `internal/accounts/accounts_test.go::TestRepairDropsTombstoneOnSurvivingAccount`

Proved: pre-fix tests failed with quota 1 versus manager 3, a missing newly added account, and a different profile receiving the active badge. Output is saved in `docs/agent-notes-evidence/account-roster-red.txt`; the corrected tests pass. Discovery/roster tests now run directly without provider requests, and additional tests cover stale worker completion, cache identity, saved-source switching and the four-account native rendering fixture. On the user's machine the tombstoned key (`antigravity/c04cfc2b2fb262ba`) skipped every reconcile until the fixes bound the live sign-in to `logkll2913` again.
